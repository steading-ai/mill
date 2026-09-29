-- A fifth live status: stopped. No worker holds the run; the next claim
-- resumes it, exactly like the leaseless-running shape a cancel, drain,
-- shutdown, or fenced reclaim leaves behind. See docs/domain-model.md
-- §"Run states" and docs/ledger.md Invariant 13.

-- +goose Up

ALTER TABLE runs
    DROP CONSTRAINT runs_status_check,
    ADD CONSTRAINT runs_status_check CHECK (status IN (
        'selecting', 'running', 'waiting', 'parked', 'done', 'failed', 'abandoned', 'stopped'));

-- +goose Down

ALTER TABLE runs
    DROP CONSTRAINT runs_status_check,
    ADD CONSTRAINT runs_status_check CHECK (status IN (
        'selecting', 'running', 'waiting', 'parked', 'done', 'failed', 'abandoned'));
