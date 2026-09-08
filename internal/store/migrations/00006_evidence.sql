-- Evidence: the externally verified fact that closed a step run. See the
-- evidence section of docs/ledger.md.
--
-- Insert-only. There is no update path in SQL and no UPDATE or DELETE grant
-- for the application role (Invariant 6, and 00012_grants.sql). A step whose
-- evidence was wrong is a new attempt with new evidence; the old row stays.

-- +goose Up

CREATE TABLE evidence (
    -- Invariant 4: evidence closes exactly one step run and a step run has
    -- at most one evidence, which is what a primary key on the foreign key
    -- says.
    step_run_id text        NOT NULL,
    kind        text        NOT NULL,
    -- The fact, in the shape the kind defines. The mill.v1.Evidence value
    -- oneof in protojson.
    value       jsonb       NOT NULL,
    -- The check name or mill routine that established it.
    verified_by text        NOT NULL DEFAULT '',
    verified_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT evidence_pkey          PRIMARY KEY (step_run_id),
    CONSTRAINT evidence_step_run_fkey FOREIGN KEY (step_run_id) REFERENCES step_runs (id),
    CONSTRAINT evidence_kind_check    CHECK (kind IN (
        'commit', 'exit', 'verdict', 'structured', 'url', 'status', 'artifact', 'gate'))
);

CREATE TRIGGER evidence_append_only
    BEFORE UPDATE OR DELETE ON evidence
    FOR EACH ROW EXECUTE FUNCTION raise_append_only();

CREATE TRIGGER evidence_no_truncate
    BEFORE TRUNCATE ON evidence
    FOR EACH STATEMENT EXECUTE FUNCTION raise_append_only();

-- Invariant 5, both directions, deferred to commit so that the close and
-- its evidence are one transaction and their order within it does not
-- matter.
--
-- The evidence side is checked at insert only. A later revise moves a
-- closed step run to superseded and leaves its evidence in place as the
-- record of pass one, so "a step run with evidence is closed" is a fact
-- about the moment the evidence was written, not a shape held forever.
-- +goose StatementBegin
CREATE FUNCTION step_runs_closed_has_evidence() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM evidence WHERE step_run_id = NEW.id) THEN
        RAISE EXCEPTION 'step run % is closed with no evidence', NEW.id
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION evidence_closes_step_run() RETURNS trigger
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

CREATE CONSTRAINT TRIGGER step_runs_closed_has_evidence
    AFTER INSERT OR UPDATE ON step_runs
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW WHEN (NEW.status = 'closed')
    EXECUTE FUNCTION step_runs_closed_has_evidence();

CREATE CONSTRAINT TRIGGER evidence_closes_step_run
    AFTER INSERT ON evidence
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION evidence_closes_step_run();

-- +goose Down

DROP TABLE evidence;
DROP TRIGGER step_runs_closed_has_evidence ON step_runs;
DROP FUNCTION evidence_closes_step_run();
DROP FUNCTION step_runs_closed_has_evidence();
