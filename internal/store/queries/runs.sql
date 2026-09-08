-- Runs. The fence's two statements are here as well: see internal/store/fence.go
-- and Invariant 9 in docs/ledger.md.

-- Takes the run's row lock for the rest of the transaction. Nothing else in
-- this file may write a run without it.
-- name: LockRun :one
SELECT id FROM runs WHERE id = $1 FOR UPDATE;

-- The run's current lease and status, read as a statement of its own after
-- the lock. Under read committed a statement's snapshot is taken when it
-- starts, and a row that is only locked is not re-read once the lock is
-- granted, so a lease a concurrent claim committed while the lock was
-- waited for would be missed if this were joined to LockRun.
-- name: GetRunFence :one
SELECT lease_id, status FROM runs WHERE id = $1;

-- name: GetRun :one
SELECT * FROM runs WHERE id = $1;

-- name: GetRunByNodeRef :one
SELECT * FROM runs WHERE node_ref = $1 ORDER BY generation DESC LIMIT 1;

-- The run a claim on this node resumes: the newest generation that restart
-- has not retired. An abandoned run has been explicitly replaced, so the
-- caller sees nothing and creates the next generation.
-- name: GetRunByNode :one
SELECT * FROM runs
WHERE node_id = $1 AND status <> 'abandoned'
ORDER BY generation DESC LIMIT 1;

-- name: InsertRun :one
INSERT INTO runs (id, node_id, node_ref, generation, context_id, lease_id, worker_ref, branch)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- A claim takes the run over: the new lease becomes what every later write
-- is compared against, and the flags a previous attempt left behind are
-- cleared. gate_node_id is not, because a resume through a gate reads it.
-- name: SetRunLease :one
UPDATE runs
SET lease_id = $2, worker_ref = $3, status = 'running',
    parked_reason = NULL, cancel_requested = false, ended_at = NULL
WHERE id = $1
RETURNING *;

-- Selection's one write to the run: the workflow version and the tier the
-- assessment named. The trigger in 00004_runs.sql refuses a second one.
-- name: SetRunWorkflow :one
UPDATE runs
SET workflow_id = $2, tier = coalesce(sqlc.narg('tier')::text, tier), status = 'running'
WHERE id = $1
RETURNING *;

-- name: MarkRunWaiting :one
UPDATE runs
SET status = 'waiting', gate_node_id = $2, lease_id = NULL
WHERE id = $1
RETURNING *;

-- name: ParkRun :one
UPDATE runs
SET status = 'parked', gate_node_id = $2, parked_reason = $3, lease_id = NULL
WHERE id = $1
RETURNING *;

-- done, failed or abandoned: the run ends and holds no lease.
-- name: FinishRun :one
UPDATE runs
SET status = $2, ended_at = now(), lease_id = NULL
WHERE id = $1
RETURNING *;

-- Cancel is a request to the worker, not a transition: the flag is set and
-- the run's status is left alone. The worker reads it at its next heartbeat,
-- stops the step cleanly and releases its lease, and the run stays resumable.
-- name: RequestRunCancel :one
UPDATE runs SET cancel_requested = true WHERE id = $1 RETURNING *;

-- A gate that has been read and answered is no longer filed, which is what
-- lets the next gate step file one.
-- name: ClearRunGate :one
UPDATE runs SET gate_node_id = NULL, status = 'running' WHERE id = $1 RETURNING *;

-- name: ListRuns :many
SELECT * FROM runs
WHERE (cardinality(sqlc.arg('statuses')::text[]) = 0 OR status = ANY(sqlc.arg('statuses')::text[]))
  AND (sqlc.narg('project')::text IS NULL OR node_ref LIKE sqlc.narg('project')::text || '#%')
ORDER BY updated_at DESC, id DESC;

-- How many dagger leases this run has held, counted from the event log
-- rather than stored: every SetLease writes one. Distinct from a step's
-- attempt, which counts review rounds.
-- name: CountRunLeases :one
SELECT count(*) FROM events WHERE run_id = $1 AND op = 'set_lease';

-- The completing patch's idempotency key, minted once and never replaced: a
-- second call returns the key already stored, so a worker resuming a run
-- whose completing patch may have landed retries it under the same key.
-- name: SetRunCompleteRequestID :one
UPDATE runs
SET complete_request_id = CASE WHEN complete_request_id = '' THEN $2 ELSE complete_request_id END
WHERE id = $1
RETURNING *;
