package driver

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ContainerNspawn is what [Driver.Container] reports inside a machine.
const ContainerNspawn = "systemd-nspawn"

// NestedRoot is the sandbox root of a worker inside a machine: a subvolume
// under the machine's own ephemeral root, in the session user's home so
// that no privilege is needed to make it (docs/sandbox.md §Self-hosting).
// Everything such a worker restores, promotes or evicts lives under it,
// and it is discarded with the machine, so a nested run can never promote
// into the lineages of the worker that booted it.
const NestedRoot = "/home/agent/mill"

// ErrOutsideEphemeralRoot is a sandbox root a nested worker was given that
// is not on its machine's ephemeral root — a bind of a host path, such as
// the image directory or the workspace — and is refused before anything
// is written under it.
var ErrOutsideEphemeralRoot = errors.New("machine: a nested worker's sandbox root must be under its own ephemeral root")

// mountInfo is where the kernel lists the calling process's mounts.
const mountInfo = "/proc/self/mountinfo"

// MountPoint is the mount a path is under: the longest mount point in the
// process's mount namespace that is a prefix of the path, symlinks
// resolved. A btrfs subvolume made under a mount is not a mount of its
// own, so a subvolume under `/` answers `/`.
func MountPoint(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("machine: %s: %w", path, err)
	}
	resolved = filepath.Clean(resolved)
	file, err := os.Open(mountInfo)
	if err != nil {
		return "", fmt.Errorf("machine: %w", err)
	}
	defer func() { _ = file.Close() }()
	longest := ""
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 {
			continue
		}
		point := unescapeMount(fields[4])
		if point == resolved || point == "/" || strings.HasPrefix(resolved, strings.TrimSuffix(point, "/")+"/") {
			if len(point) > len(longest) {
				longest = point
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("machine: %s: %w", mountInfo, err)
	}
	if longest == "" {
		return "", fmt.Errorf("machine: %s: no mount holds %s", mountInfo, resolved)
	}
	return longest, nil
}

// unescapeMount undoes mountinfo's octal escapes for the characters a
// mount point may contain.
func unescapeMount(field string) string {
	replacer := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return replacer.Replace(field)
}

// CheckEphemeralRoot refuses a sandbox root for a worker inside a machine
// unless the mount holding it is the machine's root, which is the
// ephemeral snapshot. The nearest existing ancestor stands in for a root
// that is not there yet.
func CheckEphemeralRoot(root string) error {
	probe := filepath.Clean(root)
	for {
		if _, err := os.Stat(probe); err == nil {
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		probe = parent
	}
	point, err := MountPoint(probe)
	if err != nil {
		return err
	}
	if point != "/" {
		return fmt.Errorf("%w: %s is on the mount at %s", ErrOutsideEphemeralRoot, root, point)
	}
	return nil
}

// EnsureNestedRoot makes the nested root a subvolume when it is not there,
// as the worker's own uid: its parent is the session user's home, which
// every image has.
func EnsureNestedRoot(ctx context.Context, host Driver, root string) error {
	if info, err := os.Stat(root); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("machine: nested root %s is not a directory", root)
		}
		return nil
	}
	return host.CreateSubvolume(ctx, root)
}
