-- Sessions: the deterministic id and the adapter's reference to it. Never
-- a transcript.

-- name: UpsertSession :one
INSERT INTO sessions (run_id, name, id, harness, adapter_ref, worker_ref, last_turn_at)
VALUES ($1, $2, $3, $4, $5, $6, now())
ON CONFLICT (run_id, name) DO UPDATE
SET harness = excluded.harness, adapter_ref = excluded.adapter_ref,
    worker_ref = excluded.worker_ref, last_turn_at = now()
RETURNING *;

-- name: GetSession :one
SELECT * FROM sessions WHERE run_id = $1 AND name = $2;

-- name: ListSessionsForRun :many
SELECT * FROM sessions WHERE run_id = $1 ORDER BY started_at, name;
