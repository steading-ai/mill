-- Activity index: one row per sealed record, so a closed attempt's log can
-- be paged and an every-run tail can be ordered by time without re-reading
-- every attempt's frame. See docs/activity.md's "The index row".
--
-- Append-only, and keyed by (step_run_id, seq) rather than a synthetic id:
-- nothing references an activity row by id, and the natural key is also
-- what makes a retried SealAttempt idempotent (Invariant: a seal retried
-- after a lost response never double-counts).

-- +goose Up

CREATE TABLE activity (
    step_run_id    text        NOT NULL,
    -- The record's position in the sealed frame, after coalescing.
    seq            bigint      NOT NULL,
    at             timestamptz NOT NULL,
    -- Set for a record that spans time.
    ended_at       timestamptz,
    -- The oneof field name of the record's body, e.g. "tool_call".
    kind           text        NOT NULL,
    -- Who produced it: harness, workspace, check, or mill.
    source         text        NOT NULL,
    turn           bigint      NOT NULL DEFAULT 0,
    -- Set on a tool_call or tool_result: the call it names.
    call_id        text,
    -- Set when the event happened inside a subagent.
    parent_call_id text,
    -- Set on a tool_call: the tool's name.
    tool           text,
    -- The body's size before any cap.
    bytes          bigint      NOT NULL DEFAULT 0,
    truncated      boolean     NOT NULL DEFAULT false,
    -- The record's byte position in the uncompressed frame.
    "offset"       bigint      NOT NULL,

    CONSTRAINT activity_step_run_fkey FOREIGN KEY (step_run_id) REFERENCES step_runs (id),
    CONSTRAINT activity_kind_nonempty CHECK (kind <> ''),
    CONSTRAINT activity_source_known CHECK (source IN ('harness', 'workspace', 'check', 'mill'))
);

CREATE UNIQUE INDEX activity_step_run_id_seq_idx ON activity (step_run_id, seq);
CREATE INDEX activity_at_step_run_id_seq_idx ON activity (at, step_run_id, seq);

CREATE TRIGGER activity_append_only
    BEFORE UPDATE OR DELETE ON activity
    FOR EACH ROW EXECUTE FUNCTION raise_append_only();

CREATE TRIGGER activity_no_truncate
    BEFORE TRUNCATE ON activity
    FOR EACH STATEMENT EXECUTE FUNCTION raise_append_only();

-- +goose Down

DROP TABLE activity;
