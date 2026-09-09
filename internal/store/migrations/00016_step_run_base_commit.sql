-- The branch head an attempt started from.
--
-- A `commit` step closes on a commit the step made, which is a commit that
-- did not exist when the attempt began (docs/domain-model.md §Evidence).
-- Proving that needs the head the attempt started at, so the attempt records
-- it before the session runs.
--
-- Every kind records it, not only commit steps: what a superseded span
-- produced is the range from its base to the branch head, which is what a
-- rerun and a revise reason about.

-- +goose Up

ALTER TABLE step_runs
    ADD COLUMN base_commit text NOT NULL DEFAULT '';

-- +goose Down

ALTER TABLE step_runs
    DROP COLUMN base_commit;
