-- Runs: one node, worked through one workflow, by this mill. See the runs
-- section of docs/ledger.md.
--
-- There is no current_step column (Invariant 10). It is derived from the
-- run's step runs against its workflow definition; a column would drift
-- from that, which is the failure the ledger exists to prevent.

-- +goose Up

CREATE TABLE runs (
    id               text        NOT NULL,
    -- The dagger node. One live run per node, ever.
    node_id          text        NOT NULL,
    -- Display key, e.g. `dagger#18`.
    node_ref         text        NOT NULL,
    -- 1 for the first run on a node; incremented by restart.
    generation       bigint      NOT NULL DEFAULT 1,
    -- Copied from the node at creation.
    context_id       text        NOT NULL,
    -- Null while selecting; set once and never changed (Invariant 2).
    workflow_id      text,
    status           text        NOT NULL DEFAULT 'selecting',
    -- The current dagger lease, or null. What every write for this run is
    -- fenced by (Invariant 9).
    lease_id         text,
    worker_ref       text        NOT NULL DEFAULT '',
    -- Set while waiting or parked: the gate dagger returned.
    gate_node_id     text,
    parked_reason    text,
    -- Set by cancel; the worker reads it at every heartbeat.
    cancel_requested boolean     NOT NULL DEFAULT false,
    -- The run's git branch, derived from the node ref.
    branch           text        NOT NULL DEFAULT '',
    -- What gate auto predicates compare against, from the assessment or the
    -- project default.
    tier             text,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    ended_at         timestamptz,

    CONSTRAINT runs_pkey                  PRIMARY KEY (id),
    -- Invariant 1, second half: (node_id, generation) unique overall, so a
    -- restart's generation is a new row and never a rewrite of the old one.
    CONSTRAINT runs_node_id_generation_key UNIQUE (node_id, generation),
    CONSTRAINT runs_workflow_fkey         FOREIGN KEY (workflow_id) REFERENCES workflows (id),
    CONSTRAINT runs_id_ulid               CHECK (is_ulid(id)),
    CONSTRAINT runs_node_id_nonempty      CHECK (node_id <> ''),
    CONSTRAINT runs_generation_positive   CHECK (generation >= 1),
    CONSTRAINT runs_status_check          CHECK (status IN (
        'selecting', 'running', 'waiting', 'parked', 'done', 'failed', 'abandoned')),
    CONSTRAINT runs_parked_reason_check   CHECK (parked_reason IS NULL OR parked_reason IN (
        'retries', 'budget', 'denied', 'timeout', 'refused')),
    CONSTRAINT runs_parked_has_reason     CHECK (status <> 'parked' OR parked_reason IS NOT NULL),
    -- Invariant 11, the half the store can hold on its own: a run is never
    -- waiting or parked without the gate id dagger returned, so the store
    -- cannot assert a gate exists before dagger has made it true.
    CONSTRAINT runs_blocked_has_gate      CHECK (status NOT IN ('waiting', 'parked') OR gate_node_id IS NOT NULL),
    CONSTRAINT runs_tier_check            CHECK (tier IS NULL OR tier IN (
        'quick_fix', 'small', 'feature', 'large')),
    CONSTRAINT runs_terminal_has_ended    CHECK (
        (status IN ('done', 'failed', 'abandoned')) = (ended_at IS NOT NULL))
);

-- Invariant 1, first half: one live run per node. A run that is done,
-- failed or abandoned has left the index, so the next generation is free to
-- take its place.
CREATE UNIQUE INDEX runs_live_node_id_idx
    ON runs (node_id) WHERE status NOT IN ('done', 'failed', 'abandoned');

-- `mill status`: runs by state, most recently updated first.
CREATE INDEX runs_status_updated_at_idx ON runs (status, updated_at DESC, id DESC);

-- Invariants 2 and 12, which are transitions rather than shapes and so
-- cannot be CHECK constraints: a workflow is chosen once, and a gate is
-- filed once. Clearing gate_node_id is how resume records that a gate was
-- read and answered; overwriting one with a different id would mean a
-- second gate was filed for the same step run.
-- +goose StatementBegin
CREATE FUNCTION runs_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.workflow_id IS NOT NULL AND NEW.workflow_id IS DISTINCT FROM OLD.workflow_id THEN
        RAISE EXCEPTION 'run %: workflow_id is set once and never changed', OLD.id
            USING ERRCODE = 'restrict_violation';
    END IF;
    IF OLD.gate_node_id IS NOT NULL AND NEW.gate_node_id IS NOT NULL
        AND NEW.gate_node_id <> OLD.gate_node_id THEN
        RAISE EXCEPTION 'run %: a gate is filed once per step run (% is already filed)',
            OLD.id, OLD.gate_node_id
            USING ERRCODE = 'restrict_violation';
    END IF;
    NEW.updated_at := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER runs_guard
    BEFORE UPDATE ON runs
    FOR EACH ROW EXECUTE FUNCTION runs_guard();

-- +goose Down

DROP TABLE runs;
DROP FUNCTION runs_guard();
