-- Reconciliations: one row per pass a worker ran at start.

-- name: InsertReconciliation :one
INSERT INTO reconciliations (id, worker_ref, started_at, findings)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: ListReconciliations :many
SELECT * FROM reconciliations ORDER BY id DESC LIMIT $1;
