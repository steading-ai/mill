-- What the application role may do, table by table.
--
-- Invariant 6 is the reason this file exists: evidence has no UPDATE or
-- DELETE grant, so a bug that tried to rewrite the record of a closed step
-- is refused by the server rather than by the mill's good behaviour. The
-- append-only triggers say the same thing a second time, one layer down.
--
-- Retention (docs/ledger.md) is what the DELETE grants are for: revoked
-- step tokens after 30 days, sessions after a run is terminal. Nothing
-- deletes a run, a step run, evidence, or an event, ever.

-- +goose Up

GRANT USAGE ON SCHEMA public TO mill_app;

GRANT SELECT, INSERT, UPDATE ON runs, step_runs, usage, workspaces TO mill_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON sessions, step_tokens TO mill_app;
GRANT SELECT, INSERT ON evidence, events, workflows, skills TO mill_app;

-- goose's own bookkeeping, so the role may read which migrations are
-- applied when the mill checks its schema at start.
GRANT SELECT ON goose_db_version TO mill_app;

-- +goose Down

REVOKE ALL ON goose_db_version FROM mill_app;
REVOKE ALL ON evidence, events, workflows, skills FROM mill_app;
REVOKE ALL ON sessions, step_tokens FROM mill_app;
REVOKE ALL ON runs, step_runs, usage, workspaces FROM mill_app;
REVOKE USAGE ON SCHEMA public FROM mill_app;
