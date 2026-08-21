package api

import (
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
	writeJSON(w, http.StatusOK, struct {
		Email    string   `json:"email"`
		Subject  string   `json:"subject"`
		Roles    []string `json:"roles"`
		Defaults struct {
			KillCooldownSeconds int `json:"killCooldownSeconds"`
		} `json:"defaults"`
	}{
		Email:   id.Email,
		Subject: id.Subject,
		Roles:   roles,
		Defaults: struct {
			KillCooldownSeconds int `json:"killCooldownSeconds"`
		}{KillCooldownSeconds: int(a.opts.Config.KillCooldown.Seconds())},
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
		out = append(out, renderBroadcast(agg, cfg, state))
	}
	writeJSON(w, http.StatusOK, map[string]any{"broadcasts": out})
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
		writeJSON(w, http.StatusConflict, struct {
			Error errorBody `json:"error"`
			Ban   banJSON   `json:"ban"`
		}{
			Error: errorBody{Code: CodeDuplicateActive, Message: "this broadcast is already banned"},
			Ban:   renderBan(existing),
		})
		return
	} else if _, _, known := storeStatus(err); !known {
		a.fail(w, r, "look up an existing ban", err)
		return
	}

	expires := a.now().Add(cooldown)
	created, err := a.opts.Store.CreateBan(r.Context(), store.Ban{
		Target:            target,
		Reason:            reason,
		CreatedAt:         a.now(),
		CreatedBy:         id.Actor(),
		ExpiresAt:         &expires,
		SourceBroadcastID: normID,
	})
	if err != nil {
		a.fail(w, r, "create the kill ban", err)
		return
	}

	// The HMAC'd key, if the broadcast is live — the only broadcast handle a
	// webhook may carry (D8).
	key := a.broadcastKey(r, normID)
	projErr := a.project(r.Context(), created)

	a.record(r.Context(), store.Event{
		Type:         store.EventBroadcastKilled,
		OccurredAt:   a.now(),
		Actor:        id.Actor(),
		BroadcastKey: key,
		BroadcastID:  normID,
		Payload: killPayload(reason,
			store.Summarize(store.EventBroadcastKilled, target.Type, key, id.Actor()),
			int(cooldown.Seconds()), created.ID.String()),
	})
	a.afterMutation()

	// 202, not an error: the row is committed and durable, and the reconciler
	// is exactly the "other process" RFC 9110 §15.3.3 defines 202 for. The
	// body keeps kill's `{ban}` envelope so a client parses one shape either
	// way; `enforcement` is what tells it enforcement has not started.
	if projErr != nil {
		a.log.Error("projecting the kill ban to a Ban CR failed", "banId", created.ID, "err", projErr)
		writeJSON(w, http.StatusAccepted, map[string]any{"ban": renderPendingBan(created, DetailBanPending)})
		return
	}
	// Ban reasons are operator-private context: Debug only (docs/42 §5).
	a.log.Info("broadcast killed", "broadcastKey", key, "actor", id.Actor(), "cooldownSeconds", int(cooldown.Seconds()))
	a.log.Debug("kill reason recorded", "broadcastKey", key, "reason", reason)
	writeJSON(w, http.StatusCreated, map[string]any{"ban": renderBan(created)})
}

// broadcastKey resolves a raw ID to its HMAC'd key through relayscan. Empty
// when the broadcast is not live — an event about a broadcast that already
// ended simply carries no key rather than carrying the raw ID instead.
func (a *API) broadcastKey(r *http.Request, normID string) string {
	if a.opts.Fleet == nil {
		return ""
	}
	snap, err := a.opts.Fleet.Snapshot(r.Context())
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

func killPayload(reason, summary string, cooldownSeconds int, banID string) json.RawMessage {
	raw, err := json.Marshal(map[string]any{
		store.PayloadReason:  reason,
		store.PayloadSummary: summary,
		"cooldownSeconds":    cooldownSeconds,
		"banId":              banID,
	})
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
