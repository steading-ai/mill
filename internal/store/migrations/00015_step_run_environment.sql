-- What a skill step's session process was given of the mill's environment.
--
-- The environment a sidecar is spawned with is a mode, not a fixed floor
-- (docs/architecture.md §"Skill steps: the harness adapter"): `inherit`
-- hands on the mill's own environment minus a deny list, `strict` starts
-- from a floor and adds only what the step named. Both are decisions a
-- reviewer has to be able to see after the fact, so the attempt records the
-- two lists the decision produced: what the step asked for, and what the
-- deny list removed.

-- +goose Up

ALTER TABLE step_runs
    ADD COLUMN env_passthrough text[] NOT NULL DEFAULT '{}',
    ADD COLUMN env_stripped    text[] NOT NULL DEFAULT '{}';

-- +goose Down

ALTER TABLE step_runs
    DROP COLUMN env_passthrough,
    DROP COLUMN env_stripped;
