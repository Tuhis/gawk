package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Tuhis/gawk/gawk-admin/internal/kube"
	"github.com/Tuhis/gawk/gawk-admin/internal/relayscan"
	"github.com/Tuhis/gawk/gawk-admin/internal/store"
	"github.com/Tuhis/gawk/gawk-server/events"
	"github.com/Tuhis/gawk/gawk-server/moderation"
	"github.com/Tuhis/gawk/gawk-server/rooms"
	"github.com/Tuhis/gawk/gawk-server/screening"
)

// R40 automatic content flagging (docs/71): the three flagger routes the
// sampler calls, the operator's flag list and actions, the calibration replay,
// and the leader's sweep that re-pages, ends and — when switched on — kills.
//
// Two rules shape this file.
//
//   - **A flag must reach a human** (docs/71 §1, D12). With no enabled
//     webhook that would deliver content_flag.raised, flag mode refuses to
//     flag (503 no_flag_route) and the sampler pauses; a flag nobody
//     acknowledges re-pages while its signal is live AND while it is unknown,
//     because a silent sampler is not evidence of a clean broadcast.
//   - **gawk-admin is the only actuator** (D14). The sampler holds `flagger`,
//     which is flag-only; every kill — the operator's, the auto-kill tier's,
//     the deadline's — goes through killBroadcast, R39's one kill path.

// shardMissingAfter is D22's 30 s: a shard index under the expected count that
// has not polled the target list for this long is missing. Two missed polls
// at the 5 s default refresh would be noise; six is a sampler that is gone.
const shardMissingAfter = 30 * time.Second

// Kicker-shaped notifier: the dispatcher sends a recorded event's deliveries
// at once when it is the leader, otherwise on its next poll.
func (a *API) kickDeliveries() {
	if k, ok := a.opts.Recorder.(Kicker); ok {
		k.Kick()
	}
}

// ---------------------------------------------------------------- wire shapes

// contentFlagJSON is one flag as the portal sees it (docs/71 §9.4). The raw
// broadcast ID is here behind the operator role, like every broadcast row.
type contentFlagJSON struct {
	ID    int64  `json:"id"`
	Mode  string `json:"mode"`
	State string `json:"state"`
	// Resolution and ResolutionReason are absent while the flag is unresolved.
	Resolution       string `json:"resolution,omitempty"`
	ResolutionReason string `json:"resolutionReason,omitempty"`
	BroadcastID      string `json:"broadcastId"`
	BroadcastKey     string `json:"broadcastKey"`
	PublisherEpoch   string `json:"publisherEpoch"`
	SamplerID        string `json:"samplerId"`
	RaisedAt         string `json:"raisedAt"`
	UpdatedAt        string `json:"updatedAt"`
	LastPostAt       string `json:"lastPostAt"`
	AcknowledgedAt   string `json:"acknowledgedAt,omitempty"`
	AcknowledgedBy   string `json:"acknowledgedBy,omitempty"`
	ResolvedAt       string `json:"resolvedAt,omitempty"`
	ResolvedBy       string `json:"resolvedBy,omitempty"`
	// Signal is live, unknown (no report for longer than stale-after) or
	// cleared.
	Signal      string         `json:"signal"`
	Window      flagWindowJSON `json:"window"`
	Scores      flagScoresJSON `json:"scores"`
	Labels      []string       `json:"labels"`
	LabelCounts map[string]int `json:"labelCounts,omitempty"`
	PeakScore   float64        `json:"peakScore"`
	Reminders   int            `json:"reminders"`
	MutedUntil  string         `json:"mutedUntil,omitempty"`
	Links       *watchLinkJSON `json:"links,omitempty"`
}

type flagWindowJSON struct {
	Start          string `json:"start"`
	End            string `json:"end"`
	Samples        int    `json:"samples"`
	FlaggedSamples int    `json:"flaggedSamples"`
}

type flagScoresJSON struct {
	Max  float64 `json:"max"`
	Mean float64 `json:"mean"`
}

// watchLinkJSON carries a flag's watch link, built from -app-base-url exactly
// like a broadcast row's: it is how an operator judges a flag (OD4), and an
// observed flag is worth listing only because it can be watched while the
// trigger is on screen (OD14).
type watchLinkJSON struct {
	Watch string `json:"watch"`
}

// screeningStatusJSON is the screening plane's health, read by the portal's
// two banners: no pager (D12) and missing sampler shards (D22).
type screeningStatusJSON struct {
	FlagRoute      bool   `json:"flagRoute"`
	ExpectedShards int    `json:"expectedShards"`
	MissingShards  []int  `json:"missingShards"`
	LastPollAt     string `json:"lastPollAt,omitempty"`
}

type contentFlagsPageJSON struct {
	ContentFlags []contentFlagJSON   `json:"contentFlags"`
	Screening    screeningStatusJSON `json:"screening"`
}

// emptyRequest is the body of acknowledge and dismiss: `{}` or nothing. A
// declared type so a misspelled field is refused like everywhere else.
type emptyRequest struct{}

// roomPatchRequest is PATCH /api/v1/rooms/{name}: today only the screening
// field (docs/71 D11). A pointer, so an absent field is a 400 rather than a
// silent un-exemption.
type roomPatchRequest struct {
	Screening *string `json:"screening"`
}

func (a *API) renderFlag(f store.ContentFlag, mutes map[string]time.Time) contentFlagJSON {
	b := f.Body
	out := contentFlagJSON{
		ID: f.ID, Mode: f.Mode, State: f.State,
		Resolution: f.Resolution, ResolutionReason: f.ResolutionReason,
		BroadcastID: f.BroadcastID, BroadcastKey: f.BroadcastKey, PublisherEpoch: f.PublisherEpoch,
		SamplerID: f.SamplerID,
		RaisedAt:  stamp(f.RaisedAt), UpdatedAt: stamp(f.UpdatedAt), LastPostAt: stamp(f.LastPostAt),
		AcknowledgedBy: f.AcknowledgedBy, ResolvedBy: f.ResolvedBy,
		Signal: f.Signal(a.now(), a.opts.Config.ContentFlagStaleAfter),
		Window: flagWindowJSON{Start: stamp(b.Window.Start), End: stamp(b.Window.End),
			Samples: b.Window.Samples, FlaggedSamples: b.Window.FlaggedSamples},
		Scores:      flagScoresJSON{Max: b.Scores.Max, Mean: b.Scores.Mean},
		Labels:      b.Labels,
		LabelCounts: b.LabelCounts,
		PeakScore:   f.PeakScore,
		Reminders:   f.Reminders,
	}
	if out.Labels == nil {
		out.Labels = []string{}
	}
	if f.AcknowledgedAt != nil {
		out.AcknowledgedAt = stamp(*f.AcknowledgedAt)
	}
	if f.ResolvedAt != nil {
		out.ResolvedAt = stamp(*f.ResolvedAt)
	}
	if until, ok := mutes[f.BroadcastID]; ok {
		out.MutedUntil = stamp(until)
	}
	if base := a.opts.Config.AppBaseURL; base != "" {
		out.Links = &watchLinkJSON{Watch: base + "/#/view/" + f.BroadcastID}
	}
	return out
}

// flagsByBroadcast is the unresolved flag-mode flag of every broadcast that has
// one, for the broadcast view's pinned rows (§9.4), with the mutes their views
// render. Empty with screening off; a failure degrades to no pins rather than
// failing the fleet view, which an operator needs most when something is wrong.
func (a *API) flagsByBroadcast(ctx context.Context) (map[string]store.ContentFlag, map[string]time.Time) {
	out := map[string]store.ContentFlag{}
	if !a.opts.Config.Screening {
		return out, nil
	}
	flags, err := a.opts.Store.UnresolvedContentFlags(ctx, screening.ModeFlag)
	if err != nil {
		a.log.Warn("listing content flags for the broadcast view failed; rendering without them", "err", err)
		return out, nil
	}
	for _, f := range flags {
		out[f.BroadcastID] = f
	}
	mutes, err := a.opts.Store.ActiveMutes(ctx, a.now())
	if err != nil {
		a.log.Warn("reading mutes for the broadcast view failed", "err", err)
	}
	return out, mutes
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// ------------------------------------------------------------- flagger routes

// handlePostContentFlag is the sampler's one write (docs/71 D9, D10, D12,
// D19, D2's backstop, D14's auto-kill).
func (a *API) handlePostContentFlag(w http.ResponseWriter, r *http.Request) {
	id, ok := a.caller(w, r)
	if !ok {
		return
	}
	var body screening.ContentFlag
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if err := body.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, err.Error())
		return
	}
	body.BroadcastID = strings.ToUpper(body.BroadcastID)
	ctx := r.Context()
	cfg := a.opts.Config.ConfigWebhooks()

	// D12: in flag mode, no pager means no flag. Observe mode pages no one,
	// so it is exempt.
	if body.Mode == screening.ModeFlag {
		route, err := a.opts.Store.FlagRoute(ctx, cfg)
		if err != nil {
			a.fail(w, r, "check the flag route", err)
			return
		}
		if !route {
			writeError(w, http.StatusServiceUnavailable, CodeNoFlagRoute,
				"no enabled webhook would deliver content_flag.raised: flagging is paused until one does (docs/71 D12)")
			return
		}
	}

	// D2's backstop: a tripped window for a session that is no longer the
	// broadcast's live one raises nothing. Its cached keyframe was a frozen
	// frame nobody is broadcasting.
	live, key := a.sessionLive(ctx, body.BroadcastID, body.PublisherEpoch)
	if body.State == screening.StateTripped && !live {
		writeJSON(w, http.StatusAccepted, screening.FlagResponse{Status: screening.FlagStatusEnded})
		return
	}

	res, err := a.opts.Store.PostContentFlag(ctx, store.FlagPost{Body: body, BroadcastKey: key, Actor: id.Actor(), Now: a.now()}, cfg)
	if err != nil {
		a.fail(w, r, "record the content flag", err)
		return
	}
	switch res.Status {
	case screening.FlagStatusRaised:
		// The broadcast key, never the raw ID, above Debug (docs/42 D8).
		a.log.Info("content flag raised", "mode", body.Mode, "flagId", res.Flag.ID, "broadcastKey", key,
			"samplerId", body.SamplerID, "peak", body.Scores.Max)
	case screening.FlagStatusResolved:
		a.log.Info("content flag cleared", "mode", body.Mode, "flagId", res.Flag.ID, "broadcastKey", key)
	}
	a.kickDeliveries()

	// D14: the auto-kill tier, over the POST's own `recent` and nothing
	// else. Off by default; never on an observed flag or a muted broadcast
	// (a muted POST never reaches here).
	if body.Mode == screening.ModeFlag && body.State == screening.StateTripped &&
		(res.Status == screening.FlagStatusRaised || res.Status == screening.FlagStatusUpdated) &&
		a.opts.Config.ContentFlagAutoKill.Match(body.Recent) {
		a.autoKill(ctx, res.Flag, store.FlagReasonPolicy)
	}

	status := http.StatusOK
	switch res.Status {
	case screening.FlagStatusRaised:
		status = http.StatusCreated
	case screening.FlagStatusMuted, screening.FlagStatusIgnored, screening.FlagStatusEnded:
		status = http.StatusAccepted
	}
	writeJSON(w, status, screening.FlagResponse{Status: res.Status, FlagID: res.Flag.ID})
}

// sessionLive reports whether epoch is the broadcast's live publisher session
// as the fleet scan sees it, and the broadcast's key. It fails OPEN — live —
// whenever the scan cannot say: a scan error, a pod that did not answer, no
// pod at all, a row that is not the origin's, a relay that predates the
// epoch. Only the origin knows the publisher session: an edge's row carries
// the edge's own epoch, which never matches (PR #500 review). A page delayed
// by a wrong "ended" is the failure this surface exists to prevent; a wrong
// "live" costs at most one flag that the session-end sweep resolves.
func (a *API) sessionLive(ctx context.Context, broadcastID, epoch string) (bool, string) {
	if a.opts.Fleet == nil {
		return true, ""
	}
	snap, err := a.opts.Fleet.Snapshot(ctx)
	if err != nil {
		a.log.Warn("relay enumeration failed while checking a content flag's session; treating it as live", "err", err)
		return true, ""
	}
	agg, found := snap.Broadcast(broadcastID)
	complete := snap.PodsResolved > 0 && snap.PodsAnswered == snap.PodsResolved
	switch {
	case !complete:
		return true, agg.Key
	case !found:
		return false, ""
	case agg.OriginAddr == "":
		return true, agg.Key
	}
	return sessionMatches(agg, epoch), agg.Key
}

// sessionMatches is the one rule for "is this flag's session still the live
// one?": the publisher is active and, when the relay reports an epoch at all,
// it is this one.
func sessionMatches(agg relayscan.Aggregate, epoch string) bool {
	if !agg.PublisherActive {
		return false
	}
	return agg.PublisherEpoch == "" || agg.PublisherEpoch == epoch
}

// handleScreeningTargets serves one shard's screening targets (docs/71 D2,
// D22) and records the poll.
func (a *API) handleScreeningTargets(w http.ResponseWriter, r *http.Request) {
	id, ok := a.caller(w, r)
	if !ok {
		return
	}
	shard, err1 := strconv.Atoi(r.URL.Query().Get("shard"))
	of, err2 := strconv.Atoi(r.URL.Query().Get("of"))
	if err1 != nil || err2 != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "shard and of are required integers")
		return
	}
	if err := screening.ValidateShard(shard, of); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, err.Error())
		return
	}
	ctx := r.Context()
	now := a.now()
	// Recorded before anything can fail: a sampler that polls is alive, and
	// the missing-shard banner must not light because the fleet scan did.
	if err := a.opts.Store.RecordShardPoll(ctx, shard, of, id.Actor(), now); err != nil {
		a.fail(w, r, "record the shard poll", err)
		return
	}
	if a.opts.Fleet == nil {
		writeError(w, http.StatusServiceUnavailable, CodeUnavailable, "relay enumeration is not configured")
		return
	}
	snap, err := a.opts.Fleet.Snapshot(ctx)
	if err != nil {
		a.log.Error("relay enumeration failed", "err", err)
		writeError(w, http.StatusServiceUnavailable, CodeUnavailable, "the relay fleet could not be enumerated")
		return
	}
	mutes, err := a.opts.Store.ActiveMutes(ctx, now)
	if err != nil {
		a.fail(w, r, "read the mutes", err)
		return
	}
	route, err := a.opts.Store.FlagRoute(ctx, a.opts.Config.ConfigWebhooks())
	if err != nil {
		a.fail(w, r, "check the flag route", err)
		return
	}
	exempt := a.exemptBroadcasts(ctx, snap)

	out := screening.TargetsResponse{Schema: screening.SchemaTargets, Shard: shard, Of: of, FlagRoute: route,
		Targets: []screening.Target{}}
	for _, agg := range snap.Broadcasts {
		// Origins only, publisher active only (D2): an edge is a copy of a
		// broadcast another pod screens, and an away broadcast's cached
		// keyframe is a frozen frame nobody is broadcasting. A relay that
		// reports no epoch predates R40 and has no keyframe route.
		if agg.OriginAddr == "" || !agg.PublisherActive || agg.PublisherEpoch == "" || agg.Key == "" {
			continue
		}
		if screening.Shard(agg.Key, of) != shard {
			continue
		}
		_, muted := mutes[agg.ID]
		out.Targets = append(out.Targets, screening.Target{
			BroadcastID: agg.ID, BroadcastKey: agg.Key, PublisherEpoch: agg.PublisherEpoch,
			Relay: "http://" + agg.OriginAddr, Muted: muted, Exempt: exempt[agg.ID],
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// exemptBroadcasts is every broadcast attached to an exempt room (docs/71
// D11): the Room CRs say which rooms are exempt — through
// rooms.Room.ScreeningExempt, which fails closed on a room that may not be —
// and their attachments, merged with each live room's own roster from the
// scan, say which broadcasts. Rooms off, or a CR list that fails, exempts
// nothing: an error here must fail toward screening.
func (a *API) exemptBroadcasts(ctx context.Context, snap relayscan.Snapshot) map[string]bool {
	out := map[string]bool{}
	if a.opts.Rooms == nil {
		return out
	}
	list, err := a.opts.Rooms.List(ctx)
	if err != nil {
		a.log.Warn("listing rooms for the screening exemptions failed; screening every broadcast", "err", err)
		return out
	}
	exemptRooms := map[string]bool{}
	for _, obj := range list {
		if obj.Err != nil || !obj.Room.ScreeningExempt() {
			continue
		}
		exemptRooms[obj.Name] = true
		for _, att := range obj.Room.Status.Attachments {
			out[strings.ToUpper(att.BroadcastID)] = true
		}
	}
	for _, room := range snap.Rooms {
		if !exemptRooms[room.Code] {
			continue
		}
		for _, att := range room.Attachments {
			out[strings.ToUpper(att.BroadcastID)] = true
		}
	}
	return out
}

// handleScreeningSamples stores one sampler batch (docs/71 D20).
func (a *API) handleScreeningSamples(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.caller(w, r); !ok {
		return
	}
	var batch screening.SampleBatch
	if err := decodeJSON(w, r, &batch); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if err := batch.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, err.Error())
		return
	}
	for i := range batch.Sessions {
		batch.Sessions[i].BroadcastID = strings.ToUpper(batch.Sessions[i].BroadcastID)
	}
	stored, err := a.opts.Store.IngestSamples(r.Context(), batch)
	if err != nil {
		a.fail(w, r, "store the samples", err)
		return
	}
	writeJSON(w, http.StatusOK, screening.SamplesResponse{Stored: stored})
}

// ------------------------------------------------------------ operator routes

// handleScreeningReplay answers "what would this setting have done?" over the
// stored samples (docs/71 D21).
func (a *API) handleScreeningReplay(w http.ResponseWriter, r *http.Request) {
	var req screening.ReplayRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, err.Error())
		return
	}
	out, err := a.opts.Store.Replay(r.Context(), req)
	if errors.Is(err, store.ErrReplayBelowFloor) || errors.Is(err, store.ErrReplayUnknownLabel) {
		writeError(w, http.StatusBadRequest, CodeBadRequest, err.Error())
		return
	}
	if err != nil {
		a.fail(w, r, "replay the samples", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleListContentFlags lists flags with the screening plane's health.
func (a *API) handleListContentFlags(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := q.Get("state")
	switch state {
	case "", store.FlagFilterUnresolved, store.FlagFilterResolved, store.FlagFilterAll:
	default:
		writeError(w, http.StatusBadRequest, CodeBadRequest, "state must be unresolved, resolved or all")
		return
	}
	mode := q.Get("mode")
	switch mode {
	case "", screening.ModeFlag, screening.ModeObserve, store.FlagFilterAll:
	default:
		writeError(w, http.StatusBadRequest, CodeBadRequest, "mode must be flag, observe or all")
		return
	}
	limit := 0
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, CodeBadRequest, "limit must be a positive integer")
			return
		}
		limit = n
	}
	ctx := r.Context()
	flags, err := a.opts.Store.ListContentFlags(ctx, store.FlagQuery{State: state, Mode: mode, Limit: limit})
	if err != nil {
		a.fail(w, r, "list content flags", err)
		return
	}
	mutes, err := a.opts.Store.ActiveMutes(ctx, a.now())
	if err != nil {
		a.fail(w, r, "read the mutes", err)
		return
	}
	status, err := a.screeningStatus(ctx)
	if err != nil {
		a.fail(w, r, "read the screening status", err)
		return
	}
	out := contentFlagsPageJSON{ContentFlags: make([]contentFlagJSON, 0, len(flags)), Screening: status}
	for _, f := range flags {
		out.ContentFlags = append(out.ContentFlags, a.renderFlag(f, mutes))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) screeningStatus(ctx context.Context) (screeningStatusJSON, error) {
	route, err := a.opts.Store.FlagRoute(ctx, a.opts.Config.ConfigWebhooks())
	if err != nil {
		return screeningStatusJSON{}, err
	}
	shards, err := a.opts.Store.ShardStatus(ctx, a.now(), shardMissingAfter)
	if err != nil {
		return screeningStatusJSON{}, err
	}
	out := screeningStatusJSON{FlagRoute: route, ExpectedShards: shards.Expected, MissingShards: shards.Missing}
	if shards.LastPoll != nil {
		out.LastPollAt = stamp(*shards.LastPoll)
	}
	return out, nil
}

// ShardsMissing is the number the gawk_admin_screening_shards_missing gauge
// exports (D22). Every replica reads it from Postgres, so any one of them
// answers for the fleet.
func (a *API) ShardsMissing(ctx context.Context) (int, error) {
	s, err := a.opts.Store.ShardStatus(ctx, a.now(), shardMissingAfter)
	if err != nil {
		return 0, err
	}
	return len(s.Missing), nil
}

func flagIDParam(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusNotFound, CodeNotFound, "no such content flag")
		return 0, false
	}
	return id, true
}

// decodeOptional reads `{}` or an empty body into v.
func decodeOptional(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := decodeJSON(w, r, v); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// handleAcknowledgeContentFlag is "I'm looking" (D9): reminders and the
// deadline stop, nothing resolves.
func (a *API) handleAcknowledgeContentFlag(w http.ResponseWriter, r *http.Request) {
	id, ok := a.caller(w, r)
	if !ok {
		return
	}
	flagID, ok := flagIDParam(w, r)
	if !ok || !decodeOptional(w, r, &emptyRequest{}) {
		return
	}
	f, err := a.opts.Store.AcknowledgeContentFlag(r.Context(), flagID, id.Actor(), a.now())
	if a.flagStateError(w, r, "acknowledge the content flag", err) {
		return
	}
	a.log.Info("content flag acknowledged", "flagId", f.ID, "broadcastKey", f.BroadcastKey, "actor", id.Actor())
	a.writeFlag(w, r, f)
}

// handleDismissContentFlag resolves a flag as dismissed and mutes the
// broadcast for -content-flag-mute (D10).
func (a *API) handleDismissContentFlag(w http.ResponseWriter, r *http.Request) {
	id, ok := a.caller(w, r)
	if !ok {
		return
	}
	flagID, ok := flagIDParam(w, r)
	if !ok || !decodeOptional(w, r, &emptyRequest{}) {
		return
	}
	now := a.now()
	f, err := a.opts.Store.DismissContentFlag(r.Context(), flagID, id.Actor(), now, now.Add(a.opts.Config.ContentFlagMute),
		a.opts.Config.ConfigWebhooks())
	if a.flagStateError(w, r, "dismiss the content flag", err) {
		return
	}
	a.kickDeliveries()
	a.log.Info("content flag dismissed", "flagId", f.ID, "broadcastKey", f.BroadcastKey, "actor", id.Actor(),
		"mute", a.opts.Config.ContentFlagMute.String())
	a.writeFlag(w, r, f)
}

func (a *API) flagStateError(w http.ResponseWriter, r *http.Request, what string, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, CodeNotFound, "no such content flag")
	case errors.Is(err, store.ErrFlagNotOpen):
		writeError(w, http.StatusConflict, CodeContentFlagState, "only an open content flag can be acknowledged")
	case errors.Is(err, store.ErrFlagResolved):
		writeError(w, http.StatusConflict, CodeContentFlagState, "the content flag is already resolved")
	case errors.Is(err, store.ErrFlagObserved):
		writeError(w, http.StatusConflict, CodeContentFlagState,
			"an observed flag pages nobody and cannot be acknowledged or dismissed (docs/71 D19)")
	default:
		a.fail(w, r, what, err)
	}
	return true
}

func (a *API) writeFlag(w http.ResponseWriter, r *http.Request, f store.ContentFlag) {
	mutes, err := a.opts.Store.ActiveMutes(r.Context(), a.now())
	if err != nil {
		a.log.Warn("reading the mutes for a flag response failed", "err", err)
	}
	writeJSON(w, http.StatusOK, a.renderFlag(f, mutes))
}

// handlePatchRoom sets a room's screening (docs/71 D11): `exempt` only on a
// static room with an attach secret, audited as room.screening_changed.
func (a *API) handlePatchRoom(w http.ResponseWriter, r *http.Request) {
	id, ok := a.caller(w, r)
	if !ok {
		return
	}
	if a.opts.Rooms == nil {
		writeError(w, http.StatusNotFound, CodeNotFound, "no such room")
		return
	}
	name, ok := roomName(w, r)
	if !ok {
		return
	}
	var req roomPatchRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if req.Screening == nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, `screening is required: "exempt" or ""`)
		return
	}
	value := *req.Screening
	if value != "" && value != rooms.ScreeningExempt {
		writeError(w, http.StatusBadRequest, CodeBadRequest, `screening must be "exempt" or ""`)
		return
	}
	// The rule runs inside the write, on the object it changes, so a room
	// that lost its secret in between is refused rather than exempted.
	check := func(obj kube.RoomObject) error {
		spec := obj.Room.Spec
		spec.Screening = value
		if obj.Err != nil {
			return rooms.ErrScreeningNotAllowed
		}
		return rooms.ValidateScreening(spec)
	}
	before, after, err := a.opts.Rooms.SetScreening(r.Context(), name, value, check)
	if errors.Is(err, rooms.ErrScreeningNotAllowed) {
		writeError(w, http.StatusBadRequest, CodeScreeningNotAllowed,
			"only a static room with an attach secret can be exempt from screening (docs/71 D11)")
		return
	}
	if err != nil {
		a.failRoom(w, r, "set the room's screening", err)
		return
	}
	if before.Room.Spec.Screening != after.Room.Spec.Screening {
		a.recordRoomScreening(r.Context(), id.Actor(), after)
	}
	a.log.Info("room screening set", "actor", id.Actor(), "roomKey", after.Room.Status.Key,
		"exempt", after.Room.Spec.Screening == rooms.ScreeningExempt)
	live := a.liveRooms(r.Context())
	for code := range live {
		if code != name {
			delete(live, code)
		}
	}
	rows := a.mergeRooms([]kube.RoomObject{after}, live, false)
	if len(rows) == 0 {
		writeError(w, http.StatusNotFound, CodeNotFound, "no such room")
		return
	}
	writeJSON(w, http.StatusOK, rows[0])
}

// recordRoomScreening writes room.screening_changed (D11, D16): the room's
// identity, as the other room events carry it, and the new value.
func (a *API) recordRoomScreening(ctx context.Context, actor string, obj kube.RoomObject) {
	kind := obj.Room.Spec.Kind
	display := rooms.DisplayCode(&obj.Room)
	exempt := obj.Room.Spec.Screening == rooms.ScreeningExempt
	value := events.RoomScreeningScreened
	if exempt {
		value = events.RoomScreeningExempt
	}
	payload := map[string]any{
		store.PayloadSummary:     store.SummarizeRoomScreening(kind, display, actor, exempt),
		store.PayloadRoom:        obj.Name,
		store.PayloadRoomKind:    kind,
		store.PayloadDisplayCode: display,
		store.PayloadScreening:   value,
	}
	if key := obj.Room.Status.Key; key != "" {
		payload[store.PayloadRoomKey] = key
	}
	a.record(ctx, store.Event{
		Type:       store.EventRoomScreeningChanged,
		OccurredAt: a.now(),
		Actor:      actor,
		Payload:    marshalPayload(payload),
	})
}

// ------------------------------------------------------------ kills and sweep

// resolveFlagsForKill resolves the broadcast's unresolved flag-mode flag as
// killed after an operator's kill (D9). Post-commit work: the kill already
// happened, so neither a hung-up browser nor a failure here may undo it.
func (a *API) resolveFlagsForKill(ctx context.Context, broadcastID, actor string) {
	if !a.opts.Config.Screening {
		return
	}
	ctx, cancel := postCommit(ctx)
	defer cancel()
	if _, err := a.opts.Store.ResolveFlagsForKill(ctx, broadcastID, actor, a.now(), a.opts.Config.ConfigWebhooks()); err != nil {
		a.log.Warn("resolving content flags after a kill failed", "err", err)
		return
	}
	a.kickDeliveries()
}

// autoKill is D14's and D15's actuator: R39's kill path with a cooldown ban of
// -kill-cooldown, actor system:content-flag, and the flag resolved
// auto_killed — the page still goes out, as broadcast.killed and
// content_flag.resolved. A broadcast an operator already banned is not banned
// twice; the flag still resolves, because the broadcast is gone either way.
//
// f is what the caller read, which an operator may have overtaken since: the
// ban and the resolution are one claim on the flag as it is now
// (Store.AutoKillContentFlag), so an acknowledgement or a dismissal that got
// there first means no kill. The deadline (reason deadline) needs the flag
// still open; D14's policy needs it unresolved.
func (a *API) autoKill(ctx context.Context, f store.ContentFlag, reason string) {
	ctx, cancel := postCommit(ctx)
	defer cancel()
	target := moderation.Target{Type: moderation.TargetBroadcastID, Value: f.BroadcastID}
	a.expireLapsed(ctx, target)
	cooldown := a.opts.Config.KillCooldown
	why := "automatic content-flag kill (" + reason + ")"
	expires := a.now().Add(cooldown)
	resolved, banned, ok, err := a.opts.Store.AutoKillContentFlag(ctx, f.ID, reason == store.FlagReasonDeadline, store.Ban{
		Target:            target,
		Reason:            why,
		CreatedAt:         a.now(),
		CreatedBy:         store.ActorContentFlag,
		ExpiresAt:         &expires,
		SourceBroadcastID: f.BroadcastID,
	}, reason, a.now(), a.opts.Config.ConfigWebhooks())
	if err != nil {
		a.log.Error("automatic content-flag kill failed; the flag stays open and keeps paging", "flagId", f.ID, "err", err)
		return
	}
	if !ok {
		a.log.Info("automatic content-flag kill skipped: the flag was acknowledged, dismissed or resolved first, or its broadcast muted",
			"flagId", f.ID, "broadcastKey", f.BroadcastKey, "reason", reason)
		return
	}
	if banned != nil {
		if k := a.enforceKill(ctx, f.BroadcastID, why, store.ActorContentFlag, cooldown, *banned); k.projErr != nil {
			a.log.Error("projecting an automatic kill's Ban CR failed; the reconciler retries", "banId", k.ban.ID, "err", k.projErr)
		}
		a.afterMutation()
	}
	a.kickDeliveries()
	a.log.Warn("broadcast killed automatically by content flagging", "flagId", resolved.ID, "broadcastKey", f.BroadcastKey,
		"reason", reason)
}

// ScreeningTickInterval is how often the leader sweeps the unresolved flags.
// Short next to -content-flag-repage and -content-flag-stale-after, so a
// reminder or a session end is never more than a tick late.
const ScreeningTickInterval = 15 * time.Second

// RunScreening is the leader's R40 work (docs/71 D9, D15, D17): the flag
// sweep every ScreeningTickInterval and the calibration-data pruning hourly.
// Started only from the leadership callback, like the reconciler; every write
// it makes is conditional in SQL, so an overlapping handover sends one
// reminder and one kill, not two.
func (a *API) RunScreening(ctx context.Context) {
	if !a.opts.Config.Screening {
		return
	}
	sweep := time.NewTicker(ScreeningTickInterval)
	defer sweep.Stop()
	prune := time.NewTicker(time.Hour)
	defer prune.Stop()
	a.log.Info("content-flag sweep started", "interval", ScreeningTickInterval)
	a.PruneScreening(ctx)
	for {
		a.ScreeningTick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-sweep.C:
		case <-prune.C:
			a.PruneScreening(ctx)
		}
	}
}

// ScreeningTick is one sweep over the unresolved flags. Exported as the seam
// the lifecycle tests drive with a fake clock.
//
// In order, per flag: a session that has ended resolves the flag `ended`; an
// observed flag whose sampler went quiet resolves `cleared` (stale); a
// flag-mode flag past -content-flag-deadline — open, unmuted, signal live —
// is killed; and an open flag-mode flag whose last page is a repage old is
// reminded, live signal or unknown.
func (a *API) ScreeningTick(ctx context.Context) {
	flags, err := a.opts.Store.UnresolvedContentFlags(ctx, "")
	if err != nil {
		if ctx.Err() == nil {
			a.log.Warn("content-flag sweep: listing flags failed", "err", err)
		}
		return
	}
	if len(flags) == 0 {
		return
	}
	now := a.now()
	cfg := a.opts.Config
	webhooks := cfg.ConfigWebhooks()

	// Session ends are judged only on a COMPLETE scan: a broadcast on a pod
	// that did not answer is unknown, not ended, and ending its flag would
	// silence a page.
	var (
		snap     relayscan.Snapshot
		complete bool
	)
	if a.opts.Fleet != nil {
		if s, err := a.opts.Fleet.Snapshot(ctx); err == nil && s.PodsResolved > 0 && s.PodsAnswered == s.PodsResolved {
			snap, complete = s, true
		}
	}
	mutes, err := a.opts.Store.ActiveMutes(ctx, now)
	if err != nil {
		a.log.Warn("content-flag sweep: reading mutes failed; no deadline kill this tick", "err", err)
	}

	paged := false
	for _, f := range flags {
		if complete {
			agg, found := snap.Broadcast(f.BroadcastID)
			if !found || !sessionMatches(agg, f.PublisherEpoch) {
				if _, ok, err := a.opts.Store.ResolveContentFlag(ctx, f, events.ContentFlagResolutionEnded, "", "system", now, webhooks); err != nil {
					a.log.Warn("content-flag sweep: ending a flag failed", "flagId", f.ID, "err", err)
				} else if ok {
					paged = paged || f.Mode == screening.ModeFlag
				}
				continue
			}
		}
		signal := f.Signal(now, cfg.ContentFlagStaleAfter)
		if f.Mode == screening.ModeObserve {
			if signal == events.ContentFlagSignalUnknown {
				if _, _, err := a.opts.Store.ResolveContentFlag(ctx, f, events.ContentFlagResolutionCleared, store.FlagReasonStale, "system", now, webhooks); err != nil {
					a.log.Warn("content-flag sweep: clearing a stale observed flag failed", "flagId", f.ID, "err", err)
				}
			}
			continue
		}
		if f.State != store.FlagStateOpen {
			continue // acknowledged: no reminders, no deadline (D9)
		}
		_, muted := mutes[f.BroadcastID]
		if cfg.ContentFlagDeadline > 0 && mutes != nil && !muted && signal == events.ContentFlagSignalLive &&
			now.Sub(f.RaisedAt) >= cfg.ContentFlagDeadline {
			a.autoKill(ctx, f, store.FlagReasonDeadline)
			continue
		}
		if _, ok, err := a.opts.Store.RemindContentFlag(ctx, f.ID, signal, cfg.ContentFlagRepage, now, webhooks); err != nil {
			a.log.Warn("content-flag sweep: a reminder failed", "flagId", f.ID, "err", err)
		} else if ok {
			paged = true
			a.log.Info("content flag re-paged", "flagId", f.ID, "broadcastKey", f.BroadcastKey, "signal", signal)
		}
	}
	if paged {
		a.kickDeliveries()
	}
}

// PruneScreening ages out calibration data past -screening-sample-retention
// (D17). Flag-mode flags — the audit trail — are never touched.
func (a *API) PruneScreening(ctx context.Context) {
	n, err := a.opts.Store.PruneScreening(ctx, a.now().Add(-a.opts.Config.ScreeningSampleRetention))
	if err != nil {
		if ctx.Err() == nil {
			a.log.Warn("screening prune failed", "err", err)
		}
		return
	}
	if n.Flags+n.Events+n.Samples+n.Sessions > 0 {
		a.log.Info("pruned screening data", "flags", n.Flags, "events", n.Events, "samples", n.Samples,
			"sessions", n.Sessions, "olderThan", a.opts.Config.ScreeningSampleRetention.String())
	}
}

func marshalPayload(v map[string]any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return raw
}
