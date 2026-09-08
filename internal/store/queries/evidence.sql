-- Evidence: insert-only. There is deliberately no update and no delete
-- here, and no grant for either (Invariant 6).

-- name: InsertEvidence :one
INSERT INTO evidence (step_run_id, kind, value, verified_by)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetEvidence :one
SELECT * FROM evidence WHERE step_run_id = $1;

-- name: ListEvidenceForRun :many
SELECT e.* FROM evidence e
JOIN step_runs sr ON sr.id = e.step_run_id
WHERE sr.run_id = $1
ORDER BY e.verified_at, e.step_run_id;
