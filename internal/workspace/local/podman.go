package local

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// writeContainerShims writes bin/podman and bin/docker — whichever of the
// two is on the mill's own PATH — as wrappers that inject `--label
// mill.attempt=<name>` (and, for podman only, `--cgroups=split`, which
// docs/sandbox.md §"Step boundary" documents as what lands a container
// under the invoking cgroup at all; docker has no equivalent flag) ahead of
// a `run` or `create` subcommand's own arguments, then exec the real
// binary. attemptExecer prepends this directory to PATH for every command a
// step runs, so an e2e script's own `podman run` or `docker create` is
// labelled the same way whether it runs through a check, a preparatory
// step, or a session.
//
// A compose tool (podman-compose, docker-compose) gets no wrapper of its
// own: podman-compose creates each service by invoking `podman` on PATH, so
// this shim already reaches it once it is ahead of the real one; the two
// docker-compose implementations talk to the engine API directly and never
// run the docker binary to create a container, so a wrapper here could not
// label it either way (docs/plans/2026-09-29-mill-96.md's Risks names the
// same gap for whatever creates a container without going through a
// shimmable CLI at all).
func writeContainerShims(binDir, name string) error {
	tools := []struct {
		bin   string
		extra string
	}{
		{"podman", " --cgroups=split"},
		{"docker", ""},
	}
	for _, tool := range tools {
		real, err := exec.LookPath(tool.bin)
		if err != nil {
			continue
		}
		if err := os.MkdirAll(binDir, 0o700); err != nil {
			return fmt.Errorf("workspace: %s: %w", binDir, err)
		}
		path := filepath.Join(binDir, tool.bin)
		if err := os.WriteFile(path, []byte(containerShim(real, name, tool.extra)), 0o700); err != nil { //nolint:gosec // an executable shim, meant to run
			return fmt.Errorf("workspace: %s: %w", path, err)
		}
	}
	return nil
}

// containerShim is a POSIX shell wrapper around real: a `run` or `create`
// subcommand gets the label (and extra) inserted right after it; anything
// else passes straight through.
func containerShim(real, name, extra string) string {
	return "#!/bin/sh\n" +
		"sub=\"$1\"\n" +
		"case \"$sub\" in\n" +
		"run|create)\n" +
		"\tshift\n" +
		"\texec " + shellQuote(real) + " \"$sub\" --label mill.attempt=" + shellQuote(name) + extra + " \"$@\"\n" +
		"\t;;\n" +
		"*)\n" +
		"\texec " + shellQuote(real) + " \"$@\"\n" +
		"\t;;\n" +
		"esac\n"
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// sweepContainers removes every container this step's shim labelled, along
// with its anonymous volumes: `rm -f -v`. podman does not copy a
// container's labels onto the volumes it creates, so a volume-side label
// filter can never match anything (the gap 491 orphaned volumes since
// 2026-09-20 came from) — removing the labelled containers with -v is the
// only path that reaches those volumes at all.
func (w *Workspace) sweepContainers(ctx context.Context, name string) error {
	var errs []error
	for _, tool := range []string{"podman", "docker"} {
		if _, err := exec.LookPath(tool); err != nil {
			continue
		}
		out, err := w.runTool(ctx, tool, "ps", "-a", "--filter", "label=mill.attempt="+name, "-q")
		if err != nil {
			errs = append(errs, err)
			continue
		}
		ids := strings.Fields(out)
		if len(ids) == 0 {
			continue
		}
		args := append([]string{"rm", "-f", "-v"}, ids...)
		if _, err := w.runTool(ctx, tool, args...); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
