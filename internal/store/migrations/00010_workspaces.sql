-- Workspaces: where a run's files are, and whether they still exist. See
-- the workspaces section of docs/ledger.md.
--
-- The row is the ledger's record of a workspace; the directory is the
-- worker's. Rows are kept, and the directories are destroyed at done or
-- failed and retained while a run is waiting or parked.

-- +goose Up

CREATE TABLE workspaces (
    id           text        NOT NULL,
    run_id       text        NOT NULL,
    kind         text        NOT NULL,
    -- Where it is.
    worker_ref   text        NOT NULL DEFAULT '',
    -- The git ref it was created at.
    ref          text        NOT NULL DEFAULT '',
    state        text        NOT NULL DEFAULT 'live',
    created_at   timestamptz NOT NULL DEFAULT now(),
    destroyed_at timestamptz,

    CONSTRAINT workspaces_pkey      PRIMARY KEY (id),
    CONSTRAINT workspaces_run_fkey  FOREIGN KEY (run_id) REFERENCES runs (id),
    CONSTRAINT workspaces_id_ulid   CHECK (is_ulid(id)),
    CONSTRAINT workspaces_kind_check  CHECK (kind IN ('local', 'container', 'remote')),
    CONSTRAINT workspaces_state_check CHECK (state IN ('live', 'retained', 'destroyed')),
    CONSTRAINT workspaces_destroyed_at_check CHECK (
        (state = 'destroyed') = (destroyed_at IS NOT NULL))
);

-- One workspace per run while it is live; concurrent runs never share one.
CREATE UNIQUE INDEX workspaces_live_run_id_idx
    ON workspaces (run_id) WHERE state <> 'destroyed';

-- +goose Down

DROP TABLE workspaces;
