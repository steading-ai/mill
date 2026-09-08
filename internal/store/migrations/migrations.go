// Package migrations holds the ledger's schema as goose SQL migrations and
// applies them. The files are embedded so the binary carries its own schema,
// and sqlc reads the same files, so the two never disagree.
//
// Migrations are the only way the schema changes. Each one has a down that
// leaves the database as its up found it.
package migrations

import (
	"context"
	"embed"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// FS holds every migration, in goose's `NNNNN_name.sql` layout.
//
//go:embed *.sql
var FS embed.FS

// Migrator applies migrations to one database.
type Migrator struct {
	provider *goose.Provider
}

// Open connects to databaseURL and prepares a Migrator over [FS]. Close it
// when done.
func Open(ctx context.Context, databaseURL string) (*Migrator, error) {
	cfg, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	db := stdlib.OpenDB(*cfg)
	provider, err := goose.NewProvider(goose.DialectPostgres, db, FS, goose.WithLogger(goose.NopLogger()))
	if err != nil {
		return nil, joinClose(fmt.Errorf("prepare migrations: %w", err), db)
	}
	if err := provider.Ping(ctx); err != nil {
		return nil, joinClose(fmt.Errorf("connect to database: %w", err), db)
	}
	return &Migrator{provider: provider}, nil
}

// Close releases the connection.
func (m *Migrator) Close() error {
	return m.provider.Close()
}

// Up applies every pending migration.
func (m *Migrator) Up(ctx context.Context) error {
	_, err := m.provider.Up(ctx)
	return err
}

// DownTo rolls back migrations newer than version, newest first. Version 0
// rolls back everything.
func (m *Migrator) DownTo(ctx context.Context, version int64) error {
	_, err := m.provider.DownTo(ctx, version)
	return err
}

// Version reports the newest applied migration, or 0 on an empty database.
func (m *Migrator) Version(ctx context.Context) (int64, error) {
	return m.provider.GetDBVersion(ctx)
}

// Migrate brings the database at databaseURL up to the current schema. It has
// the shape testsupport.Migrate expects, so a suite can hand it straight to
// testsupport.Start.
func Migrate(ctx context.Context, databaseURL string) error {
	m, err := Open(ctx, databaseURL)
	if err != nil {
		return err
	}
	if err := m.Up(ctx); err != nil {
		return joinClose(fmt.Errorf("apply migrations: %w", err), m)
	}
	return m.Close()
}

type closer interface{ Close() error }

// joinClose closes c after a failure, keeping err as the primary error.
func joinClose(err error, c closer) error {
	if cerr := c.Close(); cerr != nil {
		return fmt.Errorf("%w (and close: %w)", err, cerr)
	}
	return err
}
