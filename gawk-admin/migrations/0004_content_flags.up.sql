-- R40 (docs/71 D9, D10, D20, D22, §9.4): automatic content flagging.
--
-- Expand-only, like every migration here: five new tables and nothing the
-- previous release reads, so a rollback is redeploying the older image and
-- nothing else. Forward-only, and immutable once merged (docs/42 §4.15).
--
-- Every piece of lifecycle state lives here rather than in a replica's memory,
-- because gawk-admin runs several replicas behind one Service: the sampler's
-- POSTs, the operator's clicks and the leader's reminder tick may each land
-- on a different one.

-- content_flags: one row per flag, both modes (D19). A `flag`-mode row is part
-- of the moderation audit trail and is never pruned (D17); an `observe`-mode
-- row is calibration data, pruned with the samples.
CREATE TABLE content_flags (
  id                bigserial PRIMARY KEY,
  mode              text NOT NULL CHECK (mode IN ('observe','flag')),
  state             text NOT NULL CHECK (state IN ('open','acknowledged','resolved')),
  resolution        text CHECK (resolution IN ('cleared','dismissed','killed','auto_killed','ended')),
  resolution_reason text,
  broadcast_id      text NOT NULL,        -- raw; portal-only and the event subject (docs/52 D9)
  broadcast_key     text,                 -- HMAC'd
  publisher_epoch   text NOT NULL,        -- the relay's publisher session (docs/71 §9.1)
  sampler_id        text NOT NULL,
  raised_at         timestamptz NOT NULL,
  updated_at        timestamptz NOT NULL,
  last_post_at      timestamptz NOT NULL, -- the signal is live until stale-after past this
  acknowledged_at   timestamptz,
  acknowledged_by   text,
  resolved_at       timestamptz,
  resolved_by       text,
  peak_score        double precision NOT NULL DEFAULT 0,
  reminders         int NOT NULL DEFAULT 0,
  last_reminded_at  timestamptz,
  body              jsonb NOT NULL        -- the last POST's gawk.content-flag.v1 body: scores, window, recent
);

-- At most ONE unresolved flag per broadcast per mode. The database, not a
-- read-then-write in Go, is what makes "raise or update?" right when two
-- replicas take two POSTs for the same broadcast at once (the bans precedent,
-- docs/42 D16).
CREATE UNIQUE INDEX content_flags_one_unresolved
  ON content_flags (broadcast_id, mode) WHERE state <> 'resolved';
CREATE INDEX content_flags_raised_at ON content_flags (raised_at DESC);
CREATE INDEX content_flags_observe_raised ON content_flags (raised_at) WHERE mode = 'observe';

-- screening_mutes: a dismissal mutes re-flagging of the broadcast (D10). One
-- row per broadcast, overwritten by the next dismissal.
CREATE TABLE screening_mutes (
  broadcast_id text PRIMARY KEY,
  muted_until  timestamptz NOT NULL,
  muted_by     text NOT NULL,
  flag_id      bigint
);

-- screening_sessions: one row per publisher session the samplers scored (D20),
-- whose running samples_taken is what screening_samples' sequence numbers
-- are numbered against — by gawk-admin under this row's lock, never by the
-- sampler, so the count survives sampler restarts and re-homing.
CREATE TABLE screening_sessions (
  publisher_epoch text PRIMARY KEY,
  broadcast_id    text NOT NULL,
  broadcast_key   text NOT NULL,
  samples_taken   bigint NOT NULL DEFAULT 0,
  samples_stored  bigint NOT NULL DEFAULT 0,
  first_sample_at timestamptz NOT NULL,
  last_sample_at  timestamptz NOT NULL,
  -- floor is the highest -sample-floor any batch for the session declared:
  -- replay refuses a threshold below it (D21).
  floor           double precision NOT NULL,
  labels          text[] NOT NULL,
  mode            text NOT NULL,
  sampler_id      text NOT NULL
);
CREATE INDEX screening_sessions_last_sample ON screening_sessions (last_sample_at);

-- screening_samples: the samples at or above the floor (D20). A missing
-- sequence number is a sample that scored below it — never one that was not
-- taken — which is what keeps a replay of the window rule exact.
CREATE TABLE screening_samples (
  publisher_epoch text NOT NULL,
  seq             bigint NOT NULL,
  sampled_at      timestamptz NOT NULL,
  broadcast_id    text NOT NULL,
  broadcast_key   text NOT NULL,
  scores          jsonb NOT NULL,
  PRIMARY KEY (publisher_epoch, seq)
);
CREATE INDEX screening_samples_sampled_at ON screening_samples (sampled_at);

-- screening_shard_polls: the last target-list poll per shard index (D22). The
-- `of` of the most recent poll is the expected shard count; any index under
-- it that has not polled for 30 s is missing.
CREATE TABLE screening_shard_polls (
  shard       int PRIMARY KEY,
  shard_count int NOT NULL,
  sampler_id  text NOT NULL,
  polled_at   timestamptz NOT NULL
);
