package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Tuhis/gawk/gawk-server/events"
	"github.com/Tuhis/gawk/gawk-server/screening"
)

// R40's content flags (docs/71 D9–D12, D14, D15, D19, §9.4).
//
// A flag is one incident: the first tripped POST for a broadcast raises it,
// the ones after update it, and it ends in exactly one resolution. Every
// transition that a person must hear about writes its event in the SAME
// transaction as the flag row, with the webhook fan-out — the
// AppendEventAndEnqueue rule. A raise whose page was lost would be worse than
// a lost update to the row: the next tripped POST would find the flag and
// merely update it, and nobody would ever be told.

// CategoryScreening is the category of R40's calibration rows: the
// content_flag.observed events a sampler in observe mode produces (docs/71
// D16). They are pruned with the samples (D17) and are delivered only to a
// webhook that names them.
const CategoryScreening = "screening"

// EventContentFlagObserved is an observe-mode flag's one event (D19).
const EventContentFlagObserved = "content_flag.observed"

// ScreeningEventTypes is the closed vocabulary of screening rows.
func ScreeningEventTypes() []string {
	return []string{EventContentFlagObserved}
}

// Flag states (D9). A flag is unresolved while open or acknowledged.
const (
	FlagStateOpen         = "open"
	FlagStateAcknowledged = "acknowledged"
	FlagStateResolved     = "resolved"
)

// Resolution reasons: why an automatic resolution happened. A closed
// vocabulary, because it is delivered in content_flag.resolved's `reason`.
const (
	// FlagReasonDeadline: the deadline kill (D15).
	FlagReasonDeadline = "deadline"
	// FlagReasonPolicy: the auto-kill tier (D14).
	FlagReasonPolicy = "policy"
	// FlagReasonStale: an observed flag whose sampler went quiet (D19).
	FlagReasonStale = "stale"
)

// ActorContentFlag is the actor of an automatic kill (D14, D15): the system,
// named so the audit trail tells it from an operator and from ban expiry.
const ActorContentFlag = "system:content-flag"

// Content-flag sentinels. Check with errors.Is.
var (
	// ErrFlagNotOpen: acknowledge on a flag that is not open.
	ErrFlagNotOpen = errors.New("store: content flag is not open")
	// ErrFlagResolved: dismiss on a flag already resolved.
	ErrFlagResolved = errors.New("store: content flag is already resolved")
	// ErrFlagObserved: acknowledge or dismiss on an observe-mode flag, which
	// pages nobody and so has nothing to acknowledge (D19).
	ErrFlagObserved = errors.New("store: an observed flag cannot be acknowledged or dismissed")
)

// ContentFlag is one row of content_flags.
type ContentFlag struct {
	ID int64
	// Mode is screening.ModeFlag or screening.ModeObserve.
	Mode string
	// State is one of the FlagState* values.
	State string
	// Resolution is one of events.ContentFlagResolution*, empty while
	// unresolved; ResolutionReason one of the FlagReason* values or empty.
	Resolution       string
	ResolutionReason string
	BroadcastID      string
	BroadcastKey     string
	PublisherEpoch   string
	SamplerID        string
	RaisedAt         time.Time
	UpdatedAt        time.Time
	// LastPostAt is the last tripped POST: the signal is live until
	// -content-flag-stale-after past it (D9).
	LastPostAt     time.Time
	AcknowledgedAt *time.Time
	AcknowledgedBy string
	ResolvedAt     *time.Time
	ResolvedBy     string
	// PeakScore is the highest window max any POST for the flag reported.
	PeakScore      float64
	Reminders      int
	LastRemindedAt *time.Time
	// Body is the last POST, kept for audit: what the portal shows and what
	// an automatic kill decided on (D14).
	Body screening.ContentFlag
	// Trips counts the window trips this incident absorbed: a re-trip inside
	// the rejoin window is the same incident (docs/72 D3).
	Trips int
	// ClearedAt is set while a clear is held for the rejoin window: the
	// signal is cleared and the incident not yet resolved (D3).
	ClearedAt *time.Time
	// ReturnedAfterReview is true when an earlier incident on the broadcast
	// was acknowledged, dismissed or killed within ReviewLookback (D4).
	ReturnedAfterReview bool
	// MutedUntil and MuteScope record what a dismissal muted (D5). An
	// episode mute's end moves to the clear that ended it.
	MutedUntil *time.Time
	MuteScope  string
}

// Unresolved reports whether the flag is open or acknowledged.
func (f ContentFlag) Unresolved() bool { return f.State != FlagStateResolved }

// Signal is the flag's signal at now (D9): `cleared` once the sampler said
// so — a held clear included (docs/72 D3) — `live` while a tripped POST
// arrived within staleAfter, `unknown` after.
func (f ContentFlag) Signal(now time.Time, staleAfter time.Duration) string {
	if f.Resolution == events.ContentFlagResolutionCleared || f.ClearedAt != nil {
		return "cleared"
	}
	if now.Sub(f.LastPostAt) <= staleAfter {
		return events.ContentFlagSignalLive
	}
	return events.ContentFlagSignalUnknown
}

const flagColumns = `id, mode, state, resolution, resolution_reason, broadcast_id, broadcast_key,
	publisher_epoch, sampler_id, raised_at, updated_at, last_post_at, acknowledged_at, acknowledged_by,
	resolved_at, resolved_by, peak_score, reminders, last_reminded_at, body,
	trips, cleared_at, returned_after_review, muted_until, mute_scope`

func scanFlag(row pgx.Row) (ContentFlag, error) {
	var (
		f                                                     ContentFlag
		resolution, reason, key, ackBy, resolvedBy, muteScope *string
		body                                                  []byte
	)
	if err := row.Scan(&f.ID, &f.Mode, &f.State, &resolution, &reason, &f.BroadcastID, &key,
		&f.PublisherEpoch, &f.SamplerID, &f.RaisedAt, &f.UpdatedAt, &f.LastPostAt, &f.AcknowledgedAt, &ackBy,
		&f.ResolvedAt, &resolvedBy, &f.PeakScore, &f.Reminders, &f.LastRemindedAt, &body,
		&f.Trips, &f.ClearedAt, &f.ReturnedAfterReview, &f.MutedUntil, &muteScope); err != nil {
		return ContentFlag{}, err
	}
	f.Resolution, f.ResolutionReason = derefString(resolution), derefString(reason)
	f.BroadcastKey, f.AcknowledgedBy, f.ResolvedBy = derefString(key), derefString(ackBy), derefString(resolvedBy)
	f.MuteScope = derefString(muteScope)
	f.RaisedAt, f.UpdatedAt, f.LastPostAt = f.RaisedAt.UTC(), f.UpdatedAt.UTC(), f.LastPostAt.UTC()
	for _, t := range []*time.Time{f.AcknowledgedAt, f.ResolvedAt, f.LastRemindedAt, f.ClearedAt, f.MutedUntil} {
		if t != nil {
			*t = t.UTC()
		}
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &f.Body); err != nil {
			return ContentFlag{}, fmt.Errorf("store: content flag %d body: %w", f.ID, err)
		}
	}
	return f, nil
}

// FlagPost is one validated POST /api/v1/content-flags.
type FlagPost struct {
	Body screening.ContentFlag
	// BroadcastKey is the HMAC'd key from the fleet scan, "" when the scan
	// did not see the broadcast.
	BroadcastKey string
	// Actor is the sampler's service identity.
	Actor string
	Now   time.Time
	// Rejoin is -content-flag-rejoin (docs/72 D3): a clear of an open flag
	// is held this long, and a trip inside it rejoins the same incident.
	// Zero resolves a clear at once, as R40 did.
	Rejoin time.Duration
}

// FlagPostResult is what a POST did: one of the screening.FlagStatus*
// values, and the flag it raised, updated or resolved.
type FlagPostResult struct {
	Status string
	Flag   ContentFlag
}

// PostContentFlag applies one content-flag POST (D9, D10, D19): raise,
// update, resolve, or nothing (muted, ignored). The caller has validated the
// body and decided the D2 backstop and D12's no-pager rule; everything that
// depends on stored state happens here, in one transaction.
//
// Two replicas taking two POSTs for one broadcast at once both find no
// unresolved flag and both insert; the partial unique index refuses the
// second, which then retries once and finds the first one's row to update.
func (s *Store) PostContentFlag(ctx context.Context, p FlagPost, config []ConfigWebhook) (FlagPostResult, error) {
	res, err := s.postContentFlag(ctx, p, config)
	if isUniqueViolation(err, "content_flags_one_unresolved") {
		res, err = s.postContentFlag(ctx, p, config)
	}
	if err != nil {
		return FlagPostResult{}, fmt.Errorf("store: content flag: %w", err)
	}
	return res, nil
}

func (s *Store) postContentFlag(ctx context.Context, p FlagPost, config []ConfigWebhook) (FlagPostResult, error) {
	now := p.Now.UTC()
	b := p.Body
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return FlagPostResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	existing, found, err := lockUnresolvedFlag(ctx, tx, b.BroadcastID, b.Mode)
	if err != nil {
		return FlagPostResult{}, err
	}

	// A dismissal mutes re-flagging of the broadcast (D10). Observe mode
	// ignores mutes: it pages no one, and its data is what calibration needs.
	// Read AFTER the lock above: a dismissal holds that row lock until it
	// commits, and at READ COMMITTED this statement's snapshot is taken once
	// the wait is over, so it sees the mute a concurrent dismissal wrote.
	// Read before it, the mute was missing, the dismissed flag was gone, and
	// the POST raised — and paged — a new flag.
	if b.Mode == screening.ModeFlag {
		muted, err := applyMute(ctx, tx, b, now)
		if err != nil {
			return FlagPostResult{}, err
		}
		if muted {
			if err := tx.Commit(ctx); err != nil {
				return FlagPostResult{}, err
			}
			return FlagPostResult{Status: screening.FlagStatusMuted}, nil
		}
	}

	var res FlagPostResult
	switch b.State {
	case screening.StateTripped:
		if found && existing.PublisherEpoch != b.PublisherEpoch {
			// The broadcast's publisher session changed under an unresolved
			// flag, so the old session ended (D9). The leader's sweep would
			// say so within a tick; saying it here keeps the one-unresolved
			// index from refusing the new session's raise.
			if _, _, err := s.resolveFlagTx(ctx, tx, existing, events.ContentFlagResolutionEnded, "", "system", now, config); err != nil {
				return FlagPostResult{}, err
			}
			found = false
		}
		if found && existing.ClearedAt != nil && now.Sub(*existing.ClearedAt) > p.Rejoin {
			// A held clear whose window passed before the sweep got to it:
			// that incident is over, and this trip is a new one (docs/72 D3).
			if _, _, err := s.resolveFlagTx(ctx, tx, existing, events.ContentFlagResolutionCleared, "", "system", now, config); err != nil {
				return FlagPostResult{}, err
			}
			found = false
		}
		if !found {
			returned := false
			if b.Mode == screening.ModeFlag {
				if returned, err = reviewedRecently(ctx, tx, b.BroadcastID, now); err != nil {
					return FlagPostResult{}, err
				}
			}
			flag, err := insertFlag(ctx, tx, p, now, returned)
			if err != nil {
				return FlagPostResult{}, err
			}
			if err := s.appendFlagRaisedTx(ctx, tx, flag, p.Actor, config); err != nil {
				return FlagPostResult{}, err
			}
			res = FlagPostResult{Status: screening.FlagStatusRaised, Flag: flag}
		} else {
			// A trip on a held clear rejoins the incident: one more trip, no
			// new row and no page (docs/72 D3).
			flag, err := updateFlag(ctx, tx, existing.ID, p, now, existing.ClearedAt != nil)
			if err != nil {
				return FlagPostResult{}, err
			}
			res = FlagPostResult{Status: screening.FlagStatusUpdated, Flag: flag}
		}
	case screening.StateCleared:
		// A clear for another session than the unresolved flag's says nothing
		// about that flag: its session has ended and the sweep resolves it.
		if !found || existing.PublisherEpoch != b.PublisherEpoch {
			return FlagPostResult{Status: screening.FlagStatusIgnored}, tx.Commit(ctx)
		}
		// An open flag's clear is held for the rejoin window: flicker is one
		// incident, and a re-trip inside the window must not page again
		// (docs/72 D3). The leader's sweep resolves it once the window passes.
		if p.Rejoin > 0 && existing.State == FlagStateOpen {
			flag := existing
			if existing.ClearedAt == nil {
				if flag, err = scanFlag(tx.QueryRow(ctx, `UPDATE content_flags SET cleared_at = $2, updated_at = $2
					WHERE id = $1 RETURNING `+flagColumns, existing.ID, now)); err != nil {
					return FlagPostResult{}, err
				}
			}
			if err := tx.Commit(ctx); err != nil {
				return FlagPostResult{}, err
			}
			return FlagPostResult{Status: screening.FlagStatusUpdated, Flag: flag}, nil
		}
		// A clear resolves an acknowledged flag at once, so a later episode
		// in the same session raises a NEW flag and pages again (D9).
		flag, _, err := s.resolveFlagTx(ctx, tx, existing, events.ContentFlagResolutionCleared, "", p.Actor, now, config)
		if err != nil {
			return FlagPostResult{}, err
		}
		res = FlagPostResult{Status: screening.FlagStatusResolved, Flag: flag}
	default:
		return FlagPostResult{}, fmt.Errorf("unknown state %q", b.State)
	}
	if err := tx.Commit(ctx); err != nil {
		return FlagPostResult{}, err
	}
	return res, nil
}

func lockUnresolvedFlag(ctx context.Context, tx pgx.Tx, broadcastID, mode string) (ContentFlag, bool, error) {
	row := tx.QueryRow(ctx, `SELECT `+flagColumns+` FROM content_flags
		WHERE broadcast_id = $1 AND mode = $2 AND state <> 'resolved' FOR UPDATE`, broadcastID, mode)
	f, err := scanFlag(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return ContentFlag{}, false, nil
	}
	if err != nil {
		return ContentFlag{}, false, err
	}
	return f, true, nil
}

func insertFlag(ctx context.Context, tx pgx.Tx, p FlagPost, now time.Time, returned bool) (ContentFlag, error) {
	body, err := json.Marshal(p.Body)
	if err != nil {
		return ContentFlag{}, err
	}
	row := tx.QueryRow(ctx, `INSERT INTO content_flags
		(mode, state, broadcast_id, broadcast_key, publisher_epoch, sampler_id, raised_at, updated_at, last_post_at, peak_score, body,
		 returned_after_review)
		VALUES ($1,'open',$2,$3,$4,$5,$6,$6,$6,$7,$8,$9) RETURNING `+flagColumns,
		p.Body.Mode, p.Body.BroadcastID, nullString(p.BroadcastKey), p.Body.PublisherEpoch, p.Body.SamplerID, now,
		p.Body.Scores.Max, body, returned)
	return scanFlag(row)
}

// updateFlag records a tripped POST on the unresolved flag; rejoin says it
// ends a held clear, which counts one more trip (docs/72 D3).
func updateFlag(ctx context.Context, tx pgx.Tx, id int64, p FlagPost, now time.Time, rejoin bool) (ContentFlag, error) {
	body, err := json.Marshal(p.Body)
	if err != nil {
		return ContentFlag{}, err
	}
	row := tx.QueryRow(ctx, `UPDATE content_flags SET
		updated_at = $2, last_post_at = $2, sampler_id = $3, peak_score = GREATEST(peak_score, $4), body = $5,
		broadcast_key = COALESCE(broadcast_key, $6),
		trips = trips + CASE WHEN $7 THEN 1 ELSE 0 END, cleared_at = NULL
		WHERE id = $1 RETURNING `+flagColumns,
		id, now, p.Body.SamplerID, p.Body.Scores.Max, body, nullString(p.BroadcastKey), rejoin)
	return scanFlag(row)
}

// ReviewLookback bounds docs/72 D4: a review older than this does not make a
// new raise a return. Broadcast IDs are re-minted eventually; a day is far
// longer than any one broadcast's away grace.
const ReviewLookback = 24 * time.Hour

// reviewedRecently reports whether a person acknowledged, dismissed or killed
// an earlier flag-mode incident on the broadcast within ReviewLookback.
func reviewedRecently(ctx context.Context, tx pgx.Tx, broadcastID string, now time.Time) (bool, error) {
	var ok bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM content_flags
		WHERE broadcast_id = $1 AND mode = 'flag' AND state = 'resolved' AND resolved_at > $2
		  AND (acknowledged_at IS NOT NULL OR resolution IN ('dismissed','killed')))`,
		broadcastID, now.Add(-ReviewLookback)).Scan(&ok)
	return ok, err
}

// applyMute decides whether the broadcast's mute silences this flag-mode POST
// (D10, docs/72 D5), and ends the mutes the POST itself ends: an `episode` or
// `session` mute belongs to one publisher session, so a POST for another one
// is not muted and drops it, and an `episode` mute ends on its session's
// clear. Locks the mute row, so a concurrent dismissal is seen.
func applyMute(ctx context.Context, tx pgx.Tx, b screening.ContentFlag, now time.Time) (bool, error) {
	var (
		m      Mute
		epoch  *string
		flagID *int64
	)
	err := tx.QueryRow(ctx, `SELECT muted_until, scope, publisher_epoch, flag_id FROM screening_mutes
		WHERE broadcast_id = $1 AND muted_until > $2 FOR UPDATE`, b.BroadcastID, now).Scan(&m.Until, &m.Scope, &epoch, &flagID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	m.Epoch = derefString(epoch)
	end := func() error {
		if _, err := tx.Exec(ctx, `DELETE FROM screening_mutes WHERE broadcast_id = $1`, b.BroadcastID); err != nil {
			return err
		}
		if flagID != nil {
			_, err := tx.Exec(ctx, `UPDATE content_flags SET muted_until = LEAST(muted_until, $2) WHERE id = $1`, *flagID, now)
			return err
		}
		return nil
	}
	switch {
	case !m.Covers(b.PublisherEpoch):
		return false, end()
	case m.Scope == MuteScopeEpisode && b.State == screening.StateCleared:
		return true, end()
	}
	return true, nil
}

// resolveFlagTx resolves f inside tx unless it already is, and — for a
// flag-mode flag — writes content_flag.resolved with its fan-out. An observed
// flag resolves silently: it paged nobody, and its resolution must not page
// either (D19). ok is false when another transaction resolved it first.
func (s *Store) resolveFlagTx(ctx context.Context, tx pgx.Tx, f ContentFlag, resolution, reason, actor string, now time.Time, config []ConfigWebhook) (ContentFlag, bool, error) {
	row := tx.QueryRow(ctx, `UPDATE content_flags SET
		state = 'resolved', resolution = $2, resolution_reason = $3, resolved_at = $4, resolved_by = $5, updated_at = $4
		WHERE id = $1 AND state <> 'resolved' RETURNING `+flagColumns,
		f.ID, resolution, nullString(reason), now, actor)
	out, err := scanFlag(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return f, false, nil
	}
	if err != nil {
		return ContentFlag{}, false, err
	}
	if out.Mode != screening.ModeFlag {
		return out, true, nil
	}
	payload := flagPayload(out, SummarizeContentFlagResolved(out.BroadcastID, resolution, reason, actor), map[string]any{
		PayloadResolution: resolution,
	})
	if reason != "" {
		payload[PayloadReason] = reason
	}
	if _, err := s.appendEventTx(ctx, tx, Event{
		Type: EventContentFlagResolved, OccurredAt: now, Actor: actor,
		BroadcastKey: out.BroadcastKey, BroadcastID: out.BroadcastID, Payload: mustJSON(payload),
	}, config); err != nil {
		return ContentFlag{}, false, err
	}
	return out, true, nil
}

// appendFlagRaisedTx writes the raise's one event: content_flag.raised — the
// page — for a flag-mode flag, or the opt-in content_flag.observed for an
// observe-mode one (D16, D19).
func (s *Store) appendFlagRaisedTx(ctx context.Context, tx pgx.Tx, f ContentFlag, actor string, config []ConfigWebhook) error {
	ev := Event{
		Type: EventContentFlag, OccurredAt: f.RaisedAt, Actor: actor,
		BroadcastKey: f.BroadcastKey, BroadcastID: f.BroadcastID,
	}
	summary := SummarizeContentFlagRaised(f.BroadcastID, f.Body.Window.FlaggedSamples, f.Body.Window.Samples, f.Body.Scores.Max, f.ReturnedAfterReview)
	if f.Mode == screening.ModeObserve {
		ev.Type, ev.Category = EventContentFlagObserved, CategoryScreening
		summary = SummarizeContentFlagObserved(f.BroadcastID, f.Body.Window.FlaggedSamples, f.Body.Window.Samples, f.Body.Scores.Max)
	}
	b := f.Body
	extra := map[string]any{
		PayloadSamplerID: b.SamplerID,
		PayloadWindow: events.ContentFlagWindow{
			Start: rfc3339(b.Window.Start), End: rfc3339(b.Window.End),
			Samples: b.Window.Samples, FlaggedSamples: b.Window.FlaggedSamples,
		},
		PayloadScores: events.ContentFlagScores{Max: b.Scores.Max, Mean: b.Scores.Mean},
		PayloadLabels: b.Labels,
	}
	if f.ReturnedAfterReview {
		extra[PayloadReturnedAfterReview] = true
	}
	payload := flagPayload(f, summary, extra)
	ev.Payload = mustJSON(payload)
	_, err := s.appendEventTx(ctx, tx, ev, config)
	return err
}

// flagPayload is every flag event's common payload: the sentence and the flag
// it is about, plus the event's own keys.
func flagPayload(f ContentFlag, summary string, extra map[string]any) map[string]any {
	out := map[string]any{PayloadSummary: summary, PayloadFlagID: f.ID}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func mustJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return raw
}

// GetContentFlag returns one flag by ID.
func (s *Store) GetContentFlag(ctx context.Context, id int64) (ContentFlag, error) {
	f, err := scanFlag(s.pool.QueryRow(ctx, `SELECT `+flagColumns+` FROM content_flags WHERE id = $1`, id))
	if err != nil {
		return ContentFlag{}, noRows(err)
	}
	return f, nil
}

// Flag list filters.
const (
	FlagFilterUnresolved = "unresolved"
	FlagFilterResolved   = "resolved"
	FlagFilterAll        = "all"
)

// DefaultFlagLimit / MaxFlagLimit bound one page of the flag list.
const (
	DefaultFlagLimit = 100
	MaxFlagLimit     = 500
)

// FlagQuery is one GET /api/v1/content-flags.
type FlagQuery struct {
	// State is a FlagFilter* value; empty means unresolved.
	State string
	// Mode is screening.ModeFlag, screening.ModeObserve or FlagFilterAll;
	// empty means flag.
	Mode  string
	Limit int
}

// ListContentFlags returns flags newest first.
func (s *Store) ListContentFlags(ctx context.Context, q FlagQuery) ([]ContentFlag, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultFlagLimit
	}
	if limit > MaxFlagLimit {
		limit = MaxFlagLimit
	}
	state := q.State
	if state == "" {
		state = FlagFilterUnresolved
	}
	mode := q.Mode
	if mode == "" {
		mode = screening.ModeFlag
	}
	rows, err := s.pool.Query(ctx, `SELECT `+flagColumns+` FROM content_flags
		WHERE ($1 = 'all' OR ($1 = 'unresolved' AND state <> 'resolved') OR ($1 = 'resolved' AND state = 'resolved'))
		  AND ($2 = 'all' OR mode = $2)
		ORDER BY id DESC LIMIT $3`, state, mode, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list content flags: %w", err)
	}
	return collectFlags(rows)
}

// UnresolvedContentFlags returns every unresolved flag of one mode, or of
// both when mode is empty — the leader's sweep and the broadcast view.
func (s *Store) UnresolvedContentFlags(ctx context.Context, mode string) ([]ContentFlag, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+flagColumns+` FROM content_flags
		WHERE state <> 'resolved' AND ($1 = '' OR mode = $1) ORDER BY id`, mode)
	if err != nil {
		return nil, fmt.Errorf("store: unresolved content flags: %w", err)
	}
	return collectFlags(rows)
}

func collectFlags(rows pgx.Rows) ([]ContentFlag, error) {
	defer rows.Close()
	out := []ContentFlag{}
	for rows.Next() {
		f, err := scanFlag(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// AcknowledgeContentFlag moves an open flag-mode flag to acknowledged:
// reminders and the deadline stop; nothing is resolved (D9).
func (s *Store) AcknowledgeContentFlag(ctx context.Context, id int64, actor string, now time.Time) (ContentFlag, error) {
	now = now.UTC()
	f, err := scanFlag(s.pool.QueryRow(ctx, `UPDATE content_flags SET
		state = 'acknowledged', acknowledged_at = $2, acknowledged_by = $3, updated_at = $2
		WHERE id = $1 AND state = 'open' AND mode = 'flag' RETURNING `+flagColumns, id, now, actor))
	if err == nil {
		return f, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ContentFlag{}, fmt.Errorf("store: acknowledge content flag: %w", err)
	}
	cur, err := s.GetContentFlag(ctx, id)
	if err != nil {
		return ContentFlag{}, err
	}
	if cur.Mode != screening.ModeFlag {
		return ContentFlag{}, ErrFlagObserved
	}
	return ContentFlag{}, ErrFlagNotOpen
}

// Dismissal reasons (docs/72 D5): why an operator judged a flag not a
// violation. A closed vocabulary, delivered as content_flag.resolved's
// `reason`.
const (
	DismissReasonGameScene     = "game_scene"
	DismissReasonNotExplicit   = "not_explicit"
	DismissReasonDetectorError = "detector_error"
	DismissReasonOther         = "other"
)

// Mute scopes (docs/72 D5).
const (
	// MuteScopeEpisode mutes until the sampler next reports the window
	// cleared for the dismissed flag's publisher session.
	MuteScopeEpisode = "episode"
	// MuteScopeDuration mutes for a fixed time, every session: R40's mute.
	MuteScopeDuration = "duration"
	// MuteScopeSession mutes until the dismissed flag's publisher session
	// ends.
	MuteScopeSession = "session"
)

// ErrInvalidDismissal: a dismissal names a reason or scope outside the
// vocabulary.
var ErrInvalidDismissal = errors.New("store: invalid dismissal")

// Dismissal is how an operator dismissed a flag (docs/72 D5).
type Dismissal struct {
	// Reason is one of the DismissReason* values, or empty.
	Reason string
	// Scope is one of the MuteScope* values.
	Scope string
	// MutedUntil ends the mute: the caller's -content-flag-mute for a
	// duration mute, its cap for the session-bound two.
	MutedUntil time.Time
}

func (d Dismissal) validate() error {
	switch d.Reason {
	case "", DismissReasonGameScene, DismissReasonNotExplicit, DismissReasonDetectorError, DismissReasonOther:
	default:
		return fmt.Errorf("%w: reason %q", ErrInvalidDismissal, d.Reason)
	}
	switch d.Scope {
	case MuteScopeEpisode, MuteScopeDuration, MuteScopeSession:
	default:
		return fmt.Errorf("%w: scope %q", ErrInvalidDismissal, d.Scope)
	}
	return nil
}

// Mute is one broadcast's active mute (D10, docs/72 D5).
type Mute struct {
	Until time.Time
	Scope string
	// Epoch is the publisher session an episode or session mute is bound
	// to; empty for a duration mute.
	Epoch string
}

// Covers reports whether the mute silences the given publisher session.
func (m Mute) Covers(epoch string) bool {
	return m.Epoch == "" || m.Epoch == epoch
}

// DismissContentFlag resolves a flag-mode flag as dismissed and mutes its
// broadcast as d says (D10, docs/72 D5), with the resolved event, in one
// transaction.
func (s *Store) DismissContentFlag(ctx context.Context, id int64, actor string, now time.Time, d Dismissal, config []ConfigWebhook) (ContentFlag, error) {
	if err := d.validate(); err != nil {
		return ContentFlag{}, err
	}
	now = now.UTC()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ContentFlag{}, fmt.Errorf("store: dismiss content flag: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	f, err := scanFlag(tx.QueryRow(ctx, `SELECT `+flagColumns+` FROM content_flags WHERE id = $1 FOR UPDATE`, id))
	if err != nil {
		return ContentFlag{}, noRows(err)
	}
	switch {
	case f.Mode != screening.ModeFlag:
		return ContentFlag{}, ErrFlagObserved
	case !f.Unresolved():
		return ContentFlag{}, ErrFlagResolved
	}
	if _, err := tx.Exec(ctx, `UPDATE content_flags SET muted_until = $2, mute_scope = $3 WHERE id = $1`,
		f.ID, d.MutedUntil.UTC(), d.Scope); err != nil {
		return ContentFlag{}, fmt.Errorf("store: dismiss content flag: %w", err)
	}
	out, _, err := s.resolveFlagTx(ctx, tx, f, events.ContentFlagResolutionDismissed, d.Reason, actor, now, config)
	if err != nil {
		return ContentFlag{}, fmt.Errorf("store: dismiss content flag: %w", err)
	}
	epoch := ""
	if d.Scope != MuteScopeDuration {
		epoch = f.PublisherEpoch
	}
	if _, err := tx.Exec(ctx, `INSERT INTO screening_mutes (broadcast_id, muted_until, muted_by, flag_id, scope, publisher_epoch)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (broadcast_id) DO UPDATE SET muted_until = EXCLUDED.muted_until,
			muted_by = EXCLUDED.muted_by, flag_id = EXCLUDED.flag_id,
			scope = EXCLUDED.scope, publisher_epoch = EXCLUDED.publisher_epoch`,
		f.BroadcastID, d.MutedUntil.UTC(), actor, f.ID, d.Scope, nullString(epoch)); err != nil {
		return ContentFlag{}, fmt.Errorf("store: mute broadcast: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return ContentFlag{}, fmt.Errorf("store: dismiss content flag: %w", err)
	}
	return out, nil
}

// ResolveContentFlag resolves one flag (any reason the leader's sweep, a kill
// or the auto-kill tier has), with its event when it is a flag-mode flag. ok
// is false when it was already resolved — two replicas racing, or a POST
// that got there first — which is not an error.
func (s *Store) ResolveContentFlag(ctx context.Context, f ContentFlag, resolution, reason, actor string, now time.Time, config []ConfigWebhook) (ContentFlag, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ContentFlag{}, false, fmt.Errorf("store: resolve content flag: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	out, ok, err := s.resolveFlagTx(ctx, tx, f, resolution, reason, actor, now.UTC(), config)
	if err != nil {
		return ContentFlag{}, false, fmt.Errorf("store: resolve content flag: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return ContentFlag{}, false, fmt.Errorf("store: resolve content flag: %w", err)
	}
	return out, ok, nil
}

// AutoKillContentFlag is the claim D14's and D15's automatic kill makes on a
// flag-mode flag, in one transaction: it locks the flag and, only while the
// flag is still unresolved — still open, when requireOpen, as the deadline
// needs (acknowledging stops it, D9) — and its broadcast is not muted (D10),
// bans the broadcast and resolves the flag auto_killed, with its event. An
// acknowledgement, a dismissal or any resolution that commits first wins:
// ok is false and nothing is banned (PR #500 review: the sweep killed on the
// flag list it read at the start of its tick). A ban that cannot be written
// rolls the claim back, so the flag stays open and keeps paging.
//
// An active ban already covering the broadcast stands — an operator's, or a
// racing replica's — and the flag still resolves, since the broadcast is gone
// either way; banned is then nil.
func (s *Store) AutoKillContentFlag(ctx context.Context, id int64, requireOpen bool, ban Ban, reason string, now time.Time, config []ConfigWebhook) (resolved ContentFlag, banned *Ban, ok bool, err error) {
	now = now.UTC()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ContentFlag{}, nil, false, fmt.Errorf("store: auto-kill content flag: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	f, err := scanFlag(tx.QueryRow(ctx, `SELECT `+flagColumns+` FROM content_flags WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return ContentFlag{}, nil, false, nil
	}
	if err != nil {
		return ContentFlag{}, nil, false, fmt.Errorf("store: auto-kill content flag: %w", err)
	}
	if f.Mode != screening.ModeFlag || !f.Unresolved() || (requireOpen && f.State != FlagStateOpen) {
		return f, nil, false, nil
	}
	var muted bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM screening_mutes WHERE broadcast_id = $1 AND muted_until > $2
		AND (publisher_epoch IS NULL OR publisher_epoch = $3))`,
		f.BroadcastID, now, f.PublisherEpoch).Scan(&muted); err != nil {
		return ContentFlag{}, nil, false, fmt.Errorf("store: auto-kill content flag: %w", err)
	}
	if muted {
		return f, nil, false, nil
	}

	// The ban in a savepoint: a duplicate active ban is an answer, not an
	// error, and must not abort the claim around it.
	sp, err := tx.Begin(ctx)
	if err != nil {
		return ContentFlag{}, nil, false, fmt.Errorf("store: auto-kill content flag: %w", err)
	}
	created, err := s.insertBan(ctx, sp, ban)
	switch {
	case errors.Is(err, ErrDuplicateActive):
		if err := sp.Rollback(ctx); err != nil {
			return ContentFlag{}, nil, false, fmt.Errorf("store: auto-kill content flag: %w", err)
		}
	case err != nil:
		return ContentFlag{}, nil, false, err
	default:
		if err := sp.Commit(ctx); err != nil {
			return ContentFlag{}, nil, false, fmt.Errorf("store: auto-kill content flag: %w", err)
		}
		banned = &created
	}

	resolved, ok, err = s.resolveFlagTx(ctx, tx, f, events.ContentFlagResolutionAutoKilled, reason, ActorContentFlag, now, config)
	if err != nil || !ok {
		return ContentFlag{}, nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ContentFlag{}, nil, false, fmt.Errorf("store: auto-kill content flag: %w", err)
	}
	return resolved, banned, true, nil
}

// ResolveFlagsForKill resolves the broadcast's unresolved flag-mode flag as
// killed — an operator's kill through R39 (D9). Observed flags are left to the
// session-end sweep: the kill ends the session, and `ended` is their answer.
func (s *Store) ResolveFlagsForKill(ctx context.Context, broadcastID, actor string, now time.Time, config []ConfigWebhook) ([]ContentFlag, error) {
	flags, err := s.pool.Query(ctx, `SELECT `+flagColumns+` FROM content_flags
		WHERE broadcast_id = $1 AND mode = 'flag' AND state <> 'resolved'`, broadcastID)
	if err != nil {
		return nil, fmt.Errorf("store: flags for kill: %w", err)
	}
	list, err := collectFlags(flags)
	if err != nil {
		return nil, fmt.Errorf("store: flags for kill: %w", err)
	}
	var out []ContentFlag
	for _, f := range list {
		r, ok, err := s.ResolveContentFlag(ctx, f, events.ContentFlagResolutionKilled, "", actor, now, config)
		if err != nil {
			return out, err
		}
		if ok {
			out = append(out, r)
		}
	}
	return out, nil
}

// RemindContentFlag re-pages an open flag-mode flag whose last page is at
// least repage old (D9), with content_flag.reminded. The condition is in the
// UPDATE, so a leadership handover that briefly runs two sweeps sends one
// reminder, not two. ok is false when the flag was not due.
func (s *Store) RemindContentFlag(ctx context.Context, id int64, signal string, repage time.Duration, now time.Time, config []ConfigWebhook) (ContentFlag, bool, error) {
	now = now.UTC()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ContentFlag{}, false, fmt.Errorf("store: remind content flag: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	f, err := scanFlag(tx.QueryRow(ctx, `UPDATE content_flags SET
		reminders = reminders + 1, last_reminded_at = $2
		WHERE id = $1 AND state = 'open' AND mode = 'flag' AND COALESCE(last_reminded_at, raised_at) <= $3
		RETURNING `+flagColumns, id, now, now.Add(-repage)))
	if errors.Is(err, pgx.ErrNoRows) {
		return ContentFlag{}, false, nil
	}
	if err != nil {
		return ContentFlag{}, false, fmt.Errorf("store: remind content flag: %w", err)
	}
	payload := flagPayload(f, SummarizeContentFlagReminded(f.BroadcastID, f.Reminders, now.Sub(f.RaisedAt), signal), map[string]any{
		PayloadReminder: f.Reminders,
		PayloadSignal:   signal,
		PayloadRaisedAt: rfc3339(f.RaisedAt),
	})
	if _, err := s.appendEventTx(ctx, tx, Event{
		Type: EventContentFlagReminded, OccurredAt: now, Actor: "system",
		BroadcastKey: f.BroadcastKey, BroadcastID: f.BroadcastID, Payload: mustJSON(payload),
	}, config); err != nil {
		return ContentFlag{}, false, fmt.Errorf("store: remind content flag: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return ContentFlag{}, false, fmt.Errorf("store: remind content flag: %w", err)
	}
	return f, true, nil
}

// ActiveMutes returns every broadcast muted at now, with its mute. A caller
// asks Mute.Covers whether it silences a given publisher session.
func (s *Store) ActiveMutes(ctx context.Context, now time.Time) (map[string]Mute, error) {
	rows, err := s.pool.Query(ctx, `SELECT broadcast_id, muted_until, scope, publisher_epoch FROM screening_mutes
		WHERE muted_until > $1`, now.UTC())
	if err != nil {
		return nil, fmt.Errorf("store: active mutes: %w", err)
	}
	defer rows.Close()
	out := map[string]Mute{}
	for rows.Next() {
		var (
			id    string
			m     Mute
			epoch *string
		)
		if err := rows.Scan(&id, &m.Until, &m.Scope, &epoch); err != nil {
			return nil, fmt.Errorf("store: active mutes: %w", err)
		}
		m.Until, m.Epoch = m.Until.UTC(), derefString(epoch)
		out[id] = m
	}
	return out, rows.Err()
}

// ResolveHeldClears is the leader's half of docs/72 D3: every unresolved flag
// whose clear has been held for longer than rejoin — or that a person
// acknowledged meanwhile, which needs no window — resolves as cleared, with
// R40's content_flag.resolved for a flag-mode one. A rejoin of zero resolves
// every held clear, which is what switching the window off must do.
func (s *Store) ResolveHeldClears(ctx context.Context, rejoin time.Duration, now time.Time, config []ConfigWebhook) ([]ContentFlag, error) {
	now = now.UTC()
	rows, err := s.pool.Query(ctx, `SELECT `+flagColumns+` FROM content_flags
		WHERE state <> 'resolved' AND cleared_at IS NOT NULL AND (state = 'acknowledged' OR cleared_at <= $1)
		ORDER BY id`, now.Add(-rejoin))
	if err != nil {
		return nil, fmt.Errorf("store: held clears: %w", err)
	}
	list, err := collectFlags(rows)
	if err != nil {
		return nil, fmt.Errorf("store: held clears: %w", err)
	}
	var out []ContentFlag
	for _, f := range list {
		r, ok, err := s.resolveHeldClear(ctx, f.ID, rejoin, now, config)
		if err != nil {
			return out, err
		}
		if ok {
			out = append(out, r)
		}
	}
	return out, nil
}

// resolveHeldClear re-reads the flag under its lock: a trip that rejoined it
// since the list was read wins, and nothing is resolved.
func (s *Store) resolveHeldClear(ctx context.Context, id int64, rejoin time.Duration, now time.Time, config []ConfigWebhook) (ContentFlag, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ContentFlag{}, false, fmt.Errorf("store: held clear: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	f, err := scanFlag(tx.QueryRow(ctx, `SELECT `+flagColumns+` FROM content_flags WHERE id = $1 FOR UPDATE`, id))
	if err != nil {
		return ContentFlag{}, false, fmt.Errorf("store: held clear: %w", err)
	}
	if !f.Unresolved() || f.ClearedAt == nil || (f.State != FlagStateAcknowledged && now.Sub(*f.ClearedAt) < rejoin) {
		return f, false, nil
	}
	out, ok, err := s.resolveFlagTx(ctx, tx, f, events.ContentFlagResolutionCleared, "", "system", now, config)
	if err != nil || !ok {
		return out, ok, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ContentFlag{}, false, fmt.Errorf("store: held clear: %w", err)
	}
	return out, true, nil
}

// FlagRoute reports whether any enabled webhook — chart-defined or
// UI-created, through its R49 filter — would deliver content_flag.raised
// (D12). False means a flag would reach nobody, and flag mode pauses.
func (s *Store) FlagRoute(ctx context.Context, config []ConfigWebhook) (bool, error) {
	raised := Event{Type: EventContentFlag}
	seen := map[string]bool{}
	for _, h := range config {
		seen[h.Name] = true
		if WebhookWants(h.Events, raised) {
			return true, nil
		}
	}
	rows, err := s.pool.Query(ctx, `SELECT name, events FROM webhooks WHERE enabled = true`)
	if err != nil {
		return false, fmt.Errorf("store: flag route: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			name   string
			filter []string
		)
		if err := rows.Scan(&name, &filter); err != nil {
			return false, fmt.Errorf("store: flag route: %w", err)
		}
		// A UI row shadowed by a chart-defined name never receives anything
		// of its own (the config-wins rule, docs/42 D9).
		if !seen[name] && WebhookWants(filter, raised) {
			return true, nil
		}
	}
	return false, rows.Err()
}

// ContentFlagPayload is a flag event's payload as a delivery may read it.
//
// Every field is read through a closed vocabulary or a shape check, the
// EnforcementState rule: whatever a producer wrote, a resolution can only be
// one of five words, a label only [A-Z0-9_], a score only 0–1. That is what
// makes it safe to forward without reviewing every producer.
type ContentFlagPayload struct {
	FlagID     int64
	SamplerID  string
	Window     *events.ContentFlagWindow
	Scores     *events.ContentFlagScores
	Labels     []string
	Resolution string
	Reason     string
	RaisedAt   string
	Reminder   int
	Signal     string
	// ReturnedAfterReview is docs/72 D4's mark on a raise.
	ReturnedAfterReview bool
}

// ContentFlag decodes the flag payload of a content-flag event.
func (e Event) ContentFlag() ContentFlagPayload {
	var raw struct {
		FlagID     json.Number               `json:"flagId"`
		SamplerID  string                    `json:"samplerId"`
		Window     *events.ContentFlagWindow `json:"window"`
		Scores     *events.ContentFlagScores `json:"scores"`
		Labels     []string                  `json:"labels"`
		Resolution string                    `json:"resolution"`
		Reason     string                    `json:"reason"`
		RaisedAt   string                    `json:"raisedAt"`
		Reminder   json.Number               `json:"reminder"`
		Signal     string                    `json:"signal"`
		Returned   bool                      `json:"returnedAfterReview"`
	}
	if len(e.Payload) == 0 || json.Unmarshal(e.Payload, &raw) != nil {
		return ContentFlagPayload{}
	}
	var out ContentFlagPayload
	if id, err := strconv.ParseInt(raw.FlagID.String(), 10, 64); err == nil && id > 0 {
		out.FlagID = id
	}
	// A sampler is named by its pod. One whose ID parses as an address is
	// refused outright: no event carries an IP, whoever wrote it (docs/42 D8).
	if _, err := netip.ParseAddr(raw.SamplerID); err != nil && len(raw.SamplerID) <= screening.MaxSamplerIDLen && printable(raw.SamplerID) {
		out.SamplerID = raw.SamplerID
	}
	if w := raw.Window; w != nil && w.Samples >= 0 && w.FlaggedSamples >= 0 && w.FlaggedSamples <= w.Samples {
		out.Window = &events.ContentFlagWindow{Start: timestampOrEmpty(w.Start), End: timestampOrEmpty(w.End),
			Samples: w.Samples, FlaggedSamples: w.FlaggedSamples}
	}
	if sc := raw.Scores; sc != nil && unit(sc.Max) && unit(sc.Mean) {
		out.Scores = &events.ContentFlagScores{Max: sc.Max, Mean: sc.Mean}
	}
	for _, l := range raw.Labels {
		if screening.ValidateLabels([]string{l}) == nil {
			out.Labels = append(out.Labels, l)
		}
	}
	switch raw.Resolution {
	case events.ContentFlagResolutionCleared, events.ContentFlagResolutionDismissed, events.ContentFlagResolutionKilled,
		events.ContentFlagResolutionAutoKilled, events.ContentFlagResolutionEnded:
		out.Resolution = raw.Resolution
	}
	switch raw.Reason {
	case FlagReasonDeadline, FlagReasonPolicy, FlagReasonStale,
		DismissReasonGameScene, DismissReasonNotExplicit, DismissReasonDetectorError, DismissReasonOther:
		out.Reason = raw.Reason
	}
	out.ReturnedAfterReview = raw.Returned
	out.RaisedAt = timestampOrEmpty(raw.RaisedAt)
	if n, err := strconv.Atoi(raw.Reminder.String()); err == nil && n > 0 {
		out.Reminder = n
	}
	switch raw.Signal {
	case events.ContentFlagSignalLive, events.ContentFlagSignalUnknown:
		out.Signal = raw.Signal
	}
	return out
}

// RoomScreening reads a room.screening_changed event's value through its
// closed vocabulary: exempt or screened, "" for anything else.
func (e Event) RoomScreening() string {
	switch v := e.PayloadString(PayloadScreening); v {
	case events.RoomScreeningExempt, events.RoomScreeningScreened:
		return v
	}
	return ""
}

func printable(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] <= 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

func unit(v float64) bool { return v >= 0 && v <= 1 }

func timestampOrEmpty(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// Payload keys of the R40 rows. Like the others, a delivery reads them
// through accessors (ContentFlag, RoomScreening) and nothing else.
const (
	PayloadFlagID     = "flagId"
	PayloadSamplerID  = "samplerId"
	PayloadWindow     = "window"
	PayloadScores     = "scores"
	PayloadLabels     = "labels"
	PayloadResolution = "resolution"
	PayloadRaisedAt   = "raisedAt"
	PayloadReminder   = "reminder"
	PayloadSignal     = "signal"
	PayloadScreening  = "screening"
	// PayloadReturnedAfterReview marks a raise after a reviewed incident
	// (docs/72 D4).
	PayloadReturnedAfterReview = "returnedAfterReview"
)
