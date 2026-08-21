package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Tuhis/gawk/gawk-admin/internal/store"
	"github.com/Tuhis/gawk/gawk-server/moderation"
)

// PublisherTargetValue is the literal an IP-ban request sends instead of an
// address, asking the server to resolve the live publisher's IP itself (§4.7).
//
// The portal never sends an address it merely displayed: resolving server-side
// means the ban lands on the address the relay actually observed, at the
// moment the operator confirmed, rather than on whatever a five-second-old
// table row said.
const PublisherTargetValue = "publisher"

type banTargetRequest struct {
	Type  moderation.TargetType `json:"type"`
	Value string                `json:"value"`
	// PrefixLength is the operator-confirmed prefix for an IP ban (§4.9): v4
	// defaults to /32, v6 to /64 because privacy-address rotation (RFC 8981)
	// makes a /128 near-useless. Validated against the family, never coerced.
	PrefixLength *int `json:"prefixLength,omitempty"`
}

type createBanRequest struct {
	Target banTargetRequest `json:"target"`
	// ExpiresAt is RFC3339, or null/absent for a permanent ban.
	ExpiresAt         *string `json:"expiresAt"`
	Reason            string  `json:"reason"`
	SourceBroadcastID string  `json:"sourceBroadcastId,omitempty"`
}

func (a *API) handleListBans(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	if state == "" {
		state = store.FilterActive
	}
	if state != store.FilterActive && state != store.FilterAll {
		writeError(w, http.StatusBadRequest, CodeBadRequest, `state must be "active" or "all"`)
		return
	}
	bans, err := a.opts.Store.ListBans(r.Context(), state)
	if err != nil {
		a.fail(w, r, "list bans", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"bans": renderBans(bans)})
}

func (a *API) handleCreateBan(w http.ResponseWriter, r *http.Request) {
	id, ok := a.caller(w, r)
	if !ok {
		return
	}
	var req createBanRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	reason, ok := requireReason(w, req.Reason)
	if !ok {
		return
	}

	expiresAt, err := parseExpiry(req.ExpiresAt)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, err.Error())
		return
	}

	source := strings.TrimSpace(req.SourceBroadcastID)
	if source != "" {
		if normalized, err := normalizeBroadcastID(source); err == nil {
			source = normalized
		}
	}

	target, err := a.resolveTarget(r, req.Target, source)
	if err != nil {
		// Every target failure is the caller's to fix — a malformed value, a
		// family/prefix mismatch, or a "publisher" that is no longer live.
		writeError(w, http.StatusBadRequest, CodeInvalidTarget, err.Error())
		return
	}

	// Duplicate active target answers 409 WITH the existing ban, so the
	// operator sees what is already in force (§4.7).
	if existing, err := a.opts.Store.ActiveBanForTarget(r.Context(), target); err == nil {
		writeJSON(w, http.StatusConflict, struct {
			Error errorBody `json:"error"`
			Ban   banJSON   `json:"ban"`
		}{
			Error: errorBody{Code: CodeDuplicateActive, Message: "an active ban already covers this target"},
			Ban:   renderBan(existing),
		})
		return
	} else if _, _, known := storeStatus(err); !known {
		a.fail(w, r, "look up an existing ban", err)
		return
	}

	created, err := a.opts.Store.CreateBan(r.Context(), store.Ban{
		Target:            target,
		Reason:            reason,
		CreatedAt:         a.now(),
		CreatedBy:         id.Actor(),
		ExpiresAt:         expiresAt,
		SourceBroadcastID: source,
	})
	if err != nil {
		a.fail(w, r, "create the ban", err)
		return
	}

	key := ""
	if source != "" {
		key = a.broadcastKey(r, source)
	}
	projErr := a.project(r.Context(), created)
	enforcement := enforcementState(projErr)

	a.record(r.Context(), store.Event{
		Type:         store.EventBanCreated,
		OccurredAt:   a.now(),
		Actor:        id.Actor(),
		BroadcastKey: key,
		BroadcastID:  source,
		Payload: banPayload(created, reason,
			store.SummarizeWithEnforcement(store.EventBanCreated, target.Type, key, id.Actor(), enforcement),
			enforcement),
	})
	a.afterMutation()

	// 202 Accepted: the record is durable, the reconciler completes it. See
	// handleKill for why this is a success and not a 5xx.
	if projErr != nil {
		a.log.Error("projecting the ban to a Ban CR failed", "banId", created.ID, "err", projErr)
		writeJSON(w, http.StatusAccepted, renderPendingBan(created, DetailBanPending))
		return
	}
	a.log.Info("ban created", "banId", created.ID, "targetType", created.Target.Type, "actor", id.Actor())
	a.log.Debug("ban reason recorded", "banId", created.ID, "reason", reason)
	writeJSON(w, http.StatusCreated, renderBan(created))
}

// handleDeleteBan is the unban: state = removed, CR deleted, ban.removed event
// (§4.7).
func (a *API) handleDeleteBan(w http.ResponseWriter, r *http.Request) {
	id, ok := a.caller(w, r)
	if !ok {
		return
	}
	banID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, CodeNotFound, "no such ban")
		return
	}
	removed, err := a.opts.Store.RemoveBan(r.Context(), banID, id.Actor())
	if err != nil {
		a.fail(w, r, "remove the ban", err)
		return
	}

	projErr := a.project(r.Context(), removed)
	enforcement := enforcementState(projErr)

	a.record(r.Context(), store.Event{
		Type:        store.EventBanRemoved,
		OccurredAt:  a.now(),
		Actor:       id.Actor(),
		BroadcastID: removed.SourceBroadcastID,
		Payload: banPayload(removed, removed.Reason,
			store.SummarizeWithEnforcement(store.EventBanRemoved, removed.Target.Type, "", id.Actor(), enforcement),
			enforcement),
	})
	a.afterMutation()

	// The unban's 202 answers WITH the removed ban rather than the clean
	// 204's empty body: this is the direction the operator is most likely to
	// misread — the row now says `removed` while the target is still banned —
	// so the response carries both the row and the sentence that says so.
	if projErr != nil {
		a.log.Error("deleting the ban's CR failed", "banId", removed.ID, "err", projErr)
		writeJSON(w, http.StatusAccepted, renderPendingBan(removed, DetailUnbanPending))
		return
	}
	a.log.Info("ban removed", "banId", removed.ID, "targetType", removed.Target.Type, "actor", id.Actor())
	w.WriteHeader(http.StatusNoContent)
}

var errPublisherUnresolved = errors.New("the live publisher's IP could not be resolved")

// resolveTarget turns a request target into the normalized ban target.
//
// The IP arm carries the whole §4.9 contract: the literal "publisher" resolves
// through relayscan, a bare address is widened to the operator-confirmed
// prefix, and a prefix length that does not belong to the address family is
// REJECTED rather than coerced — silently turning a /64 into a /32 (or worse,
// a /32 into a /64) would fire a differently-sized weapon than the operator
// confirmed.
func (a *API) resolveTarget(r *http.Request, req banTargetRequest, sourceBroadcastID string) (moderation.Target, error) {
	value := strings.TrimSpace(req.Value)
	switch req.Target() {
	case moderation.TargetBroadcastID:
		if req.PrefixLength != nil {
			return moderation.Target{}, errors.New("prefixLength is only meaningful for an IP target")
		}
		rec, err := moderation.Normalize(moderation.Record{
			Target: moderation.Target{Type: moderation.TargetBroadcastID, Value: value},
		})
		if err != nil {
			return moderation.Target{}, errors.New("not a valid broadcast ID")
		}
		return rec.Target, nil

	case moderation.TargetIP:
		if value == PublisherTargetValue {
			resolved, err := a.resolvePublisherIP(r, sourceBroadcastID)
			if err != nil {
				return moderation.Target{}, err
			}
			value = resolved
		}
		prefix, err := applyPrefixLength(value, req.PrefixLength)
		if err != nil {
			return moderation.Target{}, err
		}
		rec, err := moderation.Normalize(moderation.Record{
			Target: moderation.Target{Type: moderation.TargetIP, Value: prefix},
		})
		if err != nil {
			return moderation.Target{}, fmt.Errorf("not a valid IP target: %s", value)
		}
		return rec.Target, nil

	default:
		return moderation.Target{}, fmt.Errorf(`target.type must be %q or %q`,
			moderation.TargetBroadcastID, moderation.TargetIP)
	}
}

// Target normalizes the request's target type so an unknown value falls
// through to the default arm rather than being compared as a raw string.
func (t banTargetRequest) Target() moderation.TargetType {
	switch t.Type {
	case moderation.TargetBroadcastID, moderation.TargetIP:
		return t.Type
	default:
		return ""
	}
}

func (a *API) resolvePublisherIP(r *http.Request, sourceBroadcastID string) (string, error) {
	if sourceBroadcastID == "" {
		return "", fmt.Errorf(`%w: target value %q requires sourceBroadcastId`, errPublisherUnresolved, PublisherTargetValue)
	}
	if a.opts.Fleet == nil {
		return "", fmt.Errorf("%w: relay enumeration is not configured", errPublisherUnresolved)
	}
	snap, err := a.opts.Fleet.Snapshot(r.Context())
	if err != nil {
		return "", fmt.Errorf("%w: the relay fleet could not be enumerated", errPublisherUnresolved)
	}
	agg, ok := snap.Broadcast(sourceBroadcastID)
	if !ok {
		return "", fmt.Errorf("%w: that broadcast is not live on any relay pod", errPublisherUnresolved)
	}
	addr, ok := parsePeerAddr(agg.PublisherRemoteIP)
	if !ok {
		return "", fmt.Errorf("%w: the relay reported no usable publisher address", errPublisherUnresolved)
	}
	return addr.String(), nil
}

// applyPrefixLength widens an address to the confirmed prefix.
//
//   - A bare address plus a prefix length becomes that prefix. Absent, the
//     family default applies: /32 for v4, /64 for v6 (§4.9).
//   - A value that ALREADY carries a prefix must agree with any prefixLength
//     sent alongside it; a disagreement is an error, because one of the two is
//     not what the operator meant and guessing which is not the server's call.
func applyPrefixLength(value string, prefixLength *int) (string, error) {
	if strings.Contains(value, "/") {
		p, err := netip.ParsePrefix(value)
		if err != nil {
			return "", fmt.Errorf("not a valid CIDR: %s", value)
		}
		if prefixLength != nil && *prefixLength != p.Bits() {
			return "", fmt.Errorf("prefixLength %d disagrees with the /%d in %q", *prefixLength, p.Bits(), value)
		}
		return value, nil
	}
	addr, err := netip.ParseAddr(value)
	if err != nil {
		return "", fmt.Errorf("not a valid IP address: %s", value)
	}
	addr = moderation.CanonicalAddr(addr)
	bits := defaultPrefixLength(addr)
	if prefixLength != nil {
		bits = *prefixLength
		if bits < 1 || bits > addr.BitLen() {
			family := "IPv6"
			if addr.Is4() {
				family = "IPv4"
			}
			return "", fmt.Errorf("prefixLength %d is out of range for an %s address (want 1..%d)", bits, family, addr.BitLen())
		}
	}
	return netip.PrefixFrom(addr, bits).String(), nil
}

// defaultPrefixLength is the family default the portal also pre-selects: one
// address for v4, and /64 for v6 because a rotating privacy address makes a
// /128 ban expire on its own within hours.
func defaultPrefixLength(addr netip.Addr) int {
	if addr.Is4() {
		return 32
	}
	return 64
}

func parseExpiry(raw *string) (*time.Time, error) {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(*raw))
	if err != nil {
		return nil, fmt.Errorf("expiresAt must be RFC3339 or null: %w", err)
	}
	utc := t.UTC()
	return &utc, nil
}

func requireReason(w http.ResponseWriter, reason string) (string, bool) {
	trimmed := strings.TrimSpace(reason)
	if trimmed == "" {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "reason is required")
		return "", false
	}
	const maxReason = 512 // the CRD's own limit (docs/42 §4.2)
	if len(trimmed) > maxReason {
		writeError(w, http.StatusBadRequest, CodeBadRequest, fmt.Sprintf("reason must be at most %d characters", maxReason))
		return "", false
	}
	return trimmed, true
}

// banPayload is portal-visible event context. It may carry the target —
// including an IP CIDR — because the payload lives in Postgres and the portal;
// only the keys store declares webhook-safe — store.PayloadReason,
// store.PayloadSummary and store.PayloadEnforcement — ever reach a webhook (D8).
func banPayload(b store.Ban, reason, summary string, enforcement store.EnforcementState) json.RawMessage {
	payload := map[string]any{
		store.PayloadSummary: summary,
		"target":             b.Target,
		"banId":              b.ID.String(),
	}
	if reason != "" {
		payload[store.PayloadReason] = reason
	}
	// Only when there is something to say — see killPayload.
	if enforcement != store.EnforcementInSync {
		payload[store.PayloadEnforcement] = string(enforcement)
	}
	if b.ExpiresAt != nil {
		payload["expiresAt"] = b.ExpiresAt.UTC().Format(time.RFC3339)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return raw
}
