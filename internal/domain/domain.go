// Package domain is the layer between the store's API and its rules: the
// rules the ledger's schema cannot hold, applied before a write reaches it.
//
// Three of them live here. A run's status moves along the edges of the state
// diagram in docs/domain-model.md §"Run states" and along no others; a step
// run's attempt does the same for its own, smaller diagram; and evidence is
// the shape its kind defines before any of it is recorded. Each is a
// transition rather than a shape, so no CHECK constraint can express it, and
// each has to hold whichever face of the binary asks — which is why it is
// here and not in a handler.
//
// This is the only package that may import [github.com/ivy/mill/internal/store]
// (depguard enforces it). Handlers reach the ledger through a [Service] and
// never around it, so a rule cannot be skipped by calling the store directly.
package domain

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ivy/mill/internal/store"
	"github.com/ivy/mill/internal/store/migrations"
)

// The failures a caller distinguishes. The first five are the store's own,
// re-exported so a handler maps them to a Connect code without importing the
// store; the typed errors below them are this layer's.
var (
	// ErrNotFound: the row a call named does not exist.
	ErrNotFound = store.ErrNotFound
	// ErrFenced: the presented lease is not the run's current one. Its
	// message names both. The normal path for a stale worker.
	ErrFenced = store.ErrFenced
	// ErrNotClaimable: a takeover of a run whose status admits none.
	ErrNotClaimable = store.ErrNotClaimable
	// ErrNotRunning: an attempt that has already ended.
	ErrNotRunning = store.ErrNotRunning
	// ErrUnknownStep: a step name the run's workflow does not contain.
	ErrUnknownStep = store.ErrUnknownStep
	// ErrNoWorkflow: an operation needing the workflow of a run that is
	// still selecting.
	ErrNoWorkflow = store.ErrNoWorkflow
)

// Service is the ledger with its rules applied. Every handler holds one.
type Service struct {
	store *store.Store
}

// Migrate brings the database at databaseURL up to the current schema. The
// service applies it at startup, before it answers anything.
func Migrate(ctx context.Context, databaseURL string) error {
	return migrations.Migrate(ctx, databaseURL)
}

// Open connects to databaseURL and returns the service over it. Close it
// when done.
func Open(ctx context.Context, databaseURL string) (*Service, error) {
	st, err := store.Open(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("domain: %w", err)
	}
	return &Service{store: st}, nil
}

// New returns the service over a pool the caller already owns. Closing the
// service closes the pool.
func New(pool *pgxpool.Pool) *Service {
	return &Service{store: store.FromPool(pool)}
}

// Close releases every connection.
func (s *Service) Close() { s.store.Close() }

// Ping reports whether the database answers. Readiness is this, not the
// socket being bound: a service that cannot reach its database cannot take
// work.
func (s *Service) Ping(ctx context.Context) error {
	if err := s.store.Pool().Ping(ctx); err != nil {
		return fmt.Errorf("domain: %w", err)
	}
	return nil
}
