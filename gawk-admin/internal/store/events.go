package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/Tuhis/gawk/gawk-server/moderation"
)

const eventColumns = `id, type, occurred_at, actor, broadcast_key, broadcast_id, payload`

// defaultEventLimit / maxEventLimit bound the audit feed page size. A caller
// asking for a million rows gets maxEventLimit, not an OOM.
const (
	defaultEventLimit = 50
	maxEventLimit     = 500
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

// ListEvents returns the feed newest-first. afterID is the cursor: 0 starts at
// the newest event, otherwise only events strictly OLDER than that ID are
// returned — "after" in feed order, which is descending ID.
func (s *Store) ListEvents(ctx context.Context, afterID int64, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = defaultEventLimit
	}
	if limit > maxEventLimit {
		limit = maxEventLimit
	}
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
// templating (docs/42 §4.10's `summary` field).
//
// It is a SECURITY-relevant helper, not a formatting convenience: `summary` is
// one of the two payload fields AP7 copies into a webhook body, so it must
// never name a raw broadcast ID or an IP address (D8). It therefore takes the
// HMAC'd broadcast key — never the ID — and says only what KIND of target a
// ban covers, never its value.
func Summarize(eventType string, targetType moderation.TargetType, broadcastKey, actor string) string {
	what := "broadcast"
	if targetType == moderation.TargetIP {
		what = "publisher IP"
	}
	switch eventType {
	case EventBroadcastKilled:
		subject := "a broadcast"
		if broadcastKey != "" {
			subject = "broadcast " + broadcastKey
		}
		return subject + " was terminated by " + actorOrOperator(actor)
	case EventBanCreated:
		return "a " + what + " ban was created by " + actorOrOperator(actor)
	case EventBanExpired:
		return "a " + what + " ban expired"
	case EventBanRemoved:
		return "a " + what + " ban was lifted by " + actorOrOperator(actor)
	default:
		return eventType
	}
}

func actorOrOperator(actor string) string {
	if actor == "" {
		return "an operator"
	}
	return actor
}
