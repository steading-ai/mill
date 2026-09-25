-- Step runs: one row per attempt at a step. See the step_runs section of
-- docs/ledger.md.
--
-- Append-only in spirit: a row's status moves on as the attempt ends, and
-- nothing is ever deleted. An interrupted step starts a new attempt from
-- the top rather than resuming, which is why the attempt number matters.

-- +goose Up

CREATE TABLE step_runs (
    id            text        NOT NULL,
    run_id        text        NOT NULL,
    -- The step from the run's workflow definition.
    step_name     text        NOT NULL,
    -- 1-based per (run_id, step_name), assigned as max + 1 (Invariant 7).
    attempt       bigint      NOT NULL,
    -- Copied at start so the row reads alone.
    kind          text        NOT NULL,
    -- Skill steps: the session this attempt ran in.
    session_id    text,
    -- The skift lease held when this attempt ran.
    lease_id      text,
    worker_ref    text        NOT NULL DEFAULT '',
    status        text        NOT NULL DEFAULT 'running',
    -- harness.v1.ProcessExit for skill and check steps. status = -1 means
    -- the process never ran.
    exit          jsonb,
    -- The step run whose failure this attempt is the revise for.
    feedback_from text,
    -- Gate steps: true when the auto predicate closed it without a human.
    auto_approved boolean     NOT NULL DEFAULT false,
    started_at    timestamptz NOT NULL DEFAULT now(),
    ended_at      timestamptz,

    CONSTRAINT step_runs_pkey            PRIMARY KEY (id),
    CONSTRAINT step_runs_attempt_key     UNIQUE (run_id, step_name, attempt),
    CONSTRAINT step_runs_run_fkey        FOREIGN KEY (run_id) REFERENCES runs (id),
    CONSTRAINT step_runs_feedback_fkey   FOREIGN KEY (feedback_from) REFERENCES step_runs (id),
    CONSTRAINT step_runs_id_ulid         CHECK (is_ulid(id)),
    CONSTRAINT step_runs_step_nonempty   CHECK (step_name <> ''),
    CONSTRAINT step_runs_attempt_positive CHECK (attempt >= 1),
    CONSTRAINT step_runs_kind_check      CHECK (kind IN ('skill', 'check', 'gate')),
    CONSTRAINT step_runs_status_check    CHECK (status IN (
        'running', 'closed', 'failed', 'aborted', 'superseded')),
    CONSTRAINT step_runs_ended_when_over CHECK ((status = 'running') = (ended_at IS NULL))
);

-- Invariant 3: at most one running step run per run.
CREATE UNIQUE INDEX step_runs_one_running_idx ON step_runs (run_id) WHERE status = 'running';

-- The latest attempt at a step, which is what current_step is derived from.
CREATE INDEX step_runs_run_id_step_name_attempt_idx
    ON step_runs (run_id, step_name, attempt DESC);

-- Invariant 7: attempts are contiguous. The number is not the caller's to
-- choose; it is max + 1 for the (run, step) pair, checked here so a query
-- that computed it some other way cannot leave a gap.
-- +goose StatementBegin
CREATE FUNCTION step_runs_attempt_contiguous() RETURNS trigger
    LANGUAGE plpgsql AS $$
DECLARE
    expected bigint;
BEGIN
    SELECT coalesce(max(attempt), 0) + 1 INTO expected
        FROM step_runs WHERE run_id = NEW.run_id AND step_name = NEW.step_name;
    IF NEW.attempt <> expected THEN
        RAISE EXCEPTION 'step run %: attempt % for (%, %) must be %',
            NEW.id, NEW.attempt, NEW.run_id, NEW.step_name, expected
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER step_runs_attempt_contiguous
    BEFORE INSERT ON step_runs
    FOR EACH ROW EXECUTE FUNCTION step_runs_attempt_contiguous();

-- +goose Down

DROP TABLE step_runs;
DROP FUNCTION step_runs_attempt_contiguous();
