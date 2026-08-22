package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Tuhis/gawk/gawk-server/moderation"
)

const banColumns = `id, target_type, target_value, state, reason, created_at, created_by,
	expires_at, removed_at, removed_by, source_broadcast_id, cr_name`

// CreateBan inserts an active ban.
//
// The target is normalized and the CR name derived HERE rather than by the
// caller, so every row in the table is addressable by the same rules the relay
// evaluates and the reconciler projects (D13). A caller that hand-rolled
// either would be the start of the two-implementations-disagree bug the shared
// package exists to prevent.
//
// An active ban already covering the target is ErrDuplicateActive — from the
// partial unique index, not from a read-then-write check, so two replicas
// racing produce one ban and one 409 rather than two bans.
func (s *Store) CreateBan(ctx context.Context, b Ban) (Ban, error) {
	rec, err := moderation.Normalize(b.Record())
	if err != nil {
		return Ban{}, err
	}
	crName, err := moderation.CRName(rec.Target)
	if err != nil {
		return Ban{}, err
	}
	b.Target = rec.Target
	b.CRName = crName
	b.State = BanActive
	if b.ID == uuid.Nil {
		b.ID = uuid.New()
	}
	if b.CreatedAt.IsZero() {
		b.CreatedAt = s.now()
	}
	b.CreatedAt = b.CreatedAt.UTC()

	const q = `INSERT INTO bans (` + banColumns + `)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NULL,NULL,$9,$10)
		RETURNING ` + banColumns
	row := s.pool.QueryRow(ctx, q,
		b.ID, string(b.Target.Type), b.Target.Value, string(b.State), b.Reason,
		b.CreatedAt, b.CreatedBy, b.ExpiresAt, nullString(b.SourceBroadcastID), b.CRName)
	out, err := scanBan(row)
	if isUniqueViolation(err, "bans_one_active_per_target") {
		return Ban{}, ErrDuplicateActive
	}
	if err != nil {
		return Ban{}, fmt.Errorf("store: create ban: %w", err)
	}
	return out, nil
}

// ListBans returns bans newest first. state is FilterActive or FilterAll;
// anything else is rejected rather than silently widened — "?state=activee"
// must not quietly list removed bans to the portal.
func (s *Store) ListBans(ctx context.Context, state string) ([]Ban, error) {
	var q string
	switch state {
	case FilterActive:
		q = `SELECT ` + banColumns + ` FROM bans WHERE state = 'active' ORDER BY created_at DESC, id DESC`
	case FilterAll, "":
		q = `SELECT ` + banColumns + ` FROM bans ORDER BY created_at DESC, id DESC`
	default:
		return nil, fmt.Errorf("store: list bans: unknown state filter %q", state)
	}
	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("store: list bans: %w", err)
	}
	defer rows.Close()
	out := []Ban{}
	for rows.Next() {
		b, err := scanBan(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list bans: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// GetBan returns one ban by ID.
func (s *Store) GetBan(ctx context.Context, id uuid.UUID) (Ban, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+banColumns+` FROM bans WHERE id = $1`, id)
	b, err := scanBan(row)
	if err != nil {
		return Ban{}, noRows(err)
	}
	return b, nil
}

// ActiveBanForTarget returns the ban IN FORCE on an exact target, if any.
// The target is normalized first, so "203.0.113.7" and "203.0.113.7/32" ask
// the same question. ErrNotFound when nothing in force covers it.
//
// This is an EXACT-target lookup, not an evaluation: it answers "is this
// target already banned by name?" for the 409 paths. Whether a given IP falls
// inside some broader CIDR ban is moderation.Set's question, on the relay.
//
// Expiry is evaluated HERE rather than left to the janitor's sweep, for the
// same reason Ban.Active does it: a relay stops enforcing the moment expiresAt
// passes, against its own clock (§4.2), so a row still marked `active` because
// nothing has swept it yet is not a ban — and reporting one would answer 409
// duplicate_active for a live, unenforced broadcast the operator is trying to
// re-kill. That window is up to a minute in the happy case and unbounded when
// no replica holds the leader Lease.
func (s *Store) ActiveBanForTarget(ctx context.Context, t moderation.Target) (Ban, error) {
	rec, err := moderation.Normalize(moderation.Record{Target: t})
	if err != nil {
		return Ban{}, err
	}
	row := s.pool.QueryRow(ctx, `SELECT `+banColumns+`
		FROM bans WHERE state = 'active' AND target_type = $1 AND target_value = $2
			AND (expires_at IS NULL OR expires_at > $3)`,
		string(rec.Target.Type), rec.Target.Value, s.now().UTC())
	b, err := scanBan(row)
	if err != nil {
		return Ban{}, noRows(err)
	}
	return b, nil
}

// RemoveBan is the unban path: active → removed, recording who and when.
//
// Idempotent for an already-removed ban (the row comes back unchanged) because
// a double-clicked Unban is not an error. An EXPIRED ban is ErrNotActive: the
// state machine is one-way and there is nothing left to lift.
//
// The second return says whether this call is the one that MADE the
// transition. Idempotence is only half an answer for a caller that emits an
// audit event and a signed webhook delivery per call: without it, a replayed
// unban writes a second ban.removed row and pages every receiver again under a
// distinct delivery ID, which receiver-side dedup cannot catch. Anything with
// a side effect per unban must gate on this, not on the error being nil.
func (s *Store) RemoveBan(ctx context.Context, id uuid.UUID, by string) (Ban, bool, error) {
	const q = `UPDATE bans SET state = 'removed', removed_at = $2, removed_by = $3
		WHERE id = $1 AND state = 'active' RETURNING ` + banColumns
	row := s.pool.QueryRow(ctx, q, id, s.now().UTC(), by)
	b, err := scanBan(row)
	if err == nil {
		return b, true, nil
	}
	if !errors.Is(noRows(err), ErrNotFound) {
		return Ban{}, false, fmt.Errorf("store: remove ban: %w", err)
	}
	// The UPDATE matched nothing: either no such ban, or it was not active.
	// Whichever it is, this call moved nothing — the statement is the single
	// point where two replicas racing one unban are decided, so exactly one of
	// them can be told it made the transition.
	existing, err := s.GetBan(ctx, id)
	if err != nil {
		return Ban{}, false, err
	}
	if existing.State == BanRemoved {
		return existing, false, nil
	}
	return existing, false, ErrNotActive
}

// ExpireDueBans flips every active ban whose expiry has passed to `expired`
// and returns the rows that moved, so the caller can delete their CRs and emit
// one ban.expired event each.
//
// It is the janitor's whole write path, and it is a single statement on
// purpose: two leaders overlapping during a handover both run it, and only one
// of them sees any given row in its RETURNING set. Nothing double-emits.
func (s *Store) ExpireDueBans(ctx context.Context, now time.Time) ([]Ban, error) {
	const q = `UPDATE bans SET state = 'expired'
		WHERE state = 'active' AND expires_at IS NOT NULL AND expires_at <= $1
		RETURNING ` + banColumns
	return s.expire(ctx, q, now.UTC())
}

// ExpireLapsedBansForTarget is that same sweep narrowed to ONE target, so a
// mutation can clear a lapsed row out of its own way rather than waiting for
// the janitor.
//
// It exists because the two gates disagree by construction: ActiveBanForTarget
// evaluates expiry the way a relay does, but the partial unique index behind
// ErrDuplicateActive can only know `state = 'active'` (an index predicate has
// to be immutable, so it cannot compare against now()). Without this, a kill
// whose cooldown has lapsed collides with its own predecessor until the next
// 60 s sweep — and indefinitely whenever no replica holds the leader Lease.
//
// Single statement with RETURNING for exactly the reason ExpireDueBans is:
// whoever gets there first, janitor or handler, is the only one that sees the
// row, so ban.expired is emitted once.
func (s *Store) ExpireLapsedBansForTarget(ctx context.Context, t moderation.Target, now time.Time) ([]Ban, error) {
	rec, err := moderation.Normalize(moderation.Record{Target: t})
	if err != nil {
		return nil, err
	}
	const q = `UPDATE bans SET state = 'expired'
		WHERE state = 'active' AND target_type = $1 AND target_value = $2
			AND expires_at IS NOT NULL AND expires_at <= $3
		RETURNING ` + banColumns
	return s.expire(ctx, q, string(rec.Target.Type), rec.Target.Value, now.UTC())
}

func (s *Store) expire(ctx context.Context, q string, args ...any) ([]Ban, error) {
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: expire bans: %w", err)
	}
	defer rows.Close()
	out := []Ban{}
	for rows.Next() {
		b, err := scanBan(rows)
		if err != nil {
			return nil, fmt.Errorf("store: expire bans: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func scanBan(row pgx.Row) (Ban, error) {
	var (
		b        Ban
		typ      string
		state    string
		removeBy *string
		srcID    *string
	)
	err := row.Scan(&b.ID, &typ, &b.Target.Value, &state, &b.Reason, &b.CreatedAt, &b.CreatedBy,
		&b.ExpiresAt, &b.RemovedAt, &removeBy, &srcID, &b.CRName)
	if err != nil {
		return Ban{}, err
	}
	b.Target.Type = moderation.TargetType(typ)
	b.State = BanState(state)
	b.RemovedBy = derefString(removeBy)
	b.SourceBroadcastID = derefString(srcID)
	b.CreatedAt = b.CreatedAt.UTC()
	if b.ExpiresAt != nil {
		t := b.ExpiresAt.UTC()
		b.ExpiresAt = &t
	}
	if b.RemovedAt != nil {
		t := b.RemovedAt.UTC()
		b.RemovedAt = &t
	}
	return b, nil
}

func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
