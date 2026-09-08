-- The idempotency key of the patch that completes the node.
--
-- "Dagger first, store second" (Invariant 11) leaves a window where the
-- completing ApplyPatch may have landed and `store.Done` may not have. The
-- next worker has to retry that patch under the key the first one used, or
-- dagger would see a second, distinct write. Minting the key and storing it
-- before the patch is sent is what makes the retry the same patch.

-- +goose Up

ALTER TABLE runs ADD COLUMN complete_request_id text NOT NULL DEFAULT '';

-- +goose Down

ALTER TABLE runs DROP COLUMN complete_request_id;
