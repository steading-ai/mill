-- The skill's description: the one frontmatter field besides `name` that
-- every agent-skills client is guaranteed to carry, and what a router
-- matches a node against.
--
-- It lives in its own column rather than in `metadata` because it is a
-- normative frontmatter field, not a breadcrumb, and because
-- `mill workflow pull` has to write a SKILL.md the loader will accept: a
-- skill with no description is not a valid skill.

-- +goose Up

ALTER TABLE skills ADD COLUMN description text NOT NULL DEFAULT '';

-- +goose Down

ALTER TABLE skills DROP COLUMN description;
