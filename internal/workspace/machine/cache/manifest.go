package cache

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Manifest is the worker's record of one lineage, `manifest.json` in its
// directory: its generations and what produced each, when a run last
// restored it, and the reset that emptied it if one did. It is operational
// state of the worker and never the ledger's (docs/sandbox.md §Promote).
type Manifest struct {
	// Scope is the lineage's scope.
	Scope Scope `json:"scope"`
	// Name is the lineage's name.
	Name string `json:"name"`
	// Generations are the generations on disk, ascending. A generation the
	// tree has and the manifest does not was promoted by a worker that
	// died between the rename and the write; it is listed with what is
	// known, which is its number.
	Generations []Generation `json:"generations"`
	// LastRestoredAt is when a run last restored the lineage; zero when
	// none has.
	LastRestoredAt time.Time `json:"last_restored_at"`
	// LastRestoredBy is that run.
	LastRestoredBy string `json:"last_restored_by,omitempty"`
	// Reset is the last promotion that reset the lineage instead, and why;
	// nil when none has.
	Reset *Reset `json:"reset,omitempty"`
	// Evicted is the eviction that last emptied the lineage, and why; nil
	// when none has, and cleared by the next promotion.
	Evicted *Evicted `json:"evicted,omitempty"`
}

// Generation is one read-only snapshot in a lineage.
type Generation struct {
	// Number is n in `gen-<n>`.
	Number int `json:"number"`
	// CreatedAt is when it was promoted.
	CreatedAt time.Time `json:"created_at"`
	// SizeBytes is `du -s` of the snapshot at promotion: an estimate for
	// ranking and capping, never the truth about free space.
	SizeBytes uint64 `json:"size_bytes"`
	// Run is the run that promoted it.
	Run string `json:"run"`
	// Step is the evidence-closed step whose candidate it was.
	Step string `json:"step"`
	// KeyHash is the key hash the run restored under, when the lineage
	// declares key files.
	KeyHash string `json:"key_hash,omitempty"`
}

// Reset is a promotion a global lineage's cap refused: every generation
// was deleted so the next run starts it empty (docs/sandbox.md §Promote).
type Reset struct {
	// At is when.
	At time.Time `json:"at"`
	// Run and Step are whose candidate was refused.
	Run  string `json:"run"`
	Step string `json:"step"`
	// SizeBytes is the candidate's estimate, and Cap what it exceeded.
	SizeBytes uint64 `json:"size_bytes"`
	Cap       uint64 `json:"cap"`
	// Reason is the sentence `mill cache ls` shows.
	Reason string `json:"reason"`
}

// Evicted is an eviction that deleted every generation of a lineage
// (docs/sandbox.md §Eviction). The directory and its lock stay, so a run
// that restored the lineage before promotes into it as a fresh one.
type Evicted struct {
	// At is when.
	At time.Time `json:"at"`
	// Generations are what went.
	Generations []int `json:"generations"`
	// Reason is the sentence the eviction report carries.
	Reason string `json:"reason"`
}

// LastUsed is when the lineage was last restored, or when its latest
// generation was promoted if that is later or the restore was never
// recorded: what expiry and the watermark pass rank by.
func (m Manifest) LastUsed() time.Time {
	last := m.LastRestoredAt
	if latest := m.Latest(); latest != nil && latest.CreatedAt.After(last) {
		last = latest.CreatedAt
	}
	return last
}

// Latest is the newest generation, or nil when the lineage has none.
func (m Manifest) Latest() *Generation {
	if len(m.Generations) == 0 {
		return nil
	}
	return &m.Generations[len(m.Generations)-1]
}

// manifestPath is `<lineage dir>/manifest.json`.
func (s *Store) manifestPath(lineage Lineage) string {
	return filepath.Join(s.Dir(lineage), ManifestName)
}

// Manifest reads a lineage's record, reconciled with its directory tree: a
// generation that is in the tree and not the record is listed by number,
// and one in the record and not the tree is dropped. A lineage with no
// directory has an empty manifest.
func (s *Store) Manifest(lineage Lineage) (Manifest, error) {
	manifest := Manifest{Scope: lineage.Scope, Name: lineage.Name}
	raw, err := os.ReadFile(s.manifestPath(lineage))
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &manifest); err != nil {
			return Manifest{}, fmt.Errorf("cache: %s: %w", s.manifestPath(lineage), err)
		}
		manifest.Scope, manifest.Name = lineage.Scope, lineage.Name
	case errors.Is(err, os.ErrNotExist):
	default:
		return Manifest{}, fmt.Errorf("cache: %w", err)
	}
	numbers, err := s.generations(lineage)
	if err != nil {
		return Manifest{}, err
	}
	recorded := map[int]Generation{}
	for _, generation := range manifest.Generations {
		recorded[generation.Number] = generation
	}
	manifest.Generations = manifest.Generations[:0]
	for _, n := range numbers {
		generation, ok := recorded[n]
		if !ok {
			generation = Generation{Number: n}
		}
		manifest.Generations = append(manifest.Generations, generation)
	}
	return manifest, nil
}

func (s *Store) writeManifest(lineage Lineage, manifest Manifest) error {
	return writeJSON(s.manifestPath(lineage), manifest)
}

// writeJSON writes a value as indented JSON through a temporary file and a
// rename, so a reader never sees half of it.
func writeJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("cache: %w", err)
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("cache: %s: %w", path, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("cache: %w", err)
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("cache: %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("cache: %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("cache: %w", err)
	}
	return nil
}

// readJSON reads a value written by writeJSON.
func readJSON(path string, value any) error {
	raw, err := os.ReadFile(path) //nolint:gosec // the worker's own record
	if err != nil {
		return fmt.Errorf("cache: %w", err)
	}
	if err := json.Unmarshal(raw, value); err != nil {
		return fmt.Errorf("cache: %s: %w", path, err)
	}
	return nil
}
