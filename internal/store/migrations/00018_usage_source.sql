-- Who counted a usage row.
--
-- The harness adapter reports what the SDK told it; the credential gateway
-- counts the model's own `usage` block on every response it forwards, from
-- outside the session (docs/sandbox.md §Credentials). The two are separate
-- rows with a source each, so what a session reported and what the model
-- billed can be read beside each other, and only the adapter's rows are
-- what the ledger calls the step's cost and posts to dagger.

-- +goose Up

ALTER TABLE usage
    ADD COLUMN source text NOT NULL DEFAULT 'sdk'
        CONSTRAINT usage_source_known CHECK (source IN ('sdk', 'gateway'));

-- +goose Down

ALTER TABLE usage
    DROP COLUMN source;
