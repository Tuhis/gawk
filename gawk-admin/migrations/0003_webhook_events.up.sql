-- R49 (docs/50 D8): a per-webhook event filter.
--
-- NULL means what every webhook has always received: every moderation event
-- and none of the activity ones. A list is exact — the webhook receives the
-- listed types and nothing else — so the room activity events R49 makes
-- deliverable are opt-in, and a receiver that never opts in sees no change.
--
-- Expand-only, like every migration here: the previous release never reads the
-- column, so a rollback is redeploying the older image and nothing else.
ALTER TABLE webhooks
  ADD COLUMN events text[];
