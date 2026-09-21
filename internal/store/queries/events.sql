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

-- LatestEventCursor is the position a fresh WatchRuns subscription opens
-- at: the newest event already on record, read from the database's own
-- clock rather than the caller's, so a fresh subscription's opening cursor
-- never races app-server/database clock skew. No rows yet is a valid
-- answer — an empty table's opening cursor is the zero time, before which
-- anything is new.
-- name: LatestEventCursor :one
SELECT occurred_at, id FROM events ORDER BY occurred_at DESC, id DESC LIMIT 1;

-- WatchableEvents reads events across every run, joined to runs for
-- node_ref since WatchRuns' response carries no other run content. The
-- (occurred_at, id) row comparison is the cursor: it resumes after exactly
-- the position named, with no gap and no re-delivery, using the same pair
-- events_occurred_at_id_idx (00023_events_watch_index.sql) orders by.
-- name: WatchableEvents :many
SELECT events.id, events.run_id, events.step_run_id, events.op, events.occurred_at, runs.node_ref
FROM events
JOIN runs ON runs.id = events.run_id
WHERE (events.occurred_at, events.id) > (sqlc.arg('after_occurred_at')::timestamptz, sqlc.arg('after_id')::text)
  AND (sqlc.narg('project')::text IS NULL OR runs.node_ref LIKE sqlc.narg('project')::text || '#%')
ORDER BY events.occurred_at, events.id;
