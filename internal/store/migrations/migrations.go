// Package migrations holds the ledger's schema as goose SQL migrations and
// applies them. The files are embedded so the binary carries its own schema,
// and sqlc reads the same files, so the two never disagree.
//
// Migrations are the only way the schema changes. Each one has a down that
// leaves the database as its up found it.
package migrations

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// FS holds every migration, in goose's `NNNNN_name.sql` layout.
//
//go:embed *.sql
var FS embed.FS

// outOfOrderSafe marks a migration, by a comment on its own line, as safe to
// apply after a database has already moved past its version: it touches
// nothing any migration between it and the database's current version could
// have depended on. Without it, a missing lower migration blocks Up rather
// than being applied out of order.
const outOfOrderSafe = "-- +mill out-of-order: safe"

// ErrBlocked is the error Up wraps when a migration below the database's
// current version is missing and unmarked: applying it would need goose's
// out-of-order mode, and nothing here may assume that is safe without the
// migration saying so itself.
var ErrBlocked = errors.New("migrations: blocked on a missing lower migration")

// Migrator applies migrations to one database.
type Migrator struct {
	db       *sql.DB
	fsys     fs.FS
	provider *goose.Provider
}

// Open connects to databaseURL and prepares a Migrator over [FS]. Close it
// when done.
func Open(ctx context.Context, databaseURL string) (*Migrator, error) {
	return OpenFS(ctx, databaseURL, FS)
}

// OpenFS is [Open] over fsys instead of the package's own [FS]: the seam a
// spec uses to exercise Up's missing-migration handling against a small,
// synthetic schema instead of the real one.
func OpenFS(ctx context.Context, databaseURL string, fsys fs.FS) (*Migrator, error) {
	cfg, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	db := stdlib.OpenDB(*cfg)
	provider, err := goose.NewProvider(goose.DialectPostgres, db, fsys, goose.WithLogger(goose.NopLogger()))
	if err != nil {
		return nil, joinClose(fmt.Errorf("prepare migrations: %w", err), db)
	}
	if err := provider.Ping(ctx); err != nil {
		return nil, joinClose(fmt.Errorf("connect to database: %w", err), db)
	}
	return &Migrator{db: db, fsys: fsys, provider: provider}, nil
}

// Close releases the connection.
func (m *Migrator) Close() error {
	return m.provider.Close()
}

// Up applies every pending migration. A missing migration below the current
// database version is applied out of order only when every one of them is
// marked outOfOrderSafe; otherwise Up returns an [ErrBlocked] error naming
// the migration and the manual remedy, before touching the database at all.
func (m *Migrator) Up(ctx context.Context) error {
	statuses, err := m.provider.Status(ctx)
	if err != nil {
		return err
	}
	var current int64
	for _, s := range statuses {
		if s.State == goose.StateApplied && s.Source.Version > current {
			current = s.Source.Version
		}
	}
	var missing []*goose.MigrationStatus
	for _, s := range statuses {
		if s.State == goose.StatePending && s.Source.Version < current {
			missing = append(missing, s)
		}
	}
	if len(missing) == 0 {
		_, err := m.provider.Up(ctx)
		return err
	}
	safe, err := safeVersions(m.fsys)
	if err != nil {
		return err
	}
	for _, s := range missing {
		if !safe[s.Source.Version] {
			return fmt.Errorf(
				"%w: migration %d (%s) is missing but the database is already at version %d; "+
					"apply it by hand: goose -dir internal/store/migrations postgres \"$DATABASE_URL\" -allow-missing up",
				ErrBlocked, s.Source.Version, s.Source.Path, current)
		}
	}
	outOfOrder, err := goose.NewProvider(goose.DialectPostgres, m.db, m.fsys,
		goose.WithLogger(goose.NopLogger()), goose.WithAllowOutofOrder(true))
	if err != nil {
		return err
	}
	_, err = outOfOrder.Up(ctx)
	return err
}

// safeVersions is the set of migration versions in fsys marked
// outOfOrderSafe.
func safeVersions(fsys fs.FS) (map[int64]bool, error) {
	versions, err := Versions(fsys)
	if err != nil {
		return nil, err
	}
	safe := make(map[int64]bool, len(versions))
	for name, version := range versions {
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(string(body), "\n") {
			if strings.TrimSpace(line) == outOfOrderSafe {
				safe[version] = true
				break
			}
		}
	}
	return safe, nil
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

// VersionOf parses the goose `NNNNN_name.sql` version prefix from a
// migration's file name, or reports ok false when name carries none.
func VersionOf(name string) (version int64, ok bool) {
	digits := name
	if i := strings.IndexByte(name, '_'); i >= 0 {
		digits = name[:i]
	}
	version, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, false
	}
	return version, true
}

// Versions reads every migration's version out of fsys, by file name — the
// data an order check compares between two branches without needing goose or
// a database.
func Versions(fsys fs.FS) (map[string]int64, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	versions := make(map[string]int64, len(entries))
	for _, entry := range entries {
		if version, ok := VersionOf(entry.Name()); ok {
			versions[entry.Name()] = version
		}
	}
	return versions, nil
}

// Wanted is the newest migration this binary carries, which is the version a
// current database is at. `mill doctor` compares it against what the database
// reports.
func Wanted() int64 {
	entries, err := FS.ReadDir(".")
	if err != nil {
		return 0
	}
	var newest int64
	for _, entry := range entries {
		if version, ok := VersionOf(entry.Name()); ok && version > newest {
			newest = version
		}
	}
	return newest
}
