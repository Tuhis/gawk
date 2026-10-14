-- R70 (docs/72 D1, D3–D5, D7): the broadcast moderation timeline.
--
-- Expand-only: new nullable or defaulted columns and new indexes, nothing the
-- previous release reads or writes differently, so rolling back is redeploying
-- the older image. Forward-only, and immutable once merged (docs/42 §4.15).

-- The window rule a session's sampler reported (D1). NULL for a session only
-- a sampler older than R70 scored: the timeline then knows no threshold
-- rather than guessing one. `window_size`, because WINDOW is reserved.
ALTER TABLE screening_sessions
  ADD COLUMN threshold   double precision,
  ADD COLUMN window_size int,
  ADD COLUMN window_hits int;

-- One incident across flicker (D3), its review history (D4) and the mute its
-- dismissal set (D5).
ALTER TABLE content_flags
  -- How many times the window tripped within this one incident.
  ADD COLUMN trips int NOT NULL DEFAULT 1,
  -- Set while a clear is held for the rejoin window: the signal is cleared,
  -- the incident not yet resolved.
  ADD COLUMN cleared_at timestamptz,
  ADD COLUMN returned_after_review boolean NOT NULL DEFAULT false,
  -- What the dismissal muted, kept on the flag for the timeline: the mute row
  -- itself is overwritten by the next dismissal.
  ADD COLUMN muted_until timestamptz,
  ADD COLUMN mute_scope text CHECK (mute_scope IN ('episode','duration','session'));

-- The leader's sweep looks only for held clears.
CREATE INDEX content_flags_held_clears ON content_flags (cleared_at)
  WHERE cleared_at IS NOT NULL AND state <> 'resolved';
-- The timeline reads one broadcast's flags, samples and sessions by key.
CREATE INDEX content_flags_broadcast_key ON content_flags (broadcast_key, raised_at);
CREATE INDEX content_flags_broadcast_resolved ON content_flags (broadcast_id, resolved_at) WHERE state = 'resolved';
CREATE INDEX screening_samples_key_time ON screening_samples (broadcast_key, sampled_at);
CREATE INDEX screening_sessions_key ON screening_sessions (broadcast_key, first_sample_at);

-- A mute's scope (D5). `episode` and `session` mutes are bound to the
-- publisher session they were set in; `duration` (R40's only kind) is not.
ALTER TABLE screening_mutes
  ADD COLUMN scope text NOT NULL DEFAULT 'duration' CHECK (scope IN ('episode','duration','session')),
  ADD COLUMN publisher_epoch text;
