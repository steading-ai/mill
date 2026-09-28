-- A review's revise verdict fails the step it reviewed rather than closing
-- it, but it is still evidence: Inspect must return it exactly like an
-- approved review's. Relax evidence_closes_step_run so a verdict may also be
-- recorded against a step run that failed, not only one that closed. Every
-- other kind is unaffected, and a verdict itself still needs the row closed
-- or failed, not running. See docs/architecture.md's evidence section.

-- +goose Up

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

-- +goose Down

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION evidence_closes_step_run() RETURNS trigger
    LANGUAGE plpgsql AS $$
DECLARE
    step_status text;
BEGIN
    SELECT status INTO step_status FROM step_runs WHERE id = NEW.step_run_id;
    IF step_status <> 'closed' THEN
        RAISE EXCEPTION 'evidence for step run % whose status is %, not closed',
            NEW.step_run_id, step_status
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd
