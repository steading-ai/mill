-- Step tokens: the credential ladder's bottom rung, issued per step run.
-- See the step_tokens section of docs/ledger.md.
--
-- The bearer itself is never stored. The workspace validates a presented
-- bearer by hash and by revoked_at IS NULL.

-- +goose Up

CREATE TABLE step_tokens (
    id           text        NOT NULL,
    step_run_id  text        NOT NULL,
    -- SHA-256 of the bearer, hex.
    token_hash   text        NOT NULL,
    -- Copied from the step at issue.
    capabilities text[]      NOT NULL DEFAULT '{}',
    issued_at    timestamptz NOT NULL DEFAULT now(),
    -- Revoked when the step run leaves running (Invariant 8).
    revoked_at   timestamptz,

    CONSTRAINT step_tokens_pkey          PRIMARY KEY (id),
    CONSTRAINT step_tokens_hash_key      UNIQUE (token_hash),
    CONSTRAINT step_tokens_step_run_fkey FOREIGN KEY (step_run_id) REFERENCES step_runs (id),
    CONSTRAINT step_tokens_id_ulid       CHECK (is_ulid(id)),
    CONSTRAINT step_tokens_hash_nonempty CHECK (token_hash <> '')
);

-- One live token per step run: rotation revokes before it issues.
CREATE UNIQUE INDEX step_tokens_live_step_run_idx
    ON step_tokens (step_run_id) WHERE revoked_at IS NULL;

-- Invariant 8: a live token belongs to a running step run. Both directions
-- are deferred to commit, so revoking a token and moving its step run out of
-- running is one transaction whichever order the statements are written in.
-- +goose StatementBegin
CREATE FUNCTION step_tokens_live_step_run_running() RETURNS trigger
    LANGUAGE plpgsql AS $$
DECLARE
    step_status text;
BEGIN
    IF NEW.revoked_at IS NOT NULL THEN
        RETURN NULL;
    END IF;
    SELECT status INTO step_status FROM step_runs WHERE id = NEW.step_run_id;
    IF step_status <> 'running' THEN
        RAISE EXCEPTION 'step token % is live on step run %, whose status is %, not running',
            NEW.id, NEW.step_run_id, step_status
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION step_runs_no_live_tokens() RETURNS trigger
    LANGUAGE plpgsql AS $$
DECLARE
    live text;
BEGIN
    SELECT id INTO live FROM step_tokens
        WHERE step_run_id = NEW.id AND revoked_at IS NULL LIMIT 1;
    IF live IS NOT NULL THEN
        RAISE EXCEPTION 'step run % is % with step token % still live',
            NEW.id, NEW.status, live
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER step_tokens_live_step_run_running
    AFTER INSERT OR UPDATE ON step_tokens
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION step_tokens_live_step_run_running();

CREATE CONSTRAINT TRIGGER step_runs_no_live_tokens
    AFTER UPDATE ON step_runs
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW WHEN (NEW.status <> 'running')
    EXECUTE FUNCTION step_runs_no_live_tokens();

-- +goose Down

DROP TABLE step_tokens;
DROP TRIGGER step_runs_no_live_tokens ON step_runs;
DROP FUNCTION step_runs_no_live_tokens();
DROP FUNCTION step_tokens_live_step_run_running();
