package machine

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ManifestName is the file the worker writes and the template unit's
// helper reads: `/var/lib/mill/runs/<id>/machine.json` (docs/sandbox.md
// §Provision).
const ManifestName = "machine.json"

// Manifest is `machine.json`: everything a run's machine is, declared by
// the worker and validated by `/usr/libexec/mill-machine` before nspawn
// runs. The schema is the helper's (homestead's mill_machine role) and
// docs/sandbox.md §Provision reproduces it; a key the helper does not know
// fails the unit at start with the field named in its journal, so this
// type carries exactly those keys and no others.
type Manifest struct {
	// Image is the derived digest the machine boots from: sixty-four
	// lowercase hex characters naming `/var/lib/machines/mill-image-<Image>`.
	Image string `json:"image"`
	// Binds are the host paths mounted into the machine, every one with
	// `:idmap` so uid 1000 is uid 1000 on both sides. Nothing can be added
	// once the unit has started: `machinectl bind` refuses a
	// user-namespaced machine.
	Binds []Bind `json:"binds"`
	// Limits bound the whole run, containers included, because the
	// machine's cgroup tree is the unit's (docs/sandbox.md §"Resource
	// limits"). A limit left empty is the unit's default.
	Limits Limits `json:"limits"`
	// Nested marks a machine that boots machines of its own: the helper
	// binds the host's sysfs at HostSysfsTarget for it, and admits the
	// nested read-only sources only with it (docs/sandbox.md §Provision).
	Nested bool `json:"nested"`
}

// Bind is one bind mount, host side to machine side.
type Bind struct {
	// Source is the host path. The helper admits a source under the run
	// directory, under `/run/mill/<id>`, or one of its read-only sources —
	// the worker's skill directory — and refuses everything else.
	Source string `json:"source"`
	// Target is where the machine sees it; absolute, never `/`.
	Target string `json:"target"`
	// ReadOnly makes it `--bind-ro`. Required for a read-only source.
	ReadOnly bool `json:"readonly,omitempty"`
}

// Limits are the unit's resource limits, in the forms
// systemd.resource-control(5) accepts.
type Limits struct {
	// MemoryMax such as `8G`.
	MemoryMax string `json:"MemoryMax,omitempty"`
	// CPUQuota such as `400%`.
	CPUQuota string `json:"CPUQuota,omitempty"`
	// TasksMax such as `4096`.
	TasksMax string `json:"TasksMax,omitempty"`
}

// HostSysfsTarget is where the helper binds the host's sysfs in a nested
// machine, for an inner nspawn to pivot with. It is the helper's own and no
// manifest bind may take it.
const HostSysfsTarget = "/.hostsys"

// imagePattern is what the helper accepts as an image digest.
var imagePattern = regexp.MustCompile(`^[a-z0-9]+$`)

// ErrManifest is a manifest the helper would refuse: caught here so the
// refusal names the field before a unit is started to fail.
var ErrManifest = errors.New("machine: machine.json would be refused")

// Check is the helper's validation, ahead of it: the image is a digest,
// every bind's source is absolute and under one of the allowed roots, a
// source under a read-only root is bound read-only, and every target is
// absolute, not `/` and not the helper's own HostSysfsTarget. allowed maps
// each root to whether binds under it must be read-only. What it cannot
// check — that the paths exist as root sees them — the helper does.
func (m Manifest) Check(allowed map[string]bool) error {
	if !imagePattern.MatchString(m.Image) {
		return fmt.Errorf("%w: image %q is not a digest matching ^[a-z0-9]+$", ErrManifest, m.Image)
	}
	for i, bind := range m.Binds {
		field := fmt.Sprintf("binds[%d]", i)
		if !filepath.IsAbs(bind.Source) || filepath.Clean(bind.Source) != bind.Source {
			return fmt.Errorf("%w: %s.source %q is not an absolute, normalised path", ErrManifest, field, bind.Source)
		}
		if !filepath.IsAbs(bind.Target) || filepath.Clean(bind.Target) != bind.Target || bind.Target == "/" {
			return fmt.Errorf("%w: %s.target %q is not an absolute, normalised path other than /", ErrManifest, field, bind.Target)
		}
		if bind.Target == HostSysfsTarget {
			return fmt.Errorf("%w: %s.target %s is reserved for the helper's sysfs bind", ErrManifest, field, bind.Target)
		}
		root, readOnly, ok := underAllowed(bind.Source, allowed)
		if !ok {
			return fmt.Errorf("%w: %s.source %s is outside every allowed root", ErrManifest, field, bind.Source)
		}
		if readOnly && !bind.ReadOnly {
			return fmt.Errorf("%w: %s.source %s is under %s and may only be bound read-only", ErrManifest, field, bind.Source, root)
		}
	}
	return nil
}

// underAllowed is the allowed root a path is under, and whether that root
// is read-only.
func underAllowed(path string, allowed map[string]bool) (root string, readOnly, ok bool) {
	for root, readOnly := range allowed {
		if path == root || strings.HasPrefix(path, strings.TrimSuffix(root, "/")+"/") {
			return root, readOnly, true
		}
	}
	return "", false, false
}

// Write writes the manifest where the helper reads it, readable by root
// and the worker.
func (m Manifest) Write(path string) error {
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("machine: encode %s: %w", path, err)
	}
	if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil { //nolint:gosec // root reads it; it holds nothing secret
		return fmt.Errorf("machine: write %s: %w", path, err)
	}
	return nil
}

// ReadManifest reads a manifest the worker wrote.
func ReadManifest(path string) (Manifest, error) {
	body, err := os.ReadFile(path) //nolint:gosec // the run directory is the worker's own
	if err != nil {
		return Manifest{}, fmt.Errorf("machine: read %s: %w", path, err)
	}
	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return Manifest{}, fmt.Errorf("machine: decode %s: %w", path, err)
	}
	return m, nil
}
