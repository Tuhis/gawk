-- R39 (docs/42 §4.6): gawk-admin's system of record.
--
-- Forward-only (docs/42 §4.15 / D18). There is deliberately no companion
-- .down.sql: rollback is redeploying the previous application version, which
-- the expand-contract compatibility policy guarantees works. Fix a mistake
-- with the next migration number, never by editing this file — it is
-- immutable the moment it merges.
--
-- The `.up.sql` suffix is golang-migrate's file convention, not a claim that a
-- down migration exists somewhere.

-- bans: one row per enforcement decision, projected into a Ban CR by the
-- reconciler. `cr_name` is stored (not derived at read time) so a row and its
-- CR can be correlated by anyone with psql, including when the naming rule
-- later gains a case.
CREATE TABLE bans (
  id           uuid PRIMARY KEY,
  target_type  text NOT NULL CHECK (target_type IN ('broadcastId','ip')),
  target_value text NOT NULL,          -- normalized via moderation.Normalize
  state        text NOT NULL CHECK (state IN ('active','expired','removed')),
  reason       text NOT NULL DEFAULT '',
  created_at   timestamptz NOT NULL,
  created_by   text NOT NULL,
  expires_at   timestamptz,            -- NULL = permanent
  removed_at   timestamptz,
  removed_by   text,
  source_broadcast_id text,            -- raw ID the action was taken against (IP bans too)
  cr_name      text NOT NULL
);

-- The database, not application logic, is what makes "one active ban per
-- target" true under two gawk-admin replicas racing each other (docs/42 D16).
CREATE UNIQUE INDEX bans_one_active_per_target
  ON bans (target_type, target_value) WHERE state = 'active';

-- The janitor sweeps by expiry; the portal lists by recency.
CREATE INDEX bans_active_expiry ON bans (expires_at) WHERE state = 'active';
CREATE INDEX bans_created_at ON bans (created_at DESC);

-- moderation_events: the audit trail and the notification source. Append-only
-- by convention — nothing in gawk-admin updates or deletes a row here.
CREATE TABLE moderation_events (
  id            bigserial PRIMARY KEY,
  type          text NOT NULL,          -- broadcast.killed | ban.created | ban.expired | ban.removed  (content_flag.raised reserved for R40)
  occurred_at   timestamptz NOT NULL,
  actor         text NOT NULL,          -- OIDC email, "system", or (R40) a service-identity subject
  broadcast_key text,                   -- HMAC'd, safe to export
  broadcast_id  text,                   -- raw; portal-only, never in webhooks
  payload       jsonb NOT NULL DEFAULT '{}'
);

-- webhooks: UI-created only. Chart-defined webhooks come from -static-webhooks
-- and never enter the database, which is why their signing secrets stay in
-- Kubernetes Secrets (docs/42 §5).
CREATE TABLE webhooks (
  id         uuid PRIMARY KEY,
  name       text NOT NULL UNIQUE,      -- unique across config-sourced names too (enforced in code)
  url        text NOT NULL,
  secret     text NOT NULL,             -- HMAC signing key; never returned by the API
  enabled    boolean NOT NULL DEFAULT true,
  created_at timestamptz NOT NULL,
  created_by text NOT NULL
);

-- webhook_deliveries: the retry queue. Claimed with FOR UPDATE SKIP LOCKED so a
-- leadership handover cannot double-send (docs/42 §4.10).
CREATE TABLE webhook_deliveries (
  id           bigserial PRIMARY KEY,
  event_id     bigint NOT NULL REFERENCES moderation_events(id),
  webhook_name text NOT NULL,           -- works for config- and UI-sourced webhooks alike
  state        text NOT NULL CHECK (state IN ('pending','delivered','failed')),
  attempts     int  NOT NULL DEFAULT 0,
  next_attempt_at timestamptz,
  last_error   text,
  delivered_at timestamptz
);

-- One delivery per (event, webhook): enqueueing is then idempotent, so a
-- retried enqueue after a crash between AppendEvent and EnqueueDeliveries
-- cannot double-send.
CREATE UNIQUE INDEX webhook_deliveries_event_webhook
  ON webhook_deliveries (event_id, webhook_name);

-- The dispatcher's claim query: due pending rows, oldest first.
CREATE INDEX webhook_deliveries_due
  ON webhook_deliveries (next_attempt_at) WHERE state = 'pending';

-- The events view renders every event's delivery state.
CREATE INDEX webhook_deliveries_event ON webhook_deliveries (event_id);
