-- Step runs: one row per attempt.

-- The attempt number is computed here, not passed in: max + 1 for the
-- (run, step) pair, which is Invariant 7. The trigger in
-- 00005_step_runs.sql refuses any other value, so this is the only way in.
-- name: InsertStepRun :one
INSERT INTO step_runs (id, run_id, step_name, attempt, kind, session_id, lease_id, worker_ref, feedback_from)
VALUES (
    $1, $2, $3,
    coalesce((SELECT max(attempt) FROM step_runs WHERE run_id = $2 AND step_name = $3), 0) + 1,
    $4, sqlc.narg('session_id'), $5, $6, sqlc.narg('feedback_from'))
RETURNING *;

-- name: GetStepRun :one
SELECT * FROM step_runs WHERE id = $1;

-- name: ListStepRuns :many
SELECT * FROM step_runs WHERE run_id = $1 ORDER BY started_at, id;

-- The latest attempt at each step of a run, which is what current_step is
-- derived from.
-- name: LatestStepRuns :many
SELECT DISTINCT ON (step_name) * FROM step_runs
WHERE run_id = $1
ORDER BY step_name, attempt DESC;

-- What the environment mode gave the attempt's session process. Written once,
-- after the adapter has decided and before the session's outcome is known.
-- name: RecordStepRunEnvironment :one
UPDATE step_runs SET env_passthrough = $2, env_stripped = $3
WHERE id = $1
RETURNING *;

-- name: CloseStepRun :one
UPDATE step_runs SET status = 'closed', ended_at = now()
WHERE id = $1 AND status = 'running'
RETURNING *;

-- name: FailStepRun :one
UPDATE step_runs SET status = 'failed', ended_at = now(), exit = $2
WHERE id = $1 AND status = 'running'
RETURNING *;

-- A running row on resume belongs to a dead or fenced-out worker; the next
-- worker starts a new attempt rather than continuing it.
-- name: AbortOpenStepRuns :many
UPDATE step_runs SET status = 'aborted', ended_at = now()
WHERE run_id = $1 AND status = 'running'
RETURNING *;

-- A revise supersedes the latest attempt at every step from its target
-- through the failing step. The evidence those attempts produced stays and
-- no longer closes anything, which is what moves current_step backwards.
-- name: SupersedeStepRuns :many
UPDATE step_runs SET status = 'superseded', ended_at = coalesce(ended_at, now())
WHERE id IN (
    SELECT DISTINCT ON (latest.step_name) latest.id FROM step_runs latest
    WHERE latest.run_id = sqlc.arg('run_id') AND latest.step_name = ANY(sqlc.arg('step_names')::text[])
    ORDER BY latest.step_name, latest.attempt DESC)
  AND status <> 'superseded'
RETURNING *;

-- Gate steps: the auto predicate closed this attempt without a human, which
-- the ledger records rather than inferring later.
-- name: MarkStepRunAutoApproved :one
UPDATE step_runs SET auto_approved = true WHERE id = $1 RETURNING *;
