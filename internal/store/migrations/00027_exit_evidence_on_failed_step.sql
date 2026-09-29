-- A plain check or skill failure fails the step it ran rather than closing
-- it, but it is still evidence: a resumed attempt must be able to recover
-- what it found from the ledger alone, the same way a reviewer's revise
-- verdict already could (00026_verdict_evidence_on_failed_step.sql, and see
-- docs/architecture.md's evidence section). Relax evidence_closes_step_run
-- again so exit evidence may also be recorded against a step run that
-- failed, not only one that closed. Every other kind is unaffected.

-- +goose Up

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION evidence_closes_step_run() RETURNS trigger
    LANGUAGE plpgsql AS $$
DECLARE
    step_status text;
BEGIN
    SELECT status INTO step_status FROM step_runs WHERE id = NEW.step_run_id;
    IF step_status = 'closed' OR (step_status = 'failed' AND NEW.kind IN ('verdict', 'exit')) THEN
        RETURN NULL;
    END IF;
    RAISE EXCEPTION 'evidence for step run % whose status is %, not closed',
        NEW.step_run_id, step_status
        USING ERRCODE = 'restrict_violation';
END
$$;
-- +goose StatementEnd

-- +goose Down

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION evidence_closes_step_run() RETURNS trigger
    LANGUAGE plpgsql AS $$
DECLARE
    step_status text;
BEGIN
    SELECT status INTO step_status FROM step_runs WHERE id = NEW.step_run_id;
    IF step_status = 'closed' OR (step_status = 'failed' AND NEW.kind = 'verdict') THEN
        RETURN NULL;
    END IF;
    RAISE EXCEPTION 'evidence for step run % whose status is %, not closed',
        NEW.step_run_id, step_status
        USING ERRCODE = 'restrict_violation';
END
$$;
-- +goose StatementEnd
