-- The machine behind a workspace: name, unit, image digest, veth, and the
-- cache lineages the run restored and promoted (docs/sandbox.md
-- §Reconciliation, docs/ledger.md §workspaces). Reconciliation reads the
-- machine name against `machinectl list`, so it is a column of its own; the
-- rest is read whole, as `mill inspect` prints it, and is one document.
--
-- Every column is empty for a workspace with no machine: the local backend
-- records the row as before and nothing else about it changes. `machine` is
-- also the kind the machine backend records, which the check has to admit.

-- +goose Up

ALTER TABLE workspaces
    ADD COLUMN machine      text  NOT NULL DEFAULT '',
    ADD COLUMN unit         text  NOT NULL DEFAULT '',
    ADD COLUMN image_digest text  NOT NULL DEFAULT '',
    ADD COLUMN veth         text  NOT NULL DEFAULT '',
    -- [{scope, name, restored, promoted?}]; null when the workspace has
    -- no machine.
    ADD COLUMN lineages     jsonb,
    DROP CONSTRAINT workspaces_kind_check,
    ADD CONSTRAINT workspaces_kind_check CHECK (kind IN ('local', 'machine', 'container', 'remote'));

-- +goose Down

ALTER TABLE workspaces
    DROP COLUMN machine,
    DROP COLUMN unit,
    DROP COLUMN image_digest,
    DROP COLUMN veth,
    DROP COLUMN lineages,
    DROP CONSTRAINT workspaces_kind_check,
    ADD CONSTRAINT workspaces_kind_check CHECK (kind IN ('local', 'container', 'remote'));
