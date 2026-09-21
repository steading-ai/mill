-- The global cursor WatchRuns resumes from: every run's events in
-- (occurred_at, id) order, unlike events_run_id_occurred_at_idx
-- (00011_events.sql), which orders one run's own. See docs/ledger.md
-- §events.

-- +goose Up

CREATE INDEX events_occurred_at_id_idx ON events (occurred_at, id);

-- +goose Down

DROP INDEX events_occurred_at_id_idx;
