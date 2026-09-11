-- A fourth step kind: preparatory.
--
-- A preparatory step readies the machine for the sessions — the toolchain
-- install, `mise trust /work && mise install` — and closes on the record of
-- its run, exit and output, whichever way it exited; its failure is a
-- finding for the first session and never a failed attempt
-- (docs/sandbox.md §"Preparatory steps", docs/domain-model.md §Step). The
-- kind is copied onto every attempt so the row reads alone, and the check
-- that keeps the column honest has to admit it.

-- +goose Up

ALTER TABLE step_runs
    DROP CONSTRAINT step_runs_kind_check,
    ADD CONSTRAINT step_runs_kind_check CHECK (kind IN ('skill', 'check', 'gate', 'preparatory'));

-- +goose Down

ALTER TABLE step_runs
    DROP CONSTRAINT step_runs_kind_check,
    ADD CONSTRAINT step_runs_kind_check CHECK (kind IN ('skill', 'check', 'gate'));
