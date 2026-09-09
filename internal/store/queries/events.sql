-- Events: the audit trail. Append-only, and written by the same
-- transaction as the change it records.

-- name: InsertEvent :one
INSERT INTO events (id, run_id, step_run_id, op, before, after, worker_ref)
VALUES ($1, $2, sqlc.narg('step_run_id'), $3, $4, $5, $6)
RETURNING *;

-- name: ListEvents :many
SELECT * FROM events WHERE run_id = $1 ORDER BY occurred_at, id;

-- name: CountHeartbeats :many
SELECT step_run_id, count(*) AS heartbeats
FROM events
WHERE run_id = $1 AND op = 'session_heartbeat' AND step_run_id IS NOT NULL
GROUP BY step_run_id;
