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

// ActiveBanForTarget returns the active ban covering an exact target, if any.
// The target is normalized first, so "203.0.113.7" and "203.0.113.7/32" ask
// the same question. ErrNotFound when nothing active covers it.
//
// This is an EXACT-target lookup, not an evaluation: it answers "is this
// target already banned by name?" for the 409 paths. Whether a given IP falls
// inside some broader CIDR ban is moderation.Set's question, on the relay.
func (s *Store) ActiveBanForTarget(ctx context.Context, t moderation.Target) (Ban, error) {
	rec, err := moderation.Normalize(moderation.Record{Target: t})
	if err != nil {
		return Ban{}, err
	}
	row := s.pool.QueryRow(ctx, `SELECT `+banColumns+`
		FROM bans WHERE state = 'active' AND target_type = $1 AND target_value = $2`,
		string(rec.Target.Type), rec.Target.Value)
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
func (s *Store) RemoveBan(ctx context.Context, id uuid.UUID, by string) (Ban, error) {
	const q = `UPDATE bans SET state = 'removed', removed_at = $2, removed_by = $3
		WHERE id = $1 AND state = 'active' RETURNING ` + banColumns
	row := s.pool.QueryRow(ctx, q, id, s.now().UTC(), by)
	b, err := scanBan(row)
	if err == nil {
		return b, nil
	}
	if !errors.Is(noRows(err), ErrNotFound) {
		return Ban{}, fmt.Errorf("store: remove ban: %w", err)
	}
	// The UPDATE matched nothing: either no such ban, or it was not active.
	existing, err := s.GetBan(ctx, id)
	if err != nil {
		return Ban{}, err
	}
	if existing.State == BanRemoved {
		return existing, nil
	}
	return existing, ErrNotActive
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
	rows, err := s.pool.Query(ctx, q, now.UTC())
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
