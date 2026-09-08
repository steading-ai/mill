-- Step tokens. The bearer is never stored; only its SHA-256.

-- name: InsertStepToken :one
INSERT INTO step_tokens (id, step_run_id, token_hash, capabilities)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetStepToken :one
SELECT * FROM step_tokens WHERE id = $1;

-- name: RevokeStepToken :one
UPDATE step_tokens SET revoked_at = now()
WHERE id = $1 AND revoked_at IS NULL
RETURNING *;

-- Every live token of a step run, revoked in the transaction that moves the
-- step run out of running (Invariant 8).
-- name: RevokeStepTokensForStepRun :many
UPDATE step_tokens SET revoked_at = now()
WHERE step_run_id = $1 AND revoked_at IS NULL
RETURNING *;
