-- Reconciliations: what a worker found and did at start, before it claimed
-- anything — the machines it told from the ledger's live runs, the leftovers
-- it removed, the scratch projects it swept by name (docs/sandbox.md
-- §Reconciliation, §"External state").
--
-- A record of the host, not of a run: an orphan machine has no live run to
-- hang an event on, so this is its own table rather than a row of events.
-- Append-only, like events; one row per pass.

-- +goose Up

CREATE TABLE reconciliations (
    -- ULID, so time-ordered.
    id         text        NOT NULL,
    worker_ref text        NOT NULL DEFAULT '',
    started_at timestamptz NOT NULL,
    -- The pass's findings, in order: [{kind, name, action, detail}].
    findings   jsonb       NOT NULL,
    recorded_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT reconciliations_pkey    PRIMARY KEY (id),
    CONSTRAINT reconciliations_id_ulid CHECK (is_ulid(id))
);

-- +goose Down

DROP TABLE reconciliations;
