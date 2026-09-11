-- Usage: counts, never currency. Every row says who counted it: the adapter
-- (`sdk`) or the credential gateway (`gateway`).

-- name: InsertUsage :one
INSERT INTO usage (id, step_run_id, model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, source)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
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

-- A step run's totals by model and by who counted them. The adapter's rows
-- are the step's cost — what is posted to dagger and what status shows —
-- and the gateway's are the count taken outside the session, beside them.
-- name: TotalsForStepRun :many
SELECT model, source,
       sum(input_tokens)::bigint       AS input_tokens,
       sum(output_tokens)::bigint      AS output_tokens,
       sum(cache_read_tokens)::bigint  AS cache_read_tokens,
       sum(cache_write_tokens)::bigint AS cache_write_tokens
FROM usage WHERE step_run_id = $1
GROUP BY model, source ORDER BY model, source;

-- name: TotalsForRun :many
SELECT u.model, u.source,
       sum(u.input_tokens)::bigint       AS input_tokens,
       sum(u.output_tokens)::bigint      AS output_tokens,
       sum(u.cache_read_tokens)::bigint  AS cache_read_tokens,
       sum(u.cache_write_tokens)::bigint AS cache_write_tokens
FROM usage u
JOIN step_runs sr ON sr.id = u.step_run_id
WHERE sr.run_id = $1
GROUP BY u.model, u.source ORDER BY u.model, u.source;

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
       u.source,
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
ORDER BY r.node_ref, sr.step_name, u.model, u.source;
