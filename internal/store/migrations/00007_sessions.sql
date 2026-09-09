-- Sessions: a resumable agent session inside a run. See the sessions
-- section of docs/ledger.md.
--
-- The ledger knows a session's deterministic id and the adapter's reference
-- to it; it cannot replay one. Transcripts live with the harness, on the
-- worker's host.

-- +goose Up

CREATE TABLE sessions (
    run_id       text        NOT NULL,
    -- The step's session value: implement, review.
    name         text        NOT NULL,
    -- UUIDv5 of (context_id, generation, name). Stored for lookup;
    -- recomputable.
    id           text        NOT NULL,
    -- Adapter name: claude today.
    harness      text        NOT NULL,
    -- Whatever the adapter needs to resume: for Claude, the config dir
    -- and cwd the transcript lives under.
    adapter_ref  jsonb       NOT NULL DEFAULT '{}'::jsonb,
    -- The host the transcript is on. A different worker cannot
    -- transcript-resume; it graph-resumes with a fresh session of this id.
    worker_ref   text        NOT NULL DEFAULT '',
    started_at   timestamptz NOT NULL DEFAULT now(),
    last_turn_at timestamptz,

    CONSTRAINT sessions_pkey          PRIMARY KEY (run_id, name),
    CONSTRAINT sessions_run_fkey      FOREIGN KEY (run_id) REFERENCES runs (id),
    CONSTRAINT sessions_name_nonempty CHECK (name <> ''),
    CONSTRAINT sessions_harness_nonempty CHECK (harness <> ''),
    CONSTRAINT sessions_id_uuid       CHECK (
        id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$')
);

-- +goose Down

DROP TABLE sessions;
