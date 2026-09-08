-- Workflows and skills: immutable rows that belong to no run.

-- name: InsertWorkflow :one
INSERT INTO workflows (id, name, scope, project, version, definition, source)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetWorkflowByID :one
SELECT * FROM workflows WHERE id = $1;

-- name: GetWorkflowByVersion :one
SELECT * FROM workflows
WHERE name = $1 AND scope = $2 AND project = $3 AND version = $4;

-- A name resolves to the newest version in its scope unless a run pins one.
-- name: GetNewestWorkflow :one
SELECT * FROM workflows
WHERE name = $1 AND scope = $2 AND project = $3
ORDER BY created_at DESC, id DESC LIMIT 1;

-- name: ListWorkflows :many
SELECT * FROM workflows
WHERE (sqlc.narg('name')::text IS NULL OR name = sqlc.narg('name')::text)
  AND (sqlc.narg('scope')::text IS NULL OR scope = sqlc.narg('scope')::text)
  AND (sqlc.narg('project')::text IS NULL OR project = sqlc.narg('project')::text)
ORDER BY name, created_at DESC, id DESC;

-- name: InsertSkill :one
INSERT INTO skills (id, name, digest, body, files, metadata, source)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetSkillByDigest :one
SELECT * FROM skills WHERE name = $1 AND digest = $2;
