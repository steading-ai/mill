// Package skill materialises the mill skill where the worker mounts it.
//
// The skill and the sidecar are the worker's and change with its version;
// baked into an image they would tie a worker upgrade to a base rebuild
// (docs/adrs/018 §Rejected). Instead the worker writes the skill it was
// built with to one directory on the host — the one read-only source the
// machine helper's policy admits — and bind-mounts it into every machine
// at the harness's skill path (docs/sandbox.md §"Skill mount"). What a
// session reads under ~/.claude/skills/mill is therefore what the binary
// that started its run embeds, never a stale copy an earlier install left.
package skill

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	mill "github.com/steading-ai/mill"
)

// What the materialised directory holds beside the plugin's own files.
// Where a machine sees the directory is the machine's business
// (internal/workspace/machine/config, SkillPath).
const (
	// VersionFile is the breadcrumb: one line, the worker's version as
	// `mill version` prints it, beside SKILL.md rather than in its
	// frontmatter so the skill's frontmatter stays the plugin's
	// (docs/core-principles.md §6).
	VersionFile = "VERSION"
)

// Materialise writes the embedded skill and the version breadcrumb into
// dir, making it if it is missing, and removes whatever else an earlier
// worker left there. Each file lands by rename, so a machine that has the
// directory bound while a newer worker rewrites it reads whole files, old
// or new, and never a truncated one. Running it twice changes nothing.
func Materialise(dir, version string) error {
	if version == "" || strings.ContainsAny(version, "\n\r") {
		return fmt.Errorf("skill: version %q is not one line", version)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // bound read-only into every machine; nothing secret
		return fmt.Errorf("skill: %w", err)
	}
	wanted := map[string]bool{}
	source, err := fs.Sub(mill.Skill, mill.SkillRoot)
	if err != nil {
		return fmt.Errorf("skill: %w", err)
	}
	err = fs.WalkDir(source, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		wanted[name] = true
		target := filepath.Join(dir, filepath.FromSlash(name))
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755) //nolint:gosec // as above
		}
		body, err := fs.ReadFile(source, name)
		if err != nil {
			return err
		}
		return place(target, body, 0o644)
	})
	if err != nil {
		return fmt.Errorf("skill: %w", err)
	}
	wanted[VersionFile] = true
	if err := place(filepath.Join(dir, VersionFile), []byte(version+"\n"), 0o644); err != nil {
		return fmt.Errorf("skill: %w", err)
	}
	return prune(dir, wanted)
}

// MaterialiseSidecar copies the built sidecar at source into dest and writes
// the version breadcrumb, so the worker's sidecar rides into every machine
// beside the skill (docs/sandbox.md §"Skill mount"). Unlike the skill the
// sidecar is not embedded — its node_modules is far too large to carry in
// the binary — so it is copied from where the worker built it rather than
// written from an embedded tree. What rides is the built tree and nothing
// else: [SidecarEntrypoint] and the rest of `dist`, and `node_modules`. A
// source with no entrypoint is not a built sidecar and is refused before
// anything is written, so a misnamed source cannot leave a tree of the wrong
// thing under the share root.
//
// It is gated on the breadcrumb: a dest already carrying this version is left
// untouched, so the common startup — an unchanged binary — walks nothing.
// When the version differs the whole tree is refreshed by rename, each file
// landing atomically and an unchanged one keeping its inode, and whatever an
// earlier version left that this one does not have is pruned. That is what
// "refreshed when the binary's version differs and left alone when it does
// not" means.
func MaterialiseSidecar(source, dest, version string) error {
	if version == "" || strings.ContainsAny(version, "\n\r") {
		return fmt.Errorf("skill: sidecar version %q is not one line", version)
	}
	if info, err := os.Stat(source); err != nil || !info.IsDir() {
		return fmt.Errorf("skill: sidecar source %s is not a directory to materialise from", source)
	}
	if info, err := os.Stat(filepath.Join(source, SidecarEntrypoint)); err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("skill: sidecar source %s is not a built sidecar: it has no %s", source, SidecarEntrypoint)
	}
	if current, err := Version(dest); err == nil && current == version {
		return nil
	}
	if err := os.MkdirAll(dest, 0o755); err != nil { //nolint:gosec // bound read-only into every machine; nothing secret
		return fmt.Errorf("skill: %w", err)
	}
	wanted := map[string]bool{}
	err := filepath.WalkDir(source, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, name)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if !rides(rel) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		wanted[filepath.ToSlash(rel)] = true
		target := filepath.Join(dest, rel)
		switch {
		case entry.IsDir():
			return os.MkdirAll(target, 0o755) //nolint:gosec // as above
		case entry.Type()&fs.ModeSymlink != 0:
			// pnpm's node_modules is a tree of relative symlinks into
			// `.pnpm`, and a link is copied as a link so the tree resolves
			// the same inside the machine's mount. One that leaves the
			// source names something a machine does not have and is
			// refused: the copy would be broken where it matters.
			pointer, err := os.Readlink(name)
			if err != nil {
				return err
			}
			if !within(source, filepath.Join(filepath.Dir(name), pointer)) {
				return fmt.Errorf("sidecar link %s points outside the sidecar, to %s", rel, pointer)
			}
			return link(target, pointer)
		case entry.Type().IsRegular():
			info, err := entry.Info()
			if err != nil {
				return err
			}
			body, err := os.ReadFile(name) //nolint:gosec // the worker's own build output
			if err != nil {
				return err
			}
			// The mode rides too, so `.bin` shims and any bundled binary
			// stay executable, made world-readable as the bind is.
			return place(target, body, info.Mode().Perm()|0o444)
		default:
			return fmt.Errorf("sidecar source %s is a %s, which cannot ride into a machine", rel, entry.Type())
		}
	})
	if err != nil {
		return fmt.Errorf("skill: %w", err)
	}
	wanted[VersionFile] = true
	if err := place(filepath.Join(dest, VersionFile), []byte(version+"\n"), 0o644); err != nil {
		return fmt.Errorf("skill: %w", err)
	}
	return prune(dest, wanted)
}

// within reports whether path, resolved lexically, is under root.
func within(root, path string) bool {
	rel, err := filepath.Rel(root, filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// link makes path a symlink to pointer by rename, leaving a link that
// already says so untouched. What stood at path that was not a link is
// removed first: a rename cannot replace a directory.
func link(path, pointer string) error {
	if current, err := os.Readlink(path); err == nil && current == pointer {
		return nil
	}
	if err := clearForRename(path); err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".link")
	_ = os.Remove(tmp)
	if err := os.Symlink(pointer, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// clearForRename removes a directory standing where a file or link is about
// to be renamed in; anything else a rename replaces on its own.
func clearForRename(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() {
		return nil
	}
	return os.RemoveAll(path)
}

// SidecarEntrypoint is the sidecar's entrypoint relative to its directory:
// what the harness adapter runs, on the host or inside a machine, and the
// one file a directory must carry to count as a built sidecar.
const SidecarEntrypoint = "dist/main.js"

// sidecarTrees is what rides into a machine: the bundle and its dependencies.
// The sidecar's sources, specs and package metadata stay on the host.
var sidecarTrees = []string{"dist", "node_modules"}

// rides reports whether a path relative to the sidecar's source is part of
// the built tree that is copied.
func rides(rel string) bool {
	for _, tree := range sidecarTrees {
		if rel == tree || strings.HasPrefix(rel, tree+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// Version reads the breadcrumb a materialised directory carries.
func Version(dir string) (string, error) {
	body, err := os.ReadFile(filepath.Join(dir, VersionFile)) //nolint:gosec // the worker's own directory
	if err != nil {
		return "", fmt.Errorf("skill: %w", err)
	}
	return strings.TrimSuffix(string(body), "\n"), nil
}

// place writes body to path atomically: a sibling temporary file, then a
// rename over the target. A file already holding body is left alone, so an
// unchanged skill keeps its inode and a bound machine sees nothing move.
func place(path string, body []byte, mode fs.FileMode) error {
	if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() && info.Mode().Perm() == mode {
		if current, err := os.ReadFile(path); err == nil && string(current) == string(body) { //nolint:gosec // the worker's own directory
			return nil
		}
	}
	if err := clearForRename(path); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := write(tmp, body, mode); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// write fills a temporary file, gives it its mode, and closes it.
func write(tmp *os.File, body []byte, mode fs.FileMode) (err error) {
	defer func() {
		if closeErr := tmp.Close(); err == nil {
			err = closeErr
		}
	}()
	if _, err := tmp.Write(body); err != nil {
		return err
	}
	return tmp.Chmod(mode) //nolint:gosec // bound read-only into every machine; nothing secret
}

// prune removes what is under dir that the skill does not hold: an earlier
// version's file that was renamed away, or a placeholder. A directory the
// skill has is kept; one it does not is removed whole.
func prune(dir string, wanted map[string]bool) error {
	var stale []string
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." || wanted[filepath.ToSlash(rel)] {
			return nil
		}
		stale = append(stale, path)
		if entry.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("skill: %w", err)
	}
	for _, path := range stale {
		if err := os.RemoveAll(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("skill: %w", err)
		}
	}
	return nil
}

// MaterialiseWorkflow copies the workflow directory at source into dest, so
// the scripts a check step names beside its definition ride into every
// machine with the skill and the sidecar (docs/sandbox.md §"Skill mount").
// The directory is the operator's, not the binary's, so there is no
// breadcrumb to gate on: every call walks the source and dest ends up its
// copy, each file landing by rename and an unchanged one keeping its inode,
// with whatever an earlier directory had that this one does not pruned.
// Symlinks are followed rather than carried — the acceptance suite stages a
// workflow as links into the tree it derives from, and a link into the host
// would name nothing inside a machine — and a file's mode rides so a
// script stays executable.
func MaterialiseWorkflow(source, dest string) error {
	if info, err := os.Stat(source); err != nil || !info.IsDir() {
		return fmt.Errorf("skill: workflow source %s is not a directory to materialise from", source)
	}
	if err := os.MkdirAll(dest, 0o755); err != nil { //nolint:gosec // bound read-only into every machine; nothing secret
		return fmt.Errorf("skill: %w", err)
	}
	wanted := map[string]bool{}
	if err := copyResolved(source, dest, "", wanted, 0); err != nil {
		return fmt.Errorf("skill: %w", err)
	}
	return prune(dest, wanted)
}

// copyDepth bounds the walk of a workflow directory through its links: a
// tree deeper than this is a cycle, not a workflow.
const copyDepth = 32

// copyResolved copies the directory at source/rel into dest/rel with every
// link resolved, recording what it placed in wanted.
func copyResolved(source, dest, rel string, wanted map[string]bool, depth int) error {
	if depth > copyDepth {
		return fmt.Errorf("workflow source %s is deeper than %d levels: a link cycle", filepath.Join(source, rel), copyDepth)
	}
	entries, err := os.ReadDir(filepath.Join(source, rel))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		child := filepath.Join(rel, entry.Name())
		info, err := os.Stat(filepath.Join(source, child))
		if err != nil {
			return err
		}
		target := filepath.Join(dest, child)
		switch {
		case info.IsDir():
			wanted[filepath.ToSlash(child)] = true
			if err := os.MkdirAll(target, 0o755); err != nil { //nolint:gosec // as above
				return err
			}
			if err := copyResolved(source, dest, child, wanted, depth+1); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			wanted[filepath.ToSlash(child)] = true
			body, err := os.ReadFile(filepath.Join(source, child)) //nolint:gosec // the operator's workflow directory
			if err != nil {
				return err
			}
			if err := place(target, body, info.Mode().Perm()|0o444); err != nil {
				return err
			}
		default:
			return fmt.Errorf("workflow source %s is a %s, which cannot ride into a machine", child, info.Mode().Type())
		}
	}
	return nil
}
