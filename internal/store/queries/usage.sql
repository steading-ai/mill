-- Usage: counts, never currency.

-- name: InsertUsage :one
INSERT INTO usage (id, step_run_id, model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- Exactly the rows of one step run that have not been posted yet. A step's
-- totals go to dagger when the attempt closes or fails, and only those rows
-- are marked.
-- name: MarkUsagePosted :many
UPDATE usage SET posted_at = now()
WHERE step_run_id = $1 AND posted_at IS NULL
RETURNING *;

-- name: ListUsageForStepRun :many
SELECT * FROM usage WHERE step_run_id = $1 ORDER BY recorded_at, id;

-- name: TotalsForStepRun :many
SELECT model,
       sum(input_tokens)::bigint       AS input_tokens,
       sum(output_tokens)::bigint      AS output_tokens,
       sum(cache_read_tokens)::bigint  AS cache_read_tokens,
       sum(cache_write_tokens)::bigint AS cache_write_tokens
FROM usage WHERE step_run_id = $1
GROUP BY model ORDER BY model;

-- name: TotalsForRun :many
SELECT u.model,
       sum(u.input_tokens)::bigint       AS input_tokens,
       sum(u.output_tokens)::bigint      AS output_tokens,
       sum(u.cache_read_tokens)::bigint  AS cache_read_tokens,
       sum(u.cache_write_tokens)::bigint AS cache_write_tokens
FROM usage u
JOIN step_runs sr ON sr.id = u.step_run_id
WHERE sr.run_id = $1
GROUP BY u.model ORDER BY u.model;

-- Every usage row in a window, with the run, workflow and step it belongs
-- to, so `mill cost` can roll one pass over them up four ways. The window is
-- open-ended when since is null, and the project filter matches the prefix
-- of a node reference such as `mill#18`.
-- name: UsageSince :many
SELECT r.node_ref,
       w.name    AS workflow_name,
       w.version AS workflow_version,
       sr.step_name,
       u.model,
       u.input_tokens,
       u.output_tokens,
       u.cache_read_tokens,
       u.cache_write_tokens
FROM usage u
JOIN step_runs sr ON sr.id = u.step_run_id
JOIN runs r ON r.id = sr.run_id
LEFT JOIN workflows w ON w.id = r.workflow_id
WHERE (sqlc.narg('since')::timestamptz IS NULL OR u.recorded_at >= sqlc.narg('since')::timestamptz)
  AND (sqlc.narg('project')::text IS NULL OR split_part(r.node_ref, '#', 1) = sqlc.narg('project')::text)
ORDER BY r.node_ref, sr.step_name, u.model;
