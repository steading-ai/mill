-- Helpers every later migration leans on, and the role the application
-- connects as. See docs/ledger.md: the invariants there are constraints or
-- transactional checks, never conventions, and the two functions below are
-- what several of them are written in terms of.

-- +goose Up

-- Identifiers are ULIDs held as their canonical 26-character Crockford
-- base32 text. The regex is the whole grammar: the first character carries
-- the two spare high bits, so it never exceeds 7.
-- +goose StatementBegin
CREATE FUNCTION is_ulid(candidate text) RETURNS boolean
    LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE
    RETURN candidate ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$';
-- +goose StatementEnd

-- Refuses any rewrite of a row. Shared by every table whose rows are a
-- record of something that happened: evidence and events.
-- +goose StatementBegin
CREATE FUNCTION raise_append_only() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% is append-only: % is not permitted', TG_TABLE_NAME, TG_OP
        USING ERRCODE = 'restrict_violation';
END
$$;
-- +goose StatementEnd

-- The privilege set the mill runs under, granted table by table in
-- 00012_grants.sql. It is a group role with no login of its own: a
-- deployment grants its login user membership, and a spec reaches it with
-- SET ROLE. Roles are cluster-wide, so this is created only when absent —
-- and the check is not atomic with the create, so a second session racing
-- this migration against the same cluster (parallel specs each migrating
-- their own database) can pass the check before the first session's CREATE
-- ROLE commits. The loser's "already exists" surfaces as duplicate_object
-- when CreateRole's own catalog scan catches it, or as unique_violation
-- when both sessions instead reach the pg_authid_rolname_index insert
-- together; the nested block catches either rather than aborting its
-- migration.
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'mill_app') THEN
        BEGIN
            CREATE ROLE mill_app NOLOGIN;
        EXCEPTION WHEN duplicate_object OR unique_violation THEN
            NULL;
        END;
    END IF;
END
$$;
-- +goose StatementEnd

-- +goose Down

-- DROP OWNED BY removes this database's grants to the role; the role itself
-- may still be referenced from another database in the cluster, in which
-- case it stays and the next up finds it.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'mill_app') THEN
        EXECUTE 'DROP OWNED BY mill_app';
        BEGIN
            DROP ROLE mill_app;
        EXCEPTION WHEN dependent_objects_still_exist THEN
            NULL;
        END;
    END IF;
END
$$;
-- +goose StatementEnd

DROP FUNCTION raise_append_only();
DROP FUNCTION is_ulid(text);
