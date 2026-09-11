// Package image is the image a machine boots from (docs/sandbox.md
// §Images): the base the operator publishes to Harbor, resolved to a digest
// at each provision, and the derived image per repository — base plus the
// packages its mill.toml declares — built on the worker on first use and
// exported as the btrfs subvolume systemd-nspawn snapshots.
//
// The derived digest is a pure function of the base digest and the package
// list, so two runs that want the same image want the same subvolume, and
// one lock per digest makes them build it once. A build that fails never
// fails the run: the run boots from base and its first session receives a
// [Finding] carrying what dnf said.
//
// Every host command goes through [driver.Driver]. The build is the one
// place the worker runs a container outside a machine; the driver's
// BuildImageOnHost and this package's buildOnHostOutsideAnyMachine are
// named for it.
package image

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/ivy/mill/internal/workspace/machine/driver"
)

// Defaults for what the worker's configuration leaves unsaid.
const (
	// DefaultRegistry is the Harbor host the base is published to and
	// pulled from.
	DefaultRegistry = "harbor.tailcbcc0e.ts.net"
	// DefaultRepository is the base image's repository on that host; tags
	// are dates and `latest`.
	DefaultRepository = "mill/base"
	// DefaultTag is what is resolved when nothing pins a tag or digest.
	DefaultTag = "latest"
	// DefaultBuildBudget bounds one derived build; past it the run boots
	// from base.
	DefaultBuildBudget = 10 * time.Minute
	// DefaultExpiry is how long an image subvolume not booted goes before
	// `mill image prune` removes it (docs/sandbox.md §Eviction).
	DefaultExpiry = 30 * 24 * time.Hour
	// Prefix is what an image subvolume's name starts with; the digest
	// follows (docs/sandbox.md §Naming).
	Prefix = "mill-image-"
	// BuildingSuffix marks a subvolume an export is still landing in. One
	// that exists at worker start is a build that died.
	BuildingSuffix = ".building"
	// TailLines is how many of a failed build's last lines a finding
	// carries.
	TailLines = 20
)

// Digest names an image: the sha256 of the base's manifest as the registry
// spells it, or the derived hash, either way sixty-four lowercase hex
// characters and nothing else, because it becomes a path component and the
// host's machine helper admits only `[a-z0-9]+`.
type Digest string

var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ParseDigest reads a digest with or without the `sha256:` prefix a
// registry puts on it.
func ParseDigest(raw string) (Digest, error) {
	raw = strings.TrimSpace(raw)
	hexPart := strings.TrimPrefix(raw, "sha256:")
	if !digestPattern.MatchString(hexPart) {
		return "", fmt.Errorf("image: %q is not a sha256 digest", raw)
	}
	return Digest(hexPart), nil
}

// OCI is the digest as an image reference spells it: `sha256:<hex>`.
func (d Digest) OCI() string { return "sha256:" + string(d) }

// Short is the digest's first twelve characters, for a log line or a table.
func (d Digest) Short() string {
	if len(d) > 12 {
		return string(d[:12])
	}
	return string(d)
}

// Variant is a change to a derived image beyond its package list. There
// is one: [Nested].
type Variant string

// Nested is the variant a machine granted `[machine].nested` boots
// (docs/sandbox.md §Provision, §Self-hosting): `agent` has passwordless
// sudo for everything, because root inside the user namespace is an
// unprivileged uid on the host and the namespace is the boundary; and
// systemd-nspawn and btrfs-progs are installed, because booting a machine
// of its own is what the grant is for. No other image carries the sudoers
// line: the shim's `dnf install` rule is the only escalation elsewhere.
const Nested Variant = "nested"

// NestedPackages is what the nested variant installs over the repository's
// own list.
var NestedPackages = []string{"btrfs-progs", "systemd-container"}

// NestedSudoers is the sudoers.d file the nested variant writes.
const NestedSudoers = "/etc/sudoers.d/agent-nested"

// Derived is the digest of base plus packages: `sha256(base ‖ sorted,
// deduplicated package list)`, or base itself when the list is empty and
// there is no variant, so a repository declaring nothing boots the base
// subvolume (docs/sandbox.md §"Derived images"). Order and repetition in
// the list do not matter. The nested variant hashes its own name after
// the list, so a nested image and the flat image of the same packages are
// two subvolumes.
func Derived(base Digest, packages []string, variants ...Variant) Digest {
	packages = Packages(packages)
	nested := hasNested(variants)
	if nested {
		packages = Packages(append(packages, NestedPackages...))
	}
	if len(packages) == 0 && !nested {
		return base
	}
	h := sha256.New()
	h.Write([]byte(base))
	for _, name := range packages {
		h.Write([]byte{'\n'})
		h.Write([]byte(name))
	}
	if nested {
		h.Write([]byte("\n+" + Nested))
	}
	return Digest(hex.EncodeToString(h.Sum(nil)))
}

// hasNested is whether the variants name [Nested].
func hasNested(variants []Variant) bool {
	return slices.Contains(variants, Nested)
}

// Packages is the list sorted and deduplicated, with blanks dropped: the
// form the digest is computed over and the manifest records.
func Packages(packages []string) []string {
	out := make([]string, 0, len(packages))
	for _, name := range packages {
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Config is what the worker's configuration says about images.
type Config struct {
	// Root is the sandbox root, [driver.DefaultRoot] when empty. Locks,
	// manifests and the last resolved base live under `<Root>/images`.
	Root string
	// Machines is the image subvolume directory,
	// [driver.DefaultMachines] when empty.
	Machines string
	// Registry is the Harbor host, [DefaultRegistry] when empty.
	Registry string
	// Repository is the base image's repository on it, [DefaultRepository]
	// when empty.
	Repository string
	// Pin is a tag or a digest from worker configuration that stands in for
	// `latest`: the rollback (docs/sandbox.md §Base). Empty resolves the
	// default tag.
	Pin string
	// BuildBudget bounds one derived build, [DefaultBuildBudget] when zero.
	BuildBudget time.Duration
	// Now is the clock manifests are stamped by; nil is time.Now.
	Now func() time.Time
}

func (c Config) withDefaults() Config {
	if c.Root == "" {
		c.Root = driver.DefaultRoot
	}
	if c.Machines == "" {
		c.Machines = driver.DefaultMachines
	}
	if c.Registry == "" {
		c.Registry = DefaultRegistry
	}
	if c.Repository == "" {
		c.Repository = DefaultRepository
	}
	if c.BuildBudget <= 0 {
		c.BuildBudget = DefaultBuildBudget
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

// Store is the images on one worker: the subvolumes under the machines
// directory and the worker's records of them.
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

// Path is the subvolume a digest boots from:
// `<machines>/mill-image-<digest>`.
func (s *Store) Path(digest Digest) string {
	return filepath.Join(s.config.Machines, Prefix+string(digest))
}

// building is where an export lands before it is renamed into place.
func (s *Store) building(digest Digest) string {
	return s.Path(digest) + BuildingSuffix
}

// dir is the worker's own image directory: locks, manifests, the base
// record. It is the worker's because the machines directory is root's.
func (s *Store) dir() string {
	return filepath.Join(s.config.Root, "images")
}

// BaseReference is the image the base resolves from: `<registry>/<repo>:<tag>`,
// or `<registry>/<repo>@sha256:<digest>` for a pinned digest.
func (s *Store) BaseReference(digest Digest) string {
	return s.config.Registry + "/" + s.config.Repository + "@" + digest.OCI()
}

// ErrNoBase is a base that could not be resolved from the registry and has
// no subvolume on this host to fall back to. Nothing can boot; the run
// fails before any machine starts (docs/sandbox.md §"Failure modes").
var ErrNoBase = errors.New("image: no base image")
