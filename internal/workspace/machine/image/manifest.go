package image

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Manifest is the worker's record of one image subvolume: what it was built
// from, when, and when a machine last booted it, which is what eviction
// ranks by (docs/sandbox.md §Eviction).
type Manifest struct {
	// Digest names the subvolume, `mill-image-<digest>`.
	Digest Digest `json:"digest"`
	// Base is the base digest it was derived from; the digest itself for
	// the base's own subvolume.
	Base Digest `json:"base"`
	// Packages are what dnf installed over the base, sorted; none for the
	// base.
	Packages []string `json:"packages"`
	// BuiltAt is when the export was renamed into place.
	BuiltAt time.Time `json:"built_at"`
	// LastBootedAt is when a machine last booted from it; zero when none
	// has.
	LastBootedAt time.Time `json:"last_booted_at"`
}

// LastUsed is what expiry measures from: the last boot, or the build when
// nothing has booted it.
func (m Manifest) LastUsed() time.Time {
	if m.LastBootedAt.IsZero() {
		return m.BuiltAt
	}
	return m.LastBootedAt
}

// manifestPath is `<root>/images/<digest>.json`: beside the lock, in the
// worker's directory, because the subvolume's own directory is root's.
func (s *Store) manifestPath(digest Digest) string {
	return filepath.Join(s.dir(), string(digest)+".json")
}

func (s *Store) writeManifest(manifest Manifest) error {
	return writeJSON(s.manifestPath(manifest.Digest), manifest)
}

// Manifest reads one image's record.
func (s *Store) Manifest(digest Digest) (Manifest, error) {
	raw, err := os.ReadFile(s.manifestPath(digest))
	if err != nil {
		return Manifest{}, fmt.Errorf("image: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("image: %s: %w", s.manifestPath(digest), err)
	}
	return manifest, nil
}

// Booted records that a machine booted from the image now: the provision
// calls it, and it is what keeps an image in use from expiring.
func (s *Store) Booted(digest Digest) error {
	manifest, err := s.Manifest(digest)
	if err != nil {
		return err
	}
	manifest.LastBootedAt = s.config.Now()
	return s.writeManifest(manifest)
}

// List is every image subvolume this worker built and that is still on the
// host, oldest use first. A record whose subvolume is gone is not listed:
// the subvolume is the image, the record is the worker's note about it.
func (s *Store) List(ctx context.Context) ([]Manifest, error) {
	entries, err := os.ReadDir(s.dir())
	if err != nil {
		if isNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("image: %w", err)
	}
	var out []Manifest
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") || name == baseRecordName || strings.HasPrefix(name, ".") {
			continue
		}
		digest, err := ParseDigest(strings.TrimSuffix(name, ".json"))
		if err != nil {
			continue
		}
		manifest, err := s.Manifest(digest)
		if err != nil {
			return nil, err
		}
		present, err := s.driver.Exists(ctx, s.Path(digest))
		if err != nil {
			return nil, err
		}
		if !present {
			continue
		}
		out = append(out, manifest)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastUsed().Before(out[j].LastUsed()) })
	return out, nil
}

// Prune deletes every image subvolume whose last use is older than expiry
// and returns what went. Each deletion holds the image's build lock, so a
// provision ensuring the same digest either finds it or rebuilds it, never
// half of it. Watermarks are the eviction node's; this is expiry alone
// (docs/sandbox.md §Eviction).
func (s *Store) Prune(ctx context.Context, expiry time.Duration) ([]Manifest, error) {
	manifests, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	cutoff := s.config.Now().Add(-expiry)
	var pruned []Manifest
	for _, manifest := range manifests {
		if !manifest.LastUsed().Before(cutoff) {
			continue
		}
		if err := s.remove(ctx, manifest.Digest); err != nil {
			return pruned, err
		}
		pruned = append(pruned, manifest)
	}
	return pruned, nil
}

// remove deletes one image under its lock: the subvolume, then the record.
func (s *Store) remove(ctx context.Context, digest Digest) error {
	unlock, err := s.lock(ctx, digest)
	if err != nil {
		return err
	}
	defer unlock()
	if err := s.driver.DeleteSubvolume(ctx, s.Path(digest)); err != nil {
		return err
	}
	if err := os.Remove(s.manifestPath(digest)); err != nil && !isNotExist(err) {
		return fmt.Errorf("image: %w", err)
	}
	return nil
}
