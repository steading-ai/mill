package machine

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ivy/mill/internal/workspace"
	"github.com/ivy/mill/internal/workspace/machine/cache"
)

// RecordName is the run's own note of its machine, `run.json` in the run
// directory: what Create decided and what Attach needs to reopen it. The
// ledger's copy is [workspace.Sandbox] on the handle; this one is what a
// worker on the same host reads back before the ledger is asked anything.
const RecordName = "run.json"

// Record is `runs/<id>/run.json`.
type Record struct {
	// RunID is the run as the ledger names it; ID is its machine id
	// (IDFor), which names everything on the host.
	RunID string `json:"run_id"`
	ID    string `json:"id"`
	// Machine and Unit are the names on the host (docs/sandbox.md §Naming).
	Machine string `json:"machine"`
	Unit    string `json:"unit"`
	// Image is the digest the machine booted; Base the base it derived
	// from, and BaseFallback whether the base was the last resolved one
	// because the registry did not answer.
	Image        string `json:"image"`
	Base         string `json:"base"`
	BaseFallback bool   `json:"base_fallback,omitempty"`
	// Veth is the host-side interface as recorded after boot.
	Veth string `json:"veth,omitempty"`
	// Lineages are the caches restored, as the cache store reported them.
	Lineages []cache.Restored `json:"lineages"`
	// Findings are what the run's first session is told: an invalid
	// mill.toml, a derived build that failed, a nested request not
	// granted (docs/sandbox.md §"Preparatory steps").
	Findings []string `json:"findings,omitempty"`
	// Origin, Ref and Branch are the checkout's.
	Origin string `json:"origin"`
	Ref    string `json:"ref"`
	Branch string `json:"branch"`
	// Nested is whether the machine was granted a nested range.
	Nested bool `json:"nested,omitempty"`
	// CreatedAt is when the run directory was made; ReadyAt when the
	// machine reported ready, zero until it has. Destroy reads it: a
	// machine that never reported ready is never asked to stop
	// (docs/sandbox.md §Destroy).
	CreatedAt time.Time `json:"created_at"`
	ReadyAt   time.Time `json:"ready_at,omitempty"`
	// Slice is the step slice in flight, `mill-step-<ordinal>.slice`, and
	// Ordinal its number: written by BeginStep, cleared by EndStep. A
	// worker that attaches finds the slice a dead worker left running,
	// stops it, and recreates it under the same ordinal (docs/sandbox.md
	// §Reconciliation). Ordinal outlives the slice so the count continues.
	Slice   string `json:"slice,omitempty"`
	Ordinal int    `json:"ordinal,omitempty"`
}

// Ready reports whether the machine reached ready.
func (r Record) Ready() bool { return !r.ReadyAt.IsZero() }

// Sandbox is the record as the ledger's handle carries it.
func (r Record) Sandbox() *workspace.Sandbox {
	lineages := make([]string, 0, len(r.Lineages))
	for _, restored := range r.Lineages {
		lineages = append(lineages, fmt.Sprintf("%s@%d", restored.Lineage, restored.Generation))
	}
	return &workspace.Sandbox{Machine: r.Machine, Unit: r.Unit, Image: r.Image, Veth: r.Veth, Lineages: lineages}
}

// ErrNoRecord is a run directory with no record: never created on this
// host, or already destroyed.
var ErrNoRecord = errors.New("machine: no run record")

func writeRecord(dir string, record Record) error {
	body, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("machine: encode %s: %w", RecordName, err)
	}
	path := filepath.Join(dir, RecordName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(body, '\n'), 0o600); err != nil {
		return fmt.Errorf("machine: write %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("machine: write %s: %w", path, err)
	}
	return nil
}

func readRecord(dir string) (Record, error) {
	path := filepath.Join(dir, RecordName)
	body, err := os.ReadFile(path) //nolint:gosec // the run directory is the worker's own
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Record{}, fmt.Errorf("%w: %s", ErrNoRecord, path)
		}
		return Record{}, fmt.Errorf("machine: read %s: %w", path, err)
	}
	var record Record
	if err := json.Unmarshal(body, &record); err != nil {
		return Record{}, fmt.Errorf("machine: decode %s: %w", path, err)
	}
	return record, nil
}
