package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/Tuhis/gawk/gawk-server/moderation"
)

const eventColumns = `id, type, occurred_at, actor, broadcast_key, broadcast_id, payload`

// DefaultEventLimit / MaxEventLimit bound the audit feed page size. A caller
// asking for a million rows gets MaxEventLimit, not an OOM.
//
// They are EXPORTED because the handler has to page by the same numbers: a
// cursor decided against the limit the caller asked for, while the rows were
// cut to the limit applied here, reports "no more pages" on a truncated feed
// and makes the remainder unreachable. One pair of constants, one clamp rule.
const (
	DefaultEventLimit = 50
	MaxEventLimit     = 500
)

// AppendEvent writes one audit/notification event and returns it with its
// assigned ID — which is also the cursor the portal feed pages by, and the
// foreign key every webhook delivery hangs off.
func (s *Store) AppendEvent(ctx context.Context, e Event) (Event, error) {
	if e.OccurredAt.IsZero() {
		e.OccurredAt = s.now()
	}
	e.OccurredAt = e.OccurredAt.UTC()
	payload := e.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	const q = `INSERT INTO moderation_events (type, occurred_at, actor, broadcast_key, broadcast_id, payload)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING ` + eventColumns
	row := s.pool.QueryRow(ctx, q, e.Type, e.OccurredAt, e.Actor,
		nullString(e.BroadcastKey), nullString(e.BroadcastID), []byte(payload))
	out, err := scanEvent(row)
	if err != nil {
		return Event{}, fmt.Errorf("store: append event: %w", err)
	}
	return out, nil
}

// AppendEventAndEnqueue writes one event AND its per-webhook delivery rows in
// a single transaction.
//
// It exists to close the AppendEvent → EnqueueDeliveries window: the two
// writes land in the same Postgres, and separately they left a gap in which a
// crash — or a transient error on the second call, which had no retry, since
// deliveries are only ever enqueued from the recording path — lost that one
// event's webhook fan-out while the event claimed to be recorded. §4.10's "a
// failed delivery must be seen" (and R40's "a flag must reach a human")
// inherit this pipe, so the two writes commit together or not at all.
//
// configNames are the enabled CHART-defined webhooks, which are not rows here;
// the enabled UI-created set is read INSIDE the transaction so a concurrent
// webhook edit cannot split the decision from the write. A UI name shadowed by
// a config name yields one delivery row (the queue is keyed by name), which
// the dispatcher's resolve() signs with the config secret — the same
// config-wins rule notify applies at send time (docs/42 D9).
func (s *Store) AppendEventAndEnqueue(ctx context.Context, e Event, configNames []string) (Event, error) {
	if e.OccurredAt.IsZero() {
		e.OccurredAt = s.now()
	}
	e.OccurredAt = e.OccurredAt.UTC()
	payload := e.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Event{}, fmt.Errorf("store: append event: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const q = `INSERT INTO moderation_events (type, occurred_at, actor, broadcast_key, broadcast_id, payload)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING ` + eventColumns
	out, err := scanEvent(tx.QueryRow(ctx, q, e.Type, e.OccurredAt, e.Actor,
		nullString(e.BroadcastKey), nullString(e.BroadcastID), []byte(payload)))
	if err != nil {
		return Event{}, fmt.Errorf("store: append event: %w", err)
	}

	seen := make(map[string]struct{}, len(configNames))
	names := make([]string, 0, len(configNames))
	for _, n := range configNames {
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		names = append(names, n)
	}
	rows, err := tx.Query(ctx, `SELECT name FROM webhooks WHERE enabled = true`)
	if err != nil {
		return Event{}, fmt.Errorf("store: append event: list webhooks: %w", err)
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return Event{}, fmt.Errorf("store: append event: list webhooks: %w", err)
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		names = append(names, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Event{}, fmt.Errorf("store: append event: list webhooks: %w", err)
	}

	now := s.now().UTC()
	for _, name := range names {
		if _, err := tx.Exec(ctx, `INSERT INTO webhook_deliveries (event_id, webhook_name, state, attempts, next_attempt_at)
			VALUES ($1,$2,'pending',0,$3) ON CONFLICT (event_id, webhook_name) DO NOTHING`,
			out.ID, name, now); err != nil {
			return Event{}, fmt.Errorf("store: append event: enqueue deliveries: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return Event{}, fmt.Errorf("store: append event: %w", err)
	}
	return out, nil
}

// ClampEventLimit is the page-size rule, in one place so a caller computing a
// pagination cursor and this package cutting the rows cannot disagree.
func ClampEventLimit(limit int) int {
	if limit <= 0 {
		return DefaultEventLimit
	}
	if limit > MaxEventLimit {
		return MaxEventLimit
	}
	return limit
}

// ListEvents returns the feed newest-first. afterID is the cursor: 0 starts at
// the newest event, otherwise only events strictly OLDER than that ID are
// returned — "after" in feed order, which is descending ID.
func (s *Store) ListEvents(ctx context.Context, afterID int64, limit int) ([]Event, error) {
	limit = ClampEventLimit(limit)
	const q = `SELECT ` + eventColumns + ` FROM moderation_events
		WHERE ($1 = 0 OR id < $1) ORDER BY id DESC LIMIT $2`
	rows, err := s.pool.Query(ctx, q, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list events: %w", err)
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list events: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetEvent returns one event by ID.
func (s *Store) GetEvent(ctx context.Context, id int64) (Event, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+eventColumns+` FROM moderation_events WHERE id = $1`, id)
	e, err := scanEvent(row)
	if err != nil {
		return Event{}, noRows(err)
	}
	return e, nil
}

func scanEvent(row pgx.Row) (Event, error) {
	var (
		e       Event
		key     *string
		id      *string
		payload []byte
	)
	if err := row.Scan(&e.ID, &e.Type, &e.OccurredAt, &e.Actor, &key, &id, &payload); err != nil {
		return Event{}, err
	}
	e.BroadcastKey = derefString(key)
	e.BroadcastID = derefString(id)
	e.OccurredAt = e.OccurredAt.UTC()
	if len(payload) > 0 {
		e.Payload = json.RawMessage(payload)
	}
	return e, nil
}

// Summarize is the one human sentence a webhook receiver can render without
// templating (docs/42 §4.10's `summary` field), for an event whose enforcement
// object is in step with the record.
//
// It is a SECURITY-relevant helper, not a formatting convenience: `summary` is
// one of the payload fields AP7 copies into a webhook body, so it must never
// name a raw broadcast ID or an IP address (D8). It therefore takes the
// HMAC'd broadcast key — never the ID — and says only what KIND of target a
// ban covers, never its value.
//
// Every producer that does NOT project a CR inline is in-sync by construction,
// which is why this shorter form exists rather than making every caller say
// so: internal/kube's adoption path records a ban whose CR is the very object
// it adopted, and its expiry path records a ban the relays have already
// stopped enforcing against their own clocks (§4.2). internal/api, the one
// producer that writes a row and a CR in the same request, uses
// SummarizeWithEnforcement and grades on which of the two landed.
func Summarize(eventType string, targetType moderation.TargetType, broadcastKey, actor string) string {
	return SummarizeWithEnforcement(eventType, targetType, broadcastKey, actor, EnforcementInSync)
}

// SummarizeWithEnforcement is that same one sentence, graded on whether the
// enforcement object that MAKES the event true has been written yet.
//
// This is the whole reason the grading reaches the summary at all: an event is
// a statement of something that happened, and "a broadcast was terminated" is
// not a true statement when nothing has been terminated. `summary` is the part
// a dumb webhook-to-push bridge shows on a phone with no templating, so it is
// the part that must not overstate — a pending kill says the kill was
// RECORDED, and a pending removal says the target is STILL banned.
//
// It is the single source of the sentence in both grades; Summarize delegates
// here rather than growing a second copy, because two summarisers is exactly
// how a pending sentence and an in-sync one drift apart.
func SummarizeWithEnforcement(eventType string, targetType moderation.TargetType, broadcastKey, actor string, enforcement EnforcementState) string {
	what := "broadcast"
	if targetType == moderation.TargetIP {
		what = "publisher IP"
	}
	who := actorOrOperator(actor)
	pending := enforcement == EnforcementPending
	switch eventType {
	case EventBroadcastKilled:
		subject := "a broadcast"
		if broadcastKey != "" {
			subject = "broadcast " + broadcastKey
		}
		if pending {
			// The verb moves from "was terminated" to "a kill … was recorded"
			// on purpose: the recording is the only part that happened.
			return "a kill of " + subject + " was recorded by " + who +
				" — NOT enforced yet, the broadcast is still live"
		}
		return subject + " was terminated by " + who
	case EventBanCreated:
		if pending {
			return "a " + what + " ban was recorded by " + who + " — NOT enforced yet"
		}
		return "a " + what + " ban was created by " + who
	case EventBanExpired:
		if pending {
			return "a " + what + " ban expired in the record — the target is STILL banned"
		}
		return "a " + what + " ban expired"
	case EventBanRemoved:
		if pending {
			return "a " + what + " ban was lifted in the record by " + who +
				" — the target is STILL banned"
		}
		return "a " + what + " ban was lifted by " + who
	default:
		// An unknown type gets a DIRECTION-FREE qualifier: with no idea
		// whether the event asserts a ban or its lifting, "not enforced yet"
		// could be the backwards half. Saying less is the safe failure.
		if pending {
			return eventType + " — enforcement pending"
		}
		return eventType
	}
}

func actorOrOperator(actor string) string {
	if actor == "" {
		return "an operator"
	}
	return actor
}
