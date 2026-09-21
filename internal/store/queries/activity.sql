-- Activity index: one row per sealed record. See docs/activity.md's "The
-- index row". Insert is idempotent: a seal retried after a lost response
-- inserts nothing new for a (step_run_id, seq) already recorded.

-- name: InsertActivityIndex :one
INSERT INTO activity (step_run_id, seq, at, ended_at, kind, source, turn, call_id, parent_call_id, tool, bytes, truncated, "offset")
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
ON CONFLICT (step_run_id, seq) DO NOTHING
RETURNING *;

-- name: CountActivityForStepRun :one
SELECT count(*)::bigint FROM activity WHERE step_run_id = $1;

-- name: ListActivityForStepRun :many
SELECT * FROM activity WHERE step_run_id = $1 ORDER BY seq;

-- Every sealed record since a point in time, across every run, in log
-- order. The window is open-ended when since is null.
-- name: ListActivitySince :many
SELECT * FROM activity
WHERE sqlc.narg('since')::timestamptz IS NULL OR at >= sqlc.narg('since')::timestamptz
ORDER BY at, step_run_id, seq;
