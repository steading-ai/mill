-- A workflow row's created_at is fixed at first store (00002_workflows.sql):
-- "when this version was stored", never touched again. Selection and
-- listing order on it, so a version that already exists and is pushed again
-- never becomes the one a run without a pinned version follows while a
-- later-created row for the same name exists — the last push cannot win
-- back over the first creation. pushed_at is a second timestamp, touched on
-- every push including one that stores nothing new, that selection and
-- listing order on instead; created_at keeps its existing meaning.

-- +goose Up

ALTER TABLE workflows ADD COLUMN pushed_at timestamptz NOT NULL DEFAULT now();

-- Backfills to created_at, not to the migration's own apply time: combined
-- with the id DESC tiebreak already in the index below, this reproduces
-- today's created_at-based ordering exactly for every row not re-pushed
-- since, so applying this migration changes no run's selected workflow.
UPDATE workflows SET pushed_at = created_at;

DROP INDEX workflows_name_scope_created_at_idx;

CREATE INDEX workflows_name_scope_pushed_at_idx
    ON workflows (name, scope, project, pushed_at DESC, id DESC);

-- Column-level, not table-wide: 00012_grants.sql gives mill_app only
-- SELECT, INSERT on workflows, grouped with the other append-only tables.
-- A touch on re-push needs UPDATE on pushed_at alone; definition, version,
-- id, name, scope, project, source and created_at stay ungrantable for
-- UPDATE, so a workflow row's content is still never edited.
GRANT UPDATE (pushed_at) ON workflows TO mill_app;

-- +goose Down

REVOKE UPDATE (pushed_at) ON workflows FROM mill_app;

DROP INDEX workflows_name_scope_pushed_at_idx;

CREATE INDEX workflows_name_scope_created_at_idx
    ON workflows (name, scope, project, created_at DESC, id DESC);

ALTER TABLE workflows DROP COLUMN pushed_at;
