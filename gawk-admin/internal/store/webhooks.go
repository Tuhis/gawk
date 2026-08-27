package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ListWebhooks returns every UI-created webhook, newest first.
//
// The secret column is NOT selected. That is the mechanism behind "secrets are
// never returned by the API" (§4.7): a handler rendering this list cannot leak
// a value it was never handed, no matter how it marshals. The dispatcher uses
// GetWebhookByName when it actually needs to sign.
func (s *Store) ListWebhooks(ctx context.Context) ([]Webhook, error) {
	const q = `SELECT id, name, url, enabled, created_at, created_by
		FROM webhooks ORDER BY created_at DESC, name ASC`
	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("store: list webhooks: %w", err)
	}
	defer rows.Close()
	out := []Webhook{}
	for rows.Next() {
		var w Webhook
		if err := rows.Scan(&w.ID, &w.Name, &w.URL, &w.Enabled, &w.CreatedAt, &w.CreatedBy); err != nil {
			return nil, fmt.Errorf("store: list webhooks: %w", err)
		}
		w.CreatedAt = w.CreatedAt.UTC()
		out = append(out, w)
	}
	return out, rows.Err()
}

// CreateWebhook inserts a UI-created webhook. ErrDuplicateName on a name
// collision with another UI webhook; collisions with CONFIG-sourced names are
// the API's to reject (409 source_immutable), because config webhooks are not
// rows here and the database cannot see them.
func (s *Store) CreateWebhook(ctx context.Context, w Webhook) (Webhook, error) {
	if w.ID == uuid.Nil {
		w.ID = uuid.New()
	}
	if w.CreatedAt.IsZero() {
		w.CreatedAt = s.now()
	}
	w.CreatedAt = w.CreatedAt.UTC()
	const q = `INSERT INTO webhooks (id, name, url, secret, enabled, created_at, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		RETURNING id, name, url, enabled, created_at, created_by`
	row := s.pool.QueryRow(ctx, q, w.ID, w.Name, w.URL, w.Secret, w.Enabled, w.CreatedAt, w.CreatedBy)
	out, err := scanWebhookNoSecret(row)
	if isUniqueViolation(err, "") {
		return Webhook{}, ErrDuplicateName
	}
	if err != nil {
		return Webhook{}, fmt.Errorf("store: create webhook: %w", err)
	}
	return out, nil
}

// UpdateWebhook replaces a UI-created webhook's name, URL and enabled flag.
//
// An empty Secret KEEPS the stored one: the API never returns a secret, so the
// portal's edit form cannot round-trip it, and requiring one on every edit
// would make "disable this webhook" impossible without re-typing the key.
func (s *Store) UpdateWebhook(ctx context.Context, w Webhook) (Webhook, error) {
	const q = `UPDATE webhooks
		SET name = $2, url = $3, enabled = $4,
		    secret = CASE WHEN $5::text = '' THEN secret ELSE $5::text END
		WHERE id = $1
		RETURNING id, name, url, enabled, created_at, created_by`
	row := s.pool.QueryRow(ctx, q, w.ID, w.Name, w.URL, w.Enabled, w.Secret)
	out, err := scanWebhookNoSecret(row)
	if isUniqueViolation(err, "") {
		return Webhook{}, ErrDuplicateName
	}
	if err != nil {
		return Webhook{}, noRows(err)
	}
	return out, nil
}

// DeleteWebhook removes a UI-created webhook. Its past deliveries survive:
// webhook_deliveries references the event, not the webhook row, so the audit
// trail of what was (or was not) delivered is not rewritten by a deletion.
func (s *Store) DeleteWebhook(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM webhooks WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: delete webhook: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// GetWebhookByName returns a UI-created webhook INCLUDING its signing secret.
// This is the dispatcher's accessor (AP7) — the one place a secret leaves the
// database — and no HTTP handler should call it.
func (s *Store) GetWebhookByName(ctx context.Context, name string) (Webhook, error) {
	const q = `SELECT id, name, url, secret, enabled, created_at, created_by FROM webhooks WHERE name = $1`
	var w Webhook
	err := s.pool.QueryRow(ctx, q, name).Scan(&w.ID, &w.Name, &w.URL, &w.Secret, &w.Enabled, &w.CreatedAt, &w.CreatedBy)
	if err != nil {
		return Webhook{}, noRows(err)
	}
	w.CreatedAt = w.CreatedAt.UTC()
	return w, nil
}

func scanWebhookNoSecret(row pgx.Row) (Webhook, error) {
	var w Webhook
	if err := row.Scan(&w.ID, &w.Name, &w.URL, &w.Enabled, &w.CreatedAt, &w.CreatedBy); err != nil {
		return Webhook{}, err
	}
	w.CreatedAt = w.CreatedAt.UTC()
	return w, nil
}
