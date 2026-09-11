package cache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Promote makes the run's candidate the next generation of each lineage it
// restored: the candidate is renamed into `gen-<n+1>`, the manifest gains
// the generation, and the lineage is pruned to its [Keep] newest
// (docs/sandbox.md §Promote). A run with no candidate — no step closed on
// evidence — promotes nothing and returns nothing.
//
// A global lineage whose candidate's estimate exceeds the cap is reset
// instead: the candidate and every generation are deleted, the manifest
// records why, and the next run starts it empty. A record whose lineage
// root is not this store's is refused with [ErrOutsideRoot].
func (s *Store) Promote(ctx context.Context, run string) ([]Promoted, error) {
	record, err := s.Record(run)
	if err != nil {
		return nil, err
	}
	if filepath.Clean(record.LineageRoot) != filepath.Clean(s.LineageRoot()) {
		return nil, fmt.Errorf("%w: run %s restored from %s, this worker's is %s", ErrOutsideRoot, run, record.LineageRoot, s.LineageRoot())
	}
	if record.Candidate == nil {
		return nil, nil
	}
	var out []Promoted
	for _, restored := range record.Lineages {
		lineage := restored.Lineage
		if err := lineage.validate(); err != nil {
			return out, err
		}
		if !strings.HasPrefix(s.Dir(lineage), s.LineageRoot()+string(filepath.Separator)) {
			return out, fmt.Errorf("%w: %s", ErrOutsideRoot, lineage)
		}
		candidate := s.CandidatePath(run, lineage.Name)
		if _, err := os.Stat(candidate); errors.Is(err, os.ErrNotExist) {
			continue
		}
		promoted, err := s.promote(ctx, run, record.Candidate.Step, lineage, candidate)
		if err != nil {
			return out, err
		}
		out = append(out, promoted)
	}
	record.Candidate = nil
	if err := s.writeRecord(run, record); err != nil {
		return out, err
	}
	return out, nil
}

// promote is one lineage's promotion, under its lock.
func (s *Store) promote(ctx context.Context, run, step string, lineage Lineage, candidate string) (Promoted, error) {
	unlock, err := s.lock(ctx, lineage)
	if err != nil {
		return Promoted{}, err
	}
	defer unlock()
	manifest, err := s.Manifest(lineage)
	if err != nil {
		return Promoted{}, err
	}
	// The estimate is du -s of the read-only snapshot, once, here; it is
	// the same subvolume before and after the rename.
	size, err := s.host.DiskUsage(ctx, candidate)
	if err != nil {
		return Promoted{}, fmt.Errorf("cache: size of %s candidate: %w", lineage, err)
	}
	promoted := Promoted{Lineage: lineage, SizeBytes: size}
	if lineage.Scope.IsGlobal() && size > s.config.GlobalCap {
		return s.reset(ctx, run, step, lineage, candidate, manifest, size)
	}

	next := 1
	if latest := manifest.Latest(); latest != nil {
		next = latest.Number + 1
	}
	generation := s.GenerationPath(lineage, next)
	if err := s.move(ctx, candidate, generation); err != nil {
		return Promoted{}, fmt.Errorf("cache: promote %s into gen-%d: %w", lineage, next, err)
	}
	promoted.Generation = next
	manifest.Generations = append(manifest.Generations, Generation{
		Number:    next,
		CreatedAt: s.config.Now(),
		SizeBytes: size,
		Run:       run,
		Step:      step,
		KeyHash:   lineage.KeyHash,
	})
	manifest.Reset = nil
	if err := s.writeManifest(lineage, manifest); err != nil {
		return Promoted{}, err
	}

	// Prune to the newest Keep. The manifest is written before the
	// deletions and after them, so a worker dying between leaves a record
	// that names more than the tree holds, which Manifest reconciles.
	for len(manifest.Generations) > Keep {
		oldest := manifest.Generations[0]
		last := len(manifest.Generations) == Keep+1
		if err := s.deleteReadOnly(ctx, s.GenerationPath(lineage, oldest.Number), last); err != nil {
			return promoted, fmt.Errorf("cache: prune %s gen-%d: %w", lineage, oldest.Number, err)
		}
		promoted.Pruned = append(promoted.Pruned, oldest.Number)
		manifest.Generations = manifest.Generations[1:]
	}
	if len(promoted.Pruned) > 0 {
		if err := s.writeManifest(lineage, manifest); err != nil {
			return promoted, err
		}
	}
	return promoted, nil
}

// move renames the candidate into its generation: one rename within the
// filesystem, with the read-only property cleared for it and set again
// after, because rename(2) refuses a read-only subvolume's root with EROFS
// just as unlink does. The generation is the candidate's own subvolume,
// not a copy of it.
func (s *Store) move(ctx context.Context, candidate, generation string) error {
	if err := s.host.SetReadOnly(ctx, candidate, false); err != nil {
		return err
	}
	if err := s.host.Rename(ctx, candidate, generation); err != nil {
		_ = s.host.SetReadOnly(ctx, candidate, true)
		return err
	}
	return s.host.SetReadOnly(ctx, generation, true)
}

// reset is the promotion a global cap refuses: the candidate goes, every
// generation goes, and the manifest says why.
func (s *Store) reset(ctx context.Context, run, step string, lineage Lineage, candidate string, manifest Manifest, size uint64) (Promoted, error) {
	reason := fmt.Sprintf("candidate from run %s step %s estimated at %d bytes exceeds the global cap of %d bytes", run, step, size, s.config.GlobalCap)
	promoted := Promoted{Lineage: lineage, SizeBytes: size, Reset: true, Reason: reason}
	if err := s.deleteReadOnly(ctx, candidate, false); err != nil {
		return Promoted{}, fmt.Errorf("cache: reset %s: %w", lineage, err)
	}
	for i, generation := range manifest.Generations {
		last := i == len(manifest.Generations)-1
		if err := s.deleteReadOnly(ctx, s.GenerationPath(lineage, generation.Number), last); err != nil {
			return Promoted{}, fmt.Errorf("cache: reset %s gen-%d: %w", lineage, generation.Number, err)
		}
		promoted.Pruned = append(promoted.Pruned, generation.Number)
	}
	manifest.Generations = nil
	manifest.Reset = &Reset{
		At:        s.config.Now(),
		Run:       run,
		Step:      step,
		SizeBytes: size,
		Cap:       s.config.GlobalCap,
		Reason:    reason,
	}
	if err := s.writeManifest(lineage, manifest); err != nil {
		return Promoted{}, err
	}
	return promoted, nil
}
