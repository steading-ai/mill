-- Events: the audit trail, and what `mill status --history` reads. See the
-- events section of docs/ledger.md.
--
-- Append-only, and not partitioned: dagger's events are, because they are a
-- change stream many consumers resume from, and these are neither. One
-- operator and tens of runs a day do not justify the machinery, and nothing
-- subscribes to this table.

-- +goose Up

CREATE TABLE events (
    -- ULID, so time-ordered.
    id          text        NOT NULL,
    run_id      text        NOT NULL,
    -- The attempt the change was about, when it was about one.
    step_run_id text,
    op          text        NOT NULL,
    before      jsonb,
    after       jsonb,
    worker_ref  text        NOT NULL DEFAULT '',
    occurred_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT events_pkey          PRIMARY KEY (id),
    CONSTRAINT events_run_fkey      FOREIGN KEY (run_id) REFERENCES runs (id),
    CONSTRAINT events_step_run_fkey FOREIGN KEY (step_run_id) REFERENCES step_runs (id),
    CONSTRAINT events_id_ulid       CHECK (is_ulid(id)),
    CONSTRAINT events_op_nonempty   CHECK (op <> '')
);

CREATE INDEX events_run_id_occurred_at_idx ON events (run_id, occurred_at, id);

CREATE TRIGGER events_append_only
    BEFORE UPDATE OR DELETE ON events
    FOR EACH ROW EXECUTE FUNCTION raise_append_only();

CREATE TRIGGER events_no_truncate
    BEFORE TRUNCATE ON events
    FOR EACH STATEMENT EXECUTE FUNCTION raise_append_only();

-- +goose Down

DROP TABLE events;
