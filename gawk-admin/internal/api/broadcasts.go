package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/Tuhis/gawk/gawk-admin/internal/store"
	"github.com/Tuhis/gawk/gawk-server/moderation"
)

// handleMe is the SPA's authorization probe (§4.7).
//
// It also carries the server-side defaults the dialogs pre-fill. That lives
// here rather than on /auth/config because /auth/config is unauthenticated and
// pinned to {issuer, clientId, audience}; the kill cooldown is operational
// configuration and belongs behind the same token as everything else.
func (a *API) handleMe(w http.ResponseWriter, r *http.Request) {
	id, ok := a.caller(w, r)
	if !ok {
		return
	}
	roles := id.Roles
	if roles == nil {
		roles = []string{}
	}
	writeJSON(w, http.StatusOK, meJSON{
		Email:    id.Email,
		Subject:  id.Subject,
		Roles:    roles,
		Defaults: meDefaultsJSON{KillCooldownSeconds: int(a.opts.Config.KillCooldown.Seconds())},
		Features: meFeaturesJSON{Rooms: a.opts.Rooms != nil, Screening: a.opts.Config.Screening},
	})
}

// handleListBroadcasts is the fleet view: the relayscan aggregate joined with
// ban state and the deep links.
func (a *API) handleListBroadcasts(w http.ResponseWriter, r *http.Request) {
	if a.opts.Fleet == nil {
		writeError(w, http.StatusServiceUnavailable, CodeUnavailable, "relay enumeration is not configured")
		return
	}
	snap, err := a.opts.Fleet.Snapshot(r.Context())
	if err != nil {
		a.log.Error("relay enumeration failed", "err", err)
		writeError(w, http.StatusServiceUnavailable, CodeUnavailable, "the relay fleet could not be enumerated")
		return
	}

	// Ban state degrades rather than failing the view: an operator whose
	// database is down still needs to SEE the fleet. A null banState is
	// "unknown", which the UI renders as such — never as "not banned".
	var active []store.Ban
	banStateKnown := true
	if active, err = a.opts.Store.ListBans(r.Context(), store.FilterActive); err != nil {
		a.log.Warn("listing active bans for the broadcast view failed; rendering without ban state", "err", err)
		banStateKnown = false
	}

	now := a.now()
	flags, mutes := a.flagsByBroadcast(r.Context())
	cfg := linkConfig{app: a.opts.Config.AppBaseURL, telemetry: a.opts.Config.TelemetryBaseURL}
	out := make([]broadcastJSON, 0, len(snap.Broadcasts))
	for _, agg := range snap.Broadcasts {
		var state *banStateJSON
		if banStateKnown {
			state = &banStateJSON{}
			normID, _ := normalizeBroadcastID(agg.ID)
			if b := coveringBan(active, now, normID, agg.PublisherRemoteIP); b != nil {
				rendered := renderBan(*b)
				state.Banned = true
				state.Ban = &rendered
			}
		}
		row := renderBroadcast(agg, cfg, state)
		if f, ok := flags[agg.ID]; ok {
			rendered := a.renderFlag(f, mutes)
			row.ContentFlag = &rendered
		}
		out = append(out, row)
	}
	// The coverage counters ride along so partial coverage is VISIBLE: relayscan
	// treats a pod-level scrape failure as data rather than an error, and
	// without these an empty answer from an unreachable fleet would render as a
	// reassuring "nothing is broadcasting" (the same honesty rule banState's
	// null already enforces on the Postgres axis).
	writeJSON(w, http.StatusOK, broadcastsPageJSON{
		Broadcasts:   out,
		PodsResolved: snap.PodsResolved,
		PodsAnswered: snap.PodsAnswered,
	})
}

type killRequest struct {
	Reason          string `json:"reason"`
	CooldownSeconds *int   `json:"cooldownSeconds,omitempty"`
}

// handleKill is the actuator (§4.1).
//
// A plain kill IS a ban on the ID with a cooldown (D5): the broadcaster
// auto-reclaims with its resume token within seconds, so a kill without an ID
// ban resurrects before the portal refreshes. The cooldown means the operator
// is never racing auto-resume while deciding on a real ban.
func (a *API) handleKill(w http.ResponseWriter, r *http.Request) {
	id, ok := a.caller(w, r)
	if !ok {
		return
	}
	var req killRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	reason, ok := requireReason(w, req.Reason)
	if !ok {
		return
	}

	normID, err := normalizeBroadcastID(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidTarget, "not a valid broadcast ID")
		return
	}

	cooldown := a.opts.Config.KillCooldown
	if req.CooldownSeconds != nil {
		if *req.CooldownSeconds <= 0 {
			writeError(w, http.StatusBadRequest, CodeBadRequest, "cooldownSeconds must be positive")
			return
		}
		cooldown = time.Duration(*req.CooldownSeconds) * time.Second
	}

	target := moderation.Target{Type: moderation.TargetBroadcastID, Value: normID}
	// Idempotent-ish (§4.7): an existing active ID ban answers 409 WITH that
	// ban, so a double-clicked Kill shows the operator what is already in
	// force instead of a bare conflict.
	if existing, err := a.opts.Store.ActiveBanForTarget(r.Context(), target); err == nil {
		writeJSON(w, http.StatusConflict, banConflictJSON{
			Error: errorBody{Code: CodeDuplicateActive, Message: "this broadcast is already banned"},
			Ban:   renderBan(existing),
		})
		return
	} else if _, _, known := storeStatus(err); !known {
		a.fail(w, r, "look up an existing ban", err)
		return
	}

	k, err := a.killBroadcast(r.Context(), normID, reason, id.Actor(), cooldown)
	if err != nil {
		a.fail(w, r, "create the kill ban", err)
		return
	}
	// The operator's kill resolves the broadcast's open content flag as
	// killed (docs/71 D9): the page has been answered.
	a.resolveFlagsForKill(r.Context(), normID, id.Actor())
	a.afterMutation()

	// 202, not an error: the row is committed and durable, and the reconciler
	// is exactly the "other process" RFC 9110 §15.3.3 defines 202 for. The
	// body keeps kill's `{ban}` envelope so a client parses one shape either
	// way; `enforcement` is what tells it enforcement has not started.
	if k.projErr != nil {
		a.log.Error("projecting the kill ban to a Ban CR failed", "banId", k.ban.ID, "err", k.projErr)
		writeJSON(w, http.StatusAccepted, killResultJSON{Ban: renderPendingBan(k.ban, DetailBanPending)})
		return
	}
	// Ban reasons are operator-private context: Debug only (docs/42 §5).
	a.log.Info("broadcast killed", "broadcastKey", k.key, "actor", id.Actor(), "cooldownSeconds", int(cooldown.Seconds()))
	a.log.Debug("kill reason recorded", "broadcastKey", k.key, "reason", reason)
	writeJSON(w, http.StatusCreated, killResultJSON{Ban: renderBan(k.ban)})
}

// killOutcome is what one kill did: the ban row it wrote, the broadcast's
// HMAC'd key when it was live, and whether projecting the Ban CR failed.
type killOutcome struct {
	ban     store.Ban
	key     string
	projErr error
}

// killBroadcast is the one kill path (D5): a cooldown ban on the ID, its CR
// projected inline, and the broadcast.killed event graded on whether that
// landed. The operator's kill and R40's automatic ones (docs/71 D14, D15) all
// come through here, so an automatic kill is enforced, audited and paged
// exactly like one from the portal. The caller has already answered an
// existing active ban; a race that loses to one returns ErrDuplicateActive.
//
// It runs on the request context up to the row write; everything after is
// post-commit work (project and record detach on their own).
func (a *API) killBroadcast(ctx context.Context, normID, reason, actor string, cooldown time.Duration) (killOutcome, error) {
	target := moderation.Target{Type: moderation.TargetBroadcastID, Value: normID}
	a.expireLapsed(ctx, target)

	expires := a.now().Add(cooldown)
	created, err := a.opts.Store.CreateBan(ctx, store.Ban{
		Target:            target,
		Reason:            reason,
		CreatedAt:         a.now(),
		CreatedBy:         actor,
		ExpiresAt:         &expires,
		SourceBroadcastID: normID,
	})
	if err != nil {
		return killOutcome{}, err
	}
	return a.enforceKill(ctx, normID, reason, actor, cooldown, created), nil
}

// enforceKill is everything a kill does once its ban row exists: the Ban CR
// that makes the relays enforce it, and broadcast.killed. Shared by the
// operator's kill and the automatic one, whose ban is written inside its
// claim on the flag (Store.AutoKillContentFlag).
func (a *API) enforceKill(ctx context.Context, normID, reason, actor string, cooldown time.Duration, created store.Ban) killOutcome {
	// The HMAC'd key, if the broadcast is live — what the portal link is
	// keyed by. The sentence names the raw ID, as the delivery does (docs/52
	// D9).
	key := a.broadcastKey(ctx, normID)
	projErr := a.project(ctx, created)
	enforcement := enforcementState(projErr)

	a.record(ctx, store.Event{
		Type:         store.EventBroadcastKilled,
		OccurredAt:   a.now(),
		Actor:        actor,
		BroadcastKey: key,
		BroadcastID:  normID,
		Payload: killPayload(reason,
			store.SummarizeWithEnforcement(store.EventBroadcastKilled, created.Target.Type, normID, actor, enforcement),
			int(cooldown.Seconds()), created.ID.String(), enforcement),
	})
	return killOutcome{ban: created, key: key, projErr: projErr}
}

// broadcastKey resolves a raw ID to its HMAC'd key through relayscan. Empty
// when the broadcast is not live — an event about a broadcast that already
// ended simply carries no key rather than carrying the raw ID instead.
func (a *API) broadcastKey(ctx context.Context, normID string) string {
	if a.opts.Fleet == nil {
		return ""
	}
	snap, err := a.opts.Fleet.Snapshot(ctx)
	if err != nil {
		return ""
	}
	if agg, ok := snap.Broadcast(normID); ok {
		return agg.Key
	}
	return ""
}

// afterMutation drops the fleet cache and asks for an immediate reconcile, so
// the operator's next refresh reflects what they just did.
func (a *API) afterMutation() {
	if a.opts.Fleet != nil {
		a.opts.Fleet.Invalidate()
	}
	a.kick()
}

func killPayload(reason, summary string, cooldownSeconds int, banID string, enforcement store.EnforcementState) json.RawMessage {
	payload := map[string]any{
		store.PayloadReason:  reason,
		store.PayloadSummary: summary,
		"cooldownSeconds":    cooldownSeconds,
		"banId":              banID,
	}
	// Written only when there is something to say, so an in-sync event's
	// payload — and the webhook body derived from it — stays exactly what it
	// has always been (§4.10: absence means record and enforcement agree).
	if enforcement != store.EnforcementInSync {
		payload[store.PayloadEnforcement] = string(enforcement)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return raw
}

// normalizeBroadcastID runs an ID through the shared normalization so the API,
// the store, the CR name and the relay all mean the same thing by it.
func normalizeBroadcastID(id string) (string, error) {
	rec, err := moderation.Normalize(moderation.Record{
		Target: moderation.Target{Type: moderation.TargetBroadcastID, Value: id},
	})
	if err != nil {
		return "", err
	}
	return rec.Target.Value, nil
}
