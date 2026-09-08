-- Skills: the portable part of a step, stored so a workflow version pins
-- the exact prompt it ran. See the skills section of docs/ledger.md.
--
-- A workflow step references a skill by (name, digest); two workflows
-- sharing a skill share the row.

-- +goose Up

CREATE TABLE skills (
    id         text        NOT NULL,
    -- From SKILL.md frontmatter.
    name       text        NOT NULL,
    -- Content hash of the skill directory.
    digest     text        NOT NULL,
    -- The SKILL.md body: the prompt.
    body       text        NOT NULL,
    -- Manifest of scripts/, assets/, references/: path to digest.
    files      jsonb       NOT NULL DEFAULT '{}'::jsonb,
    -- The flat string map from frontmatter.
    metadata   jsonb       NOT NULL DEFAULT '{}'::jsonb,
    source     jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT skills_pkey            PRIMARY KEY (id),
    CONSTRAINT skills_name_digest_key UNIQUE (name, digest),
    CONSTRAINT skills_id_ulid         CHECK (is_ulid(id)),
    CONSTRAINT skills_name_nonempty   CHECK (name <> ''),
    CONSTRAINT skills_digest_nonempty CHECK (digest <> '')
);

-- +goose Down

DROP TABLE skills;
