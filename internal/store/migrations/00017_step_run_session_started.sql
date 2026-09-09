-- Whether the harness ever spoke in an attempt's session.
--
-- A session is started once and resumed thereafter, and which of the two a
-- worker asks for cannot be derived from the attempt number: an attempt that
-- died before the harness produced a single event left no conversation
-- behind, and resuming one is a failure that has nothing to do with the
-- work. The ledger is the record (docs/ledger.md Invariant 10), so the fact
-- is a column, written when the session's first event arrives.

-- +goose Up

ALTER TABLE step_runs
    ADD COLUMN session_started boolean NOT NULL DEFAULT false;

-- +goose Down

ALTER TABLE step_runs
    DROP COLUMN session_started;
