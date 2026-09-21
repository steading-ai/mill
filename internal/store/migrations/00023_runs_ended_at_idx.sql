-- Ended-run listing: an index for ListRuns's keyset scan over runs that
-- have ended, newest first. Partial because ended_at is null on a live run
-- (docs/ledger.md §runs), so the index carries only the rows this ordering
-- ever applies to.

-- +goose Up

CREATE INDEX runs_ended_at_idx ON runs (ended_at DESC, id DESC) WHERE ended_at IS NOT NULL;

-- +goose Down

DROP INDEX runs_ended_at_idx;
