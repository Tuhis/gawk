-- R50 (docs/51 D5): the relay's event bus feeds moderation_events a second
-- category of row — activity, not moderation.
--
-- Expand-only, like every migration here: the previous release ignores both
-- columns, so a rollback is redeploying the older image and nothing else.
--
-- category separates the two vocabularies where they matter — the audit trail
-- an operator is accountable for, and the firehose of what the fleet is doing.
-- The default is 'moderation' so every existing row is classified correctly
-- without an UPDATE, and so a producer that predates R50 keeps writing
-- moderation rows.
ALTER TABLE moderation_events
  ADD COLUMN category text NOT NULL DEFAULT 'moderation';

-- source holds the CloudEvents id of the bus message a row came from
-- ("<pod>:<seq>"). It is what turns the bus's at-least-once delivery into
-- exactly-once at the table: a redelivery collides on the unique index and the
-- insert is a no-op that still acks. NULL for every portal-originated row —
-- unique indexes ignore NULLs, so any number of them coexist.
ALTER TABLE moderation_events
  ADD COLUMN source text;

CREATE UNIQUE INDEX moderation_events_source_key
  ON moderation_events (source)
  WHERE source IS NOT NULL;

-- The activity feed is read newest-first and pruned by age, both by category.
CREATE INDEX moderation_events_category_id_idx
  ON moderation_events (category, id DESC);
CREATE INDEX moderation_events_activity_occurred_idx
  ON moderation_events (occurred_at)
  WHERE category = 'activity';
