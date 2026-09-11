-- Workspaces: where a run's files are, whether they still exist, and the
-- machine behind them (docs/ledger.md §workspaces). Written from
-- internal/store inside the fenced transaction, like every run-scoped row.

-- The run's live workspace, if it has one: at most one, by the partial
-- unique index.
-- name: GetLiveWorkspace :one
SELECT * FROM workspaces WHERE run_id = $1 AND state <> 'destroyed';

-- The run's newest workspace, live or not: what inspect shows.
-- name: GetLatestWorkspace :one
SELECT * FROM workspaces WHERE run_id = $1 ORDER BY id DESC LIMIT 1;

-- name: InsertWorkspace :one
INSERT INTO workspaces (id, run_id, kind, worker_ref, ref, machine, unit, image_digest, veth, lineages)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- A reattached workspace's record brought up to date: the worker that holds
-- it now, and the machine as it reports it.
-- name: UpdateWorkspace :one
UPDATE workspaces
SET kind = $2, worker_ref = $3, ref = $4, machine = $5, unit = $6, image_digest = $7, veth = $8, lineages = $9
WHERE id = $1
RETURNING *;

-- The workspace's end: its files gone, and its lineages with what each
-- promoted.
-- name: DestroyWorkspace :one
UPDATE workspaces
SET state = 'destroyed', destroyed_at = now(), lineages = $2
WHERE id = $1
RETURNING *;
