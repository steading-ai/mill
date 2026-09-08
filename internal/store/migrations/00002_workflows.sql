-- Workflows: immutable definitions, one row per version. See the workflows
-- section of docs/ledger.md.
--
-- Never updated. A new definition with the same name and scope is a new
-- row, and a run pins the version it started with.

-- +goose Up

CREATE TABLE workflows (
    id         text        NOT NULL,
    name       text        NOT NULL,
    -- org or project. A project definition overrides an org one.
    scope      text        NOT NULL,
    -- Empty for an org-scoped definition, so the uniqueness below is total
    -- rather than skipping rows on a null.
    project    text        NOT NULL DEFAULT '',
    -- Content hash of the canonical definition. Two definitions that hash
    -- the same are the same version.
    version    text        NOT NULL,
    -- The definition as stored: steps, evidence specs, gates, capability
    -- sets, harness tables. The mill.v1.Workflow message in protojson.
    definition jsonb       NOT NULL,
    -- {kind: repo|cli, repo, commit, path, pushed_by}.
    source     jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT workflows_pkey          PRIMARY KEY (id),
    CONSTRAINT workflows_version_key   UNIQUE (name, scope, project, version),
    CONSTRAINT workflows_id_ulid       CHECK (is_ulid(id)),
    CONSTRAINT workflows_name_nonempty CHECK (name <> ''),
    CONSTRAINT workflows_version_nonempty CHECK (version <> ''),
    CONSTRAINT workflows_scope_check   CHECK (scope IN ('org', 'project')),
    CONSTRAINT workflows_project_scope CHECK ((scope = 'project') = (project <> ''))
);

-- Resolving a name to its newest version in a scope, which is what a
-- loader does for every run that does not pin one.
CREATE INDEX workflows_name_scope_created_at_idx
    ON workflows (name, scope, project, created_at DESC, id DESC);

-- +goose Down

DROP TABLE workflows;
