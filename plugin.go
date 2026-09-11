// Package mill is the repository root, and holds what the binary carries of
// the tree beside its own code: the mill plugin's skill, embedded so the
// worker can materialise it on a host that has no checkout.
//
// The skill lives under plugin/ because a Claude Code plugin is a directory
// with that shape, and `go:embed` cannot reach above the package that names
// it — so the one package at the root is where the embed goes. Everything
// that reads it is under internal/.
package mill

import "embed"

// Skill is the mill skill as the plugin ships it: plugin/mill/skills/mill,
// with `SKILL.md` and its references. The worker writes it to the directory
// it bind-mounts read-only into every machine (internal/skill,
// docs/sandbox.md §"Skill mount"), so the skill a session reads is the one
// versioned with the worker that started it.
//
//go:embed plugin/mill/skills/mill
var Skill embed.FS

// SkillRoot is the path inside [Skill] the skill's own files are under.
const SkillRoot = "plugin/mill/skills/mill"
