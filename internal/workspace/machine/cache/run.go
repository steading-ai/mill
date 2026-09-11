package cache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Record is a run's own note of its caches, `cache.json` in the run
// directory: which lineages it restored from which root, and which step's
// candidate it holds. Promote reads it; nothing in the ledger duplicates
// it.
type Record struct {
	// LineageRoot is the root the lineages were restored from. A
	// promotion into any other root is refused: a nested run's record
	// names its own root, never the outer's (docs/sandbox.md §Promote).
	LineageRoot string `json:"lineage_root"`
	// Lineages are what the run restored, in the order given.
	Lineages []Restored `json:"lineages"`
	// Candidate is the last evidence-closed step's snapshot; nil when no
	// step has closed on evidence.
	Candidate *Candidate `json:"candidate,omitempty"`
}

// Restored is one lineage as a run restored it: what the ledger notes.
type Restored struct {
	Lineage
	// Generation is the generation the copy is a snapshot of; zero when
	// the copy started empty.
	Generation int `json:"generation"`
	// Reason says why the copy started empty, when it did: `no
	// generation` or `key changed`.
	Reason string `json:"reason,omitempty"`
	// RestoredAt is when.
	RestoredAt time.Time `json:"restored_at"`
}

// Candidate is the read-only snapshot set the run would promote.
type Candidate struct {
	// Step is the evidence-closed step it was taken at.
	Step string `json:"step"`
	// TakenAt is when.
	TakenAt time.Time `json:"taken_at"`
}

// Promoted is one lineage as a run promoted it: what the ledger notes.
type Promoted struct {
	Lineage
	// Generation is the new generation's number; zero when the promotion
	// was a reset.
	Generation int `json:"generation"`
	// SizeBytes is the candidate's estimate.
	SizeBytes uint64 `json:"size_bytes"`
	// Reset says the lineage was emptied instead, and Reason why.
	Reset  bool   `json:"reset,omitempty"`
	Reason string `json:"reason,omitempty"`
	// Pruned are the generations the promotion deleted.
	Pruned []int `json:"pruned,omitempty"`
}

// ErrOutsideRoot is a promotion addressed outside this store's lineage
// root: a nested worker's record reaching the outer's lineages, or a record
// of another host's. Refused, never redirected.
var ErrOutsideRoot = errors.New("cache: promotion addressed outside the lineage root")

// ErrRestored is a run whose caches are already restored.
var ErrRestored = errors.New("cache: the run's caches are already restored")

// recordPath is `<run dir>/cache.json`.
func (s *Store) recordPath(run string) string {
	return filepath.Join(s.RunDir(run), RecordName)
}

// Record reads a run's record. A run that never restored has none, which
// is [os.ErrNotExist].
func (s *Store) Record(run string) (Record, error) {
	var record Record
	if err := readJSON(s.recordPath(run), &record); err != nil {
		return Record{}, err
	}
	return record, nil
}

func (s *Store) writeRecord(run string, record Record) error {
	return writeJSON(s.recordPath(run), record)
}

// Restore makes the run's writable copy of each lineage under
// `runs/<id>/cache/<name>`: a snapshot of the latest generation, or an
// empty subvolume when the lineage has none or its latest generation was
// promoted under another key hash (docs/sandbox.md §Restore). The run's
// record is written for Promote to read, and each lineage's manifest notes
// the restore.
func (s *Store) Restore(ctx context.Context, run string, lineages []Lineage) ([]Restored, error) {
	if run == "" || !component.MatchString(run) {
		return nil, fmt.Errorf("cache: run id %q is not one path component", run)
	}
	if _, err := os.Stat(s.recordPath(run)); err == nil {
		return nil, fmt.Errorf("%w: %s", ErrRestored, run)
	}
	seen := map[string]bool{}
	for _, lineage := range lineages {
		if err := lineage.validate(); err != nil {
			return nil, err
		}
		if seen[lineage.Name] {
			return nil, fmt.Errorf("cache: lineage %s named twice in one run", lineage.Name)
		}
		seen[lineage.Name] = true
	}
	if err := os.MkdirAll(filepath.Join(s.RunDir(run), "cache"), 0o750); err != nil {
		return nil, fmt.Errorf("cache: %w", err)
	}
	record := Record{LineageRoot: s.LineageRoot()}
	for _, lineage := range lineages {
		restored, err := s.restore(ctx, run, lineage)
		if err != nil {
			return nil, err
		}
		record.Lineages = append(record.Lineages, restored)
	}
	if err := s.writeRecord(run, record); err != nil {
		return nil, err
	}
	return record.Lineages, nil
}

// restore is one lineage's copy, under the lineage's lock so the
// generation it snapshots is not pruned from under it.
func (s *Store) restore(ctx context.Context, run string, lineage Lineage) (Restored, error) {
	unlock, err := s.lock(ctx, lineage)
	if err != nil {
		return Restored{}, err
	}
	defer unlock()
	manifest, err := s.Manifest(lineage)
	if err != nil {
		return Restored{}, err
	}
	restored := Restored{Lineage: lineage, RestoredAt: s.config.Now()}
	copyPath := s.CopyPath(run, lineage.Name)
	switch latest := manifest.Latest(); {
	case latest == nil:
		restored.Reason = "no generation"
		err = s.host.CreateSubvolume(ctx, copyPath)
	case latest.KeyHash != lineage.KeyHash:
		restored.Reason = "key changed"
		err = s.host.CreateSubvolume(ctx, copyPath)
	default:
		restored.Generation = latest.Number
		err = s.host.Snapshot(ctx, s.GenerationPath(lineage, latest.Number), copyPath, false)
	}
	if err != nil {
		return Restored{}, fmt.Errorf("cache: restore %s for run %s: %w", lineage, run, err)
	}
	manifest.LastRestoredAt = restored.RestoredAt
	manifest.LastRestoredBy = run
	if err := s.writeManifest(lineage, manifest); err != nil {
		return Restored{}, err
	}
	return restored, nil
}

// Candidate takes a read-only snapshot of each of the run's copies as the
// step's candidate, replacing the previous step's. It runs when a step
// closes on evidence and before the step's slice is stopped
// (docs/sandbox.md §"Step boundary"); a step that is cancelled, times out
// or dies never reaches it, which is what leaves no candidate. The new
// snapshots are all taken before the old ones go, so a failure part way
// leaves the previous candidate standing.
func (s *Store) Candidate(ctx context.Context, run, step string) error {
	if step == "" {
		return errors.New("cache: a candidate names the step it was taken at")
	}
	record, err := s.Record(run)
	if err != nil {
		return err
	}
	next := filepath.Join(s.RunDir(run), "candidate.next")
	if err := os.RemoveAll(next); err != nil {
		return fmt.Errorf("cache: %w", err)
	}
	if err := os.MkdirAll(next, 0o750); err != nil {
		return fmt.Errorf("cache: %w", err)
	}
	var taken []string
	for _, lineage := range record.Lineages {
		target := s.candidateNext(run, lineage.Name)
		if err := s.host.Snapshot(ctx, s.CopyPath(run, lineage.Name), target, true); err != nil {
			for _, path := range taken {
				_ = s.deleteReadOnly(ctx, path, false)
			}
			_ = os.RemoveAll(next)
			return fmt.Errorf("cache: candidate of %s at step %s: %w", lineage.Name, step, err)
		}
		taken = append(taken, target)
	}
	if err := s.removeCandidate(ctx, run, record); err != nil {
		return err
	}
	if err := os.Rename(next, filepath.Join(s.RunDir(run), "candidate")); err != nil {
		return fmt.Errorf("cache: %w", err)
	}
	record.Candidate = &Candidate{Step: step, TakenAt: s.config.Now()}
	return s.writeRecord(run, record)
}

// removeCandidate deletes the run's candidate snapshots, if any, and their
// directory.
func (s *Store) removeCandidate(ctx context.Context, run string, record Record) error {
	dir := filepath.Join(s.RunDir(run), "candidate")
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	for _, lineage := range record.Lineages {
		path := s.CandidatePath(run, lineage.Name)
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := s.deleteReadOnly(ctx, path, false); err != nil {
			return fmt.Errorf("cache: replace candidate %s: %w", lineage.Name, err)
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("cache: %w", err)
	}
	return nil
}

// Discard deletes the run's copies and candidates and its record: the
// cache half of removing the run directory at destroy (docs/sandbox.md
// §Destroy), after Promote or instead of it. A run with no record has
// nothing to discard.
func (s *Store) Discard(ctx context.Context, run string) error {
	record, err := s.Record(run)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := s.removeCandidate(ctx, run, record); err != nil {
		return err
	}
	for _, lineage := range record.Lineages {
		path := s.CopyPath(run, lineage.Name)
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := s.host.DeleteSubvolume(ctx, path, false); err != nil {
			return fmt.Errorf("cache: discard %s: %w", lineage.Name, err)
		}
	}
	for _, name := range []string{"cache", "candidate.next", RecordName} {
		if err := os.RemoveAll(filepath.Join(s.RunDir(run), name)); err != nil {
			return fmt.Errorf("cache: %w", err)
		}
	}
	return nil
}
