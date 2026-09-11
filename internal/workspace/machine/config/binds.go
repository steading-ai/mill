package config

import (
	"path/filepath"

	"github.com/ivy/mill/internal/workspace"
)

// Bind is one bind mount into the machine, host side to machine side
// (docs/sandbox.md §Filesystem).
type Bind struct {
	// Host is the path on the host.
	Host string
	// Machine is the path inside the machine.
	Machine string
	// ReadOnly marks a bind the machine may not write.
	ReadOnly bool
}

// Mounts is where the worker keeps what a run's binds come from: nothing a
// repository writes reaches this.
type Mounts struct {
	// Run is the run's directory under the sandbox root, `runs/<id>`: its
	// workspace and cache snapshots live under it (docs/sandbox.md §Naming).
	Run string
	// Socket is the run's socket directory on the host, `/run/mill/<id>`.
	Socket string
	// Skill is the worker's skill directory, mounted read-only.
	Skill string
	// Sidecar is the worker's materialised sidecar directory, mounted
	// read-only at the path the image owns (docs/sandbox.md §"Skill mount").
	Sidecar string
}

// SkillPath is where the mill skill is mounted inside the machine
// (docs/sandbox.md §"Skill mount").
const SkillPath = Home + "/.claude/skills/mill"

// SidecarPath is where the worker's sidecar is mounted read-only inside the
// machine — the mount point the base image owns (docs/sandbox.md §"Skill
// mount"). The harness adapter's in-machine entrypoint is under it, and the
// image's `node` runs it.
const SidecarPath = Home + "/.local/share/mill/sidecar"

// Binds derives a run's bind mounts from its configuration. Every host
// side is under the run directory, the socket directory or the skill
// directory: a cache's host side is named by the lineage, whose name
// [Load] has already checked is one path component, and its machine side
// has already passed [Load]'s path rules.
func Binds(config Config, mounts Mounts) []Bind {
	binds := []Bind{
		{Host: filepath.Join(mounts.Run, "workspace"), Machine: Work},
		{Host: mounts.Socket, Machine: RunDir},
		{Host: mounts.Skill, Machine: SkillPath, ReadOnly: true},
		{Host: mounts.Sidecar, Machine: SidecarPath, ReadOnly: true},
	}
	for _, cache := range config.Caches {
		binds = append(binds, Bind{
			Host:    filepath.Join(mounts.Run, "cache", cache.Name),
			Machine: cache.Path,
		})
	}
	return binds
}

// CacheBinds is the configuration's caches as a [workspace.Spec] carries
// them.
func CacheBinds(config Config) []workspace.CacheBind {
	binds := make([]workspace.CacheBind, 0, len(config.Caches))
	for _, cache := range config.Caches {
		binds = append(binds, workspace.CacheBind{
			Name:   cache.Name,
			Path:   cache.Path,
			Global: cache.Scope == Global,
		})
	}
	return binds
}
