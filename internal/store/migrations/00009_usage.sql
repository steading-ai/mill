-- Usage: what a turn cost, per model. See the usage section of
-- docs/ledger.md.
--
-- One row per turn per model, as the adapter reports: a turn that bills two
-- models writes two rows. Counts, never currency; a rate table is applied at
-- query time. posted_at is the one field that changes, when the step's
-- totals have been reported to dagger.

-- +goose Up

CREATE TABLE usage (
    id                 text        NOT NULL,
    step_run_id        text        NOT NULL,
    -- Free string. The adapter's name for the model that billed.
    model              text        NOT NULL,
    input_tokens       bigint      NOT NULL DEFAULT 0,
    output_tokens      bigint      NOT NULL DEFAULT 0,
    cache_read_tokens  bigint      NOT NULL DEFAULT 0,
    cache_write_tokens bigint      NOT NULL DEFAULT 0,
    recorded_at        timestamptz NOT NULL DEFAULT now(),
    -- When this row's step totals were posted to dagger; null until then.
    posted_at          timestamptz,

    CONSTRAINT usage_pkey          PRIMARY KEY (id),
    CONSTRAINT usage_step_run_fkey FOREIGN KEY (step_run_id) REFERENCES step_runs (id),
    CONSTRAINT usage_id_ulid       CHECK (is_ulid(id)),
    CONSTRAINT usage_model_nonempty CHECK (model <> ''),
    CONSTRAINT usage_tokens_nonnegative CHECK (
        input_tokens >= 0 AND output_tokens >= 0
        AND cache_read_tokens >= 0 AND cache_write_tokens >= 0)
);

-- The rollups: a step run's totals, and the unposted rows a close reports.
CREATE INDEX usage_step_run_id_idx ON usage (step_run_id);

-- +goose Down

DROP TABLE usage;
