package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const deliveryColumns = `id, event_id, webhook_name, state, attempts, next_attempt_at, last_error, delivered_at`

// ClaimLease is how long a claimed delivery is invisible to other dispatchers
// before it becomes due again.
//
// It is the crash guard: SKIP LOCKED only protects a row for the life of the
// claiming TRANSACTION, so a dispatcher that dies between claiming and sending
// would otherwise strand the row forever. Five minutes is long enough that a
// live dispatcher never races itself and short enough that a pod crash costs
// one delayed notification, not a lost one.
const ClaimLease = 5 * time.Minute

// EnqueueDeliveries queues one pending delivery per webhook name for an event.
//
// Idempotent: the (event_id, webhook_name) unique index plus ON CONFLICT DO
// NOTHING means a retried enqueue — after a crash between AppendEvent and this
// call — adds nothing the first attempt already created.
func (s *Store) EnqueueDeliveries(ctx context.Context, eventID int64, webhookNames []string) error {
	if len(webhookNames) == 0 {
		return nil
	}
	now := s.now().UTC()
	batch := &pgx.Batch{}
	for _, name := range webhookNames {
		batch.Queue(`INSERT INTO webhook_deliveries (event_id, webhook_name, state, attempts, next_attempt_at)
			VALUES ($1,$2,'pending',0,$3) ON CONFLICT (event_id, webhook_name) DO NOTHING`,
			eventID, name, now)
	}
	if err := s.pool.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("store: enqueue deliveries: %w", err)
	}
	return nil
}

// ClaimDueDeliveries takes up to limit due deliveries for this dispatcher.
//
// FOR UPDATE SKIP LOCKED is the whole reason a leadership handover cannot
// double-send (docs/42 §4.10, D16): a second dispatcher running the same query
// concurrently skips the rows this one holds instead of blocking on them or
// duplicating them.
//
// Attempts is incremented on CLAIM, not on completion: a send that never
// reported back still consumed an attempt, and counting only successes would
// let a dispatcher that dies mid-send retry forever. The returned rows already
// carry the incremented count, which is what AP7's retry schedule keys on.
func (s *Store) ClaimDueDeliveries(ctx context.Context, now time.Time, limit int) ([]Delivery, error) {
	if limit <= 0 {
		limit = 1
	}
	now = now.UTC()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: claim deliveries: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const sel = `SELECT id FROM webhook_deliveries
		WHERE state = 'pending' AND next_attempt_at IS NOT NULL AND next_attempt_at <= $1
		ORDER BY next_attempt_at, id
		LIMIT $2 FOR UPDATE SKIP LOCKED`
	rows, err := tx.Query(ctx, sel, now, limit)
	if err != nil {
		return nil, fmt.Errorf("store: claim deliveries: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: claim deliveries: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: claim deliveries: %w", err)
	}
	if len(ids) == 0 {
		return nil, tx.Commit(ctx)
	}

	const upd = `UPDATE webhook_deliveries
		SET attempts = attempts + 1, next_attempt_at = $2
		WHERE id = ANY($1) RETURNING ` + deliveryColumns
	claimed, err := scanDeliveries(tx.Query(ctx, upd, ids, now.Add(ClaimLease)))
	if err != nil {
		return nil, fmt.Errorf("store: claim deliveries: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("store: claim deliveries: %w", err)
	}
	return claimed, nil
}

// MarkDelivered records a successful send.
func (s *Store) MarkDelivered(ctx context.Context, id int64, at time.Time) error {
	tag, err := s.pool.Exec(ctx, `UPDATE webhook_deliveries
		SET state = 'delivered', delivered_at = $2, next_attempt_at = NULL, last_error = NULL
		WHERE id = $1`, id, at.UTC())
	return execOne(tag.RowsAffected(), err, "mark delivered")
}

// ScheduleRetry leaves the delivery pending and due again at nextAttempt.
func (s *Store) ScheduleRetry(ctx context.Context, id int64, nextAttempt time.Time, lastErr string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE webhook_deliveries
		SET state = 'pending', next_attempt_at = $2, last_error = $3
		WHERE id = $1`, id, nextAttempt.UTC(), truncateErr(lastErr))
	return execOne(tag.RowsAffected(), err, "schedule retry")
}

// MarkDeliveryFailed is terminal: the retry budget is spent. The row stays
// visible in the portal's events view forever — "a failed delivery must be
// SEEN" (§4.10) is why nothing here deletes it.
func (s *Store) MarkDeliveryFailed(ctx context.Context, id int64, lastErr string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE webhook_deliveries
		SET state = 'failed', next_attempt_at = NULL, last_error = $2
		WHERE id = $1`, id, truncateErr(lastErr))
	return execOne(tag.RowsAffected(), err, "mark delivery failed")
}

// ListDeliveriesForEvents returns deliveries grouped by event ID — one query
// for a whole page of the events feed rather than one per row.
func (s *Store) ListDeliveriesForEvents(ctx context.Context, eventIDs []int64) (map[int64][]Delivery, error) {
	out := map[int64][]Delivery{}
	if len(eventIDs) == 0 {
		return out, nil
	}
	ds, err := scanDeliveries(s.pool.Query(ctx, `SELECT `+deliveryColumns+`
		FROM webhook_deliveries WHERE event_id = ANY($1) ORDER BY id`, eventIDs))
	if err != nil {
		return nil, fmt.Errorf("store: list deliveries: %w", err)
	}
	for _, d := range ds {
		out[d.EventID] = append(out[d.EventID], d)
	}
	return out, nil
}

func scanDeliveries(rows pgx.Rows, queryErr error) ([]Delivery, error) {
	if queryErr != nil {
		return nil, queryErr
	}
	defer rows.Close()
	out := []Delivery{}
	for rows.Next() {
		var (
			d     Delivery
			state string
			last  *string
		)
		if err := rows.Scan(&d.ID, &d.EventID, &d.WebhookName, &state, &d.Attempts,
			&d.NextAttemptAt, &last, &d.DeliveredAt); err != nil {
			return nil, err
		}
		d.State = DeliveryState(state)
		d.LastError = derefString(last)
		if d.NextAttemptAt != nil {
			t := d.NextAttemptAt.UTC()
			d.NextAttemptAt = &t
		}
		if d.DeliveredAt != nil {
			t := d.DeliveredAt.UTC()
			d.DeliveredAt = &t
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func execOne(affected int64, err error, what string) error {
	if err != nil {
		return fmt.Errorf("store: %s: %w", what, err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// truncateErr bounds what a misbehaving receiver can write into the database:
// last_error is displayed in the portal, and an endpoint returning a megabyte
// of HTML should not become a megabyte row.
func truncateErr(s string) string {
	const max = 1024
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
