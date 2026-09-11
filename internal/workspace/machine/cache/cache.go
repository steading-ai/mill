// Package cache is the lineage of docs/sandbox.md §Caching: a named
// directory whose state is carried between runs as read-only btrfs
// snapshots, one generation per promotion, under
// `<root>/cache/<scope>/<name>/gen-<n>` with a `manifest.json` beside them.
//
// A run restores each lineage it uses as a writable snapshot of the latest
// generation into its own directory, takes a read-only candidate of every
// copy each time a step closes on evidence, and at its end promotes the
// last candidate into the next generation. Restore and promote are one
// subvolume operation each, whatever the size, which is why the store is
// btrfs and not an archive.
//
// Nothing here is the ledger's. The manifest is the worker's operational
// state; what a run restored and promoted comes back to the caller as
// [Restored] and [Promoted] for the ledger to note, and nothing else about
// a lineage is recorded anywhere but under its directory.
//
// Every subvolume operation goes through [driver.Driver], as the worker's
// own uid: a read-only generation is made writable before it is deleted,
// generations are enumerated from the directory tree and never from btrfs,
// and nothing runs through sudo.
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ivy/mill/internal/workspace/machine/driver"
)

// The layout under the sandbox root (docs/sandbox.md §Naming).
const (
	// Keep is how many generations a lineage holds after promotion: the
	// newest two (docs/sandbox.md §Eviction).
	Keep = 2
	// GenerationPrefix names a generation's subvolume: `gen-<n>`.
	GenerationPrefix = "gen-"
	// ManifestName is the lineage's record, beside its generations.
	ManifestName = "manifest.json"
	// RecordName is a run's record of what it restored and which step's
	// candidate it holds, under the run directory.
	RecordName = "cache.json"
	// LockName is the file a promotion and a restore lock, under the
	// lineage directory.
	LockName = "lock"
	// DefaultGlobalCap is the size estimate past which a global lineage is
	// reset rather than promoted (docs/sandbox.md §Promote).
	DefaultGlobalCap = 20 << 30
	// DefaultRepoCap is the size estimate past which `mill cache ls` flags
	// a repo-scoped lineage; never a gate on promotion (docs/sandbox.md
	// §Eviction).
	DefaultRepoCap = 10 << 30
)

// Scope is who shares a lineage, as its directory under the lineage root
// spells it: `global`, or `repo/<repo-id>` (docs/sandbox.md §Naming).
type Scope string

// Global is the scope every run on the worker shares.
const Global Scope = "global"

// repoPrefix is what a repo-scoped scope starts with.
const repoPrefix = "repo/"

// Repo is the scope of one repository's build state, from its id.
func Repo(id string) Scope { return Scope(repoPrefix + id) }

// RepoID is the id a repository's origin gets in a scope: a slug of the
// origin with its scheme, user and `.git` dropped and every character that
// is not a path-safe one replaced, then the first eight hex characters of
// the origin's sha256, so two origins that slug alike stay apart and the
// directory still reads as the repository.
func RepoID(origin string) string {
	trimmed := strings.TrimSpace(origin)
	sum := sha256.Sum256([]byte(trimmed))
	slug := trimmed
	if i := strings.Index(slug, "://"); i >= 0 {
		slug = slug[i+3:]
	}
	if i := strings.Index(slug, "@"); i >= 0 {
		slug = slug[i+1:]
	}
	slug = strings.TrimSuffix(strings.TrimSuffix(slug, "/"), ".git")
	slug = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_':
			return r
		default:
			return '-'
		}
	}, slug)
	slug = strings.Trim(slug, "-.")
	if slug == "" {
		slug = "repo"
	}
	return slug + "-" + hex.EncodeToString(sum[:4])
}

// IsGlobal reports whether the scope is [Global].
func (s Scope) IsGlobal() bool { return s == Global }

// component is what a lineage name or a repo id may be: one path component
// with nothing in it a shell or a filesystem reads specially. The name
// rule is config's; the id rule is the same.
var component = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// validate reports a scope that is not `global` or `repo/<id>` with a
// path-safe id.
func (s Scope) validate() error {
	if s.IsGlobal() {
		return nil
	}
	id, ok := strings.CutPrefix(string(s), repoPrefix)
	if !ok || !component.MatchString(id) {
		return fmt.Errorf("cache: scope %q is not global or repo/<id>", s)
	}
	return nil
}

// Lineage names one lineage a run uses: its scope and name, and the hash of
// the key files declared for it, empty when it has none (docs/sandbox.md
// §mill.toml).
type Lineage struct {
	// Scope is `global` or `repo/<id>`.
	Scope Scope `json:"scope"`
	// Name identifies the lineage within its scope, and names the run's
	// copy under `runs/<id>/cache/<name>`.
	Name string `json:"name"`
	// KeyHash is [HashKey] of the declared key files in the checkout. A
	// generation promoted with one hash does not restore for another.
	KeyHash string `json:"key_hash,omitempty"`
}

func (l Lineage) validate() error {
	if err := l.Scope.validate(); err != nil {
		return err
	}
	if !component.MatchString(l.Name) {
		return fmt.Errorf("cache: lineage name %q is not one path component", l.Name)
	}
	return nil
}

// String is `<scope>/<name>`.
func (l Lineage) String() string { return string(l.Scope) + "/" + l.Name }

// HashKey is the hash a lineage's key files carry: sha256 over each file's
// path and content in the order declared, with a file that is not in the
// checkout hashed as absent rather than failing, since an absent key file
// is a state of the checkout like any other. No files hash to nothing.
func HashKey(checkout string, files []string) (string, error) {
	if len(files) == 0 {
		return "", nil
	}
	h := sha256.New()
	for _, name := range files {
		h.Write([]byte(name))
		h.Write([]byte{0})
		content, err := os.ReadFile(filepath.Join(checkout, name)) //nolint:gosec // a declared file in the checkout
		switch {
		case err == nil:
			h.Write(content)
		case errors.Is(err, os.ErrNotExist):
			h.Write([]byte("absent"))
		default:
			return "", fmt.Errorf("cache: key file %s: %w", name, err)
		}
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Config is what the worker's configuration says about lineages.
type Config struct {
	// Root is the sandbox root, [driver.DefaultRoot] when empty. Lineages
	// live under `<Root>/cache`, run copies under `<Root>/runs/<id>`.
	Root string
	// GlobalCap is the size estimate a global lineage may not exceed at
	// promotion; over it, the lineage is reset. [DefaultGlobalCap] when
	// zero.
	GlobalCap uint64
	// RepoCap is the size estimate past which a repo-scoped lineage is
	// flagged by `mill cache ls`. [DefaultRepoCap] when zero.
	RepoCap uint64
	// Now is the clock manifests are stamped by; nil is time.Now.
	Now func() time.Time
}

func (c Config) withDefaults() Config {
	if c.Root == "" {
		c.Root = driver.DefaultRoot
	}
	if c.GlobalCap == 0 {
		c.GlobalCap = DefaultGlobalCap
	}
	if c.RepoCap == 0 {
		c.RepoCap = DefaultRepoCap
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

// Cap is the cap that applies to a scope.
func (c Config) Cap(scope Scope) uint64 {
	if scope.IsGlobal() {
		return c.GlobalCap
	}
	return c.RepoCap
}

// Store is the lineages on one worker, reached through the driver.
type Store struct {
	host   driver.Driver
	config Config
}

// New is a store over the host the driver reaches.
func New(host driver.Driver, config Config) *Store {
	return &Store{host: host, config: config.withDefaults()}
}

// Config is the configuration in effect, defaults filled.
func (s *Store) Config() Config { return s.config }

// LineageRoot is `<root>/cache`, the directory every lineage is under. A
// promotion addressed anywhere else is refused (docs/sandbox.md §Promote).
func (s *Store) LineageRoot() string { return filepath.Join(s.config.Root, "cache") }

// Dir is a lineage's directory, `<root>/cache/<scope>/<name>`.
func (s *Store) Dir(lineage Lineage) string {
	return filepath.Join(s.LineageRoot(), filepath.FromSlash(string(lineage.Scope)), lineage.Name)
}

// GenerationPath is `<lineage dir>/gen-<n>`.
func (s *Store) GenerationPath(lineage Lineage, n int) string {
	return filepath.Join(s.Dir(lineage), GenerationPrefix+strconv.Itoa(n))
}

// RunDir is `<root>/runs/<run-id>`.
func (s *Store) RunDir(run string) string { return filepath.Join(s.config.Root, "runs", run) }

// CopyPath is the run's writable copy of a lineage,
// `<root>/runs/<run-id>/cache/<name>`: what the machine binds at the cache's
// declared path.
func (s *Store) CopyPath(run, name string) string {
	return filepath.Join(s.RunDir(run), "cache", name)
}

// CandidatePath is the read-only snapshot of the run's copy taken at the
// last evidence-closed step, `<root>/runs/<run-id>/candidate/<name>`.
func (s *Store) CandidatePath(run, name string) string {
	return filepath.Join(s.RunDir(run), "candidate", name)
}

// candidateNext is where a new candidate lands before it replaces the one
// it supersedes.
func (s *Store) candidateNext(run, name string) string {
	return filepath.Join(s.RunDir(run), "candidate.next", name)
}

// generations reads a lineage's generation numbers from its directory
// tree, ascending. The tree is the truth; the manifest is the record.
func (s *Store) generations(lineage Lineage) ([]int, error) {
	entries, err := os.ReadDir(s.Dir(lineage))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("cache: %w", err)
	}
	var out []int
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		raw, ok := strings.CutPrefix(entry.Name(), GenerationPrefix)
		if !ok {
			continue
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			continue
		}
		out = append(out, n)
	}
	sort.Ints(out)
	return out, nil
}

// lock takes `<lineage dir>/lock` with flock, waiting for a holder to
// finish; the returned function releases it. Two runs promoting into one
// lineage at once serialise here, so each takes the next number and the
// later one is latest (docs/sandbox.md §Promote).
func (s *Store) lock(ctx context.Context, lineage Lineage) (func(), error) {
	dir := s.Dir(lineage)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("cache: %w", err)
	}
	path := filepath.Join(dir, LockName)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o640) //nolint:gosec // the lock file is the worker's own
	if err != nil {
		return nil, fmt.Errorf("cache: %w", err)
	}
	locked := make(chan error, 1)
	go func() { locked <- syscall.Flock(int(file.Fd()), syscall.LOCK_EX) }()
	select {
	case err := <-locked:
		if err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("cache: lock %s: %w", path, err)
		}
		return func() { _ = file.Close() }, nil
	case <-ctx.Done():
		// flock cannot be interrupted; the lock, once granted, is released
		// by the close that follows it.
		go func() {
			<-locked
			_ = file.Close()
		}()
		return nil, fmt.Errorf("cache: waiting for the lock on %s: %w", path, ctx.Err())
	}
}

// deleteReadOnly makes a read-only subvolume writable and deletes it: a
// generation or a candidate is read-only, and its owner cannot delete it
// while it is (docs/spikes/nspawn-per-run-sandbox.md E9).
func (s *Store) deleteReadOnly(ctx context.Context, path string, sync bool) error {
	if err := s.host.SetReadOnly(ctx, path, false); err != nil {
		return err
	}
	return s.host.DeleteSubvolume(ctx, path, sync)
}
