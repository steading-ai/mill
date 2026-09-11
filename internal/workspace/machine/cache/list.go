package cache

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Summary is one lineage as `mill cache ls` shows it.
type Summary struct {
	// Lineage is the scope and name.
	Lineage Lineage
	// Generations are the generation numbers on disk, ascending.
	Generations []int
	// Latest is the newest generation's record; nil when there is none.
	Latest *Generation
	// LastRestoredAt is when a run last restored the lineage; zero when
	// none has.
	LastRestoredAt time.Time
	// LastUsed is [Manifest.LastUsed]: what eviction ranks by.
	LastUsed time.Time
	// OverCap says the latest generation's estimate exceeds the scope's
	// cap: a global lineage at the edge of a reset, a repo lineage worth a
	// look (docs/sandbox.md §Eviction).
	OverCap bool
	// Reset is the reset that last emptied the lineage, if one did.
	Reset *Reset
	// Evicted is the eviction that last emptied the lineage, if one did.
	Evicted *Evicted
}

// List is every lineage under the lineage root — global first, then by
// scope and name — read from the directory tree and each manifest, never
// from btrfs.
func (s *Store) List() ([]Summary, error) {
	var lineages []Lineage
	globals, err := s.names(filepath.Join(s.LineageRoot(), string(Global)))
	if err != nil {
		return nil, err
	}
	for _, name := range globals {
		lineages = append(lineages, Lineage{Scope: Global, Name: name})
	}
	repos, err := s.names(filepath.Join(s.LineageRoot(), "repo"))
	if err != nil {
		return nil, err
	}
	for _, id := range repos {
		names, err := s.names(filepath.Join(s.LineageRoot(), "repo", id))
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			lineages = append(lineages, Lineage{Scope: Repo(id), Name: name})
		}
	}
	var out []Summary
	for _, lineage := range lineages {
		manifest, err := s.Manifest(lineage)
		if err != nil {
			return nil, err
		}
		summary := Summary{Lineage: lineage, Latest: manifest.Latest(), LastRestoredAt: manifest.LastRestoredAt, LastUsed: manifest.LastUsed(), Reset: manifest.Reset, Evicted: manifest.Evicted}
		for _, generation := range manifest.Generations {
			summary.Generations = append(summary.Generations, generation.Number)
		}
		if summary.Latest != nil && summary.Latest.SizeBytes > s.config.Cap(lineage) {
			summary.OverCap = true
		}
		out = append(out, summary)
	}
	return out, nil
}

// names is the subdirectories of dir, sorted; none when dir is absent.
func (s *Store) names(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, entry := range entries {
		if entry.IsDir() && component.MatchString(entry.Name()) {
			out = append(out, entry.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}
