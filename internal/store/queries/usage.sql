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
