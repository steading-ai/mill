package local

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/steading-ai/mill/internal/workspace"
)

// DefaultGrace is how long a step's processes may take to stop cleanly once
// EndStep asks them to, before it escalates. It matches
// claude.DefaultGrace's value without importing it:
// internal/workspace/local sits below internal/harness/claude in the
// dependency graph, so the two are a sibling constant each, not one shared.
const DefaultGrace = 5 * time.Second

// killMode is how EndStep reaps what a step spawned.
type killMode int

const (
	// killModeGroup signals the process group [workspace.Spawner.Exec]
	// already puts every command in. It does not reach a grandchild that
	// detached into a session of its own (setsid, nohup … &).
	killModeGroup killMode = iota
	// killModeScope runs every command under a systemd --user scope of its
	// own, stopped by unit name. It is the only one of the two that reaches
	// a detached grandchild, because the scope is a cgroup and cgroup
	// membership does not depend on process ancestry.
	killModeScope
)

// stepState is what BeginStep opens and EndStep tears down: the scratch a
// step's commands run under, and everything the shared execer tracked
// against it so EndStep can reap it without the caller remembering
// anything.
type stepState struct {
	name string // "mill-<runID>-<ordinal>", unique across the host
	tmp  string // TMPDIR every command in the step gets
	bin  string // PATH-prepended directory holding the podman/docker shims

	mu        sync.Mutex
	processes []*workspace.Process // killModeGroup
	scopes    []string             // killModeScope, unit names
	counter   int                  // per-step scope unit-name suffix
}

// grace is the workspace's configured grace window, or [DefaultGrace].
func (w *Workspace) grace() time.Duration {
	if w.options.Grace > 0 {
		return w.options.Grace
	}
	return DefaultGrace
}

// BeginStep implements workspace.StepBoundary: it names the step, gives it
// a fresh scratch directory (removing any stale one of the same name — a
// worker that crashed mid-step and resumes starts its ordinal count over at
// 1, since local does not implement workspace.StepResumer, so BeginStep
// treats an already-used name as a crashed attempt's leftovers rather than
// refusing), and probes once per workspace whether this account can run a
// systemd --user scope.
func (w *Workspace) BeginStep(ctx context.Context, ordinal int) error {
	handle := w.snapshot()
	if handle == nil {
		return ErrNotCreated
	}
	w.killOnce.Do(func() { w.killMode = w.probeKillMode(ctx) })

	name := stepName(handle.RunID, ordinal)
	dir := filepath.Join(filepath.Dir(handle.Root), stepDirName, strconv.Itoa(ordinal))
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("workspace: begin step %d: %w", ordinal, err)
	}
	tmp := filepath.Join(dir, "tmp")
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return fmt.Errorf("workspace: begin step %d: %w", ordinal, err)
	}
	if err := writeContainerShims(bin, name); err != nil {
		return fmt.Errorf("workspace: begin step %d: %w", ordinal, err)
	}

	w.stepMu.Lock()
	w.step = &stepState{name: name, tmp: tmp, bin: bin}
	w.stepMu.Unlock()
	return nil
}

// EndStep implements workspace.StepBoundary: it reaps every process or
// scope the step's commands ran under, sweeps any container they labelled,
// and removes the step's scratch. Every one of those runs even when an
// earlier one fails, since a workspace that leaked half its cleanup because
// the other half errored is the exact failure mode this exists to close;
// the caller (internal/run/walk.go's underBoundary) only logs what EndStep
// returns; it does not fail the run on it.
func (w *Workspace) EndStep(ctx context.Context, _ int, _ bool) error {
	w.stepMu.Lock()
	step := w.step
	w.step = nil
	mode := w.killMode
	w.stepMu.Unlock()
	if step == nil {
		return nil
	}

	var errs []error
	if err := w.reap(ctx, step, mode); err != nil {
		errs = append(errs, err)
	}
	if err := w.sweepContainers(ctx, step.name); err != nil {
		errs = append(errs, err)
	}
	if err := os.RemoveAll(filepath.Dir(step.tmp)); err != nil {
		errs = append(errs, fmt.Errorf("workspace: end step: %w", err))
	}
	return errors.Join(errs...)
}

// reap stops everything the step's commands were tracked under. In scope
// mode, `systemctl --user stop` is synchronous and systemd's own
// TimeoutStopSec (set when the scope was created) escalates to SIGKILL on
// its own, so this only has to ask and wait. In group mode there is no
// synchronous "stop and confirm dead": the tracked *workspace.Process is
// the direct child of Exec, and its Wait is owned by whoever called
// Exec — check.Run, the harness adapter's flight — so reaping here must
// never call Wait itself, or a second call on the same *exec.Cmd panics.
// Signal's own "already gone" case is silent success (workspace.Process.Signal),
// which also rules out polling for death; the grace window is a plain sleep
// instead, and SIGKILL is sent unconditionally after it, harmless if the
// group is already gone.
func (w *Workspace) reap(ctx context.Context, step *stepState, mode killMode) error {
	step.mu.Lock()
	processes := append([]*workspace.Process(nil), step.processes...)
	scopes := append([]string(nil), step.scopes...)
	step.mu.Unlock()

	if mode == killModeScope {
		var errs []error
		for _, unit := range scopes {
			if _, err := w.runTool(ctx, "systemctl", "--user", "stop", unit); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}

	if len(processes) == 0 {
		return nil
	}
	for _, process := range processes {
		_ = process.Signal(syscall.SIGTERM)
	}
	select {
	case <-time.After(w.grace()):
	case <-ctx.Done():
	}
	for _, process := range processes {
		_ = process.Signal(syscall.SIGKILL)
	}
	return nil
}

// probeKillMode reports whether this account's session can run a systemd
// --user scope, by actually running one: `systemd-run --user --scope` needs
// a real user session (logind, an active D-Bus connection) that a bare
// account on a host with no login manager may not have, and the only
// reliable way to find out is to ask for the exact shape EndStep will later
// stop.
func (w *Workspace) probeKillMode(ctx context.Context) killMode {
	unit := "mill-probe-" + strconv.Itoa(os.Getpid())
	if _, err := w.runTool(ctx, "systemd-run",
		"--user", "--scope", "--collect", "--unit="+unit, "--", "true"); err != nil {
		return killModeGroup
	}
	return killModeScope
}

// attemptExecer is the one execer every process a step spawns runs
// through, whichever of Workspace.Exec (a check, a preparatory step) or
// Workspace.SessionExec (a skill step's session, and everything it spawns
// beneath it) reached it: it merges the step's TMPDIR and PATH-shim
// directory into whatever Command.Env the caller already built — added,
// never replacing, so it composes with the harness adapter's own
// ClearEnv-true environment the same way it composes with Exec's
// WORKSPACE_SOCKET/STEP_TOKEN — wraps the command in a systemd --user scope
// when the workspace's probe found one usable, and records what it started
// against the current step so EndStep can reap it. A command run with no
// step open — before the first BeginStep, or a verb's own git call, which
// runs directly through w.execer and never through here — passes straight
// through.
type attemptExecer struct {
	w *Workspace
}

func (a attemptExecer) Exec(ctx context.Context, command workspace.Command) (*workspace.Process, error) {
	a.w.stepMu.Lock()
	step := a.w.step
	mode := a.w.killMode
	a.w.stepMu.Unlock()
	if step == nil {
		return a.w.execer.Exec(ctx, command)
	}

	env := maps.Clone(command.Env)
	if env == nil {
		env = map[string]string{}
	}
	env["TMPDIR"] = step.tmp
	path, ok := env["PATH"]
	if !ok {
		// Command carries no PATH of its own: it will inherit the floor's,
		// which for this workspace's own execer is built from the mill's
		// environment (New's workspace.Floor(os.Environ())) — the same
		// source read here, so the shim directory lands ahead of the PATH
		// the child would otherwise get either way.
		path = os.Getenv("PATH")
	}
	env["PATH"] = step.bin + string(os.PathListSeparator) + path
	command.Env = env

	if mode != killModeScope {
		process, err := a.w.execer.Exec(ctx, command)
		if err != nil {
			return nil, err
		}
		step.mu.Lock()
		step.processes = append(step.processes, process)
		step.mu.Unlock()
		return process, nil
	}

	step.mu.Lock()
	step.counter++
	unit := step.name + "-" + strconv.Itoa(step.counter) + ".scope"
	step.mu.Unlock()
	process, err := a.w.execer.Exec(ctx, wrapScope(command, unit, a.w.grace()))
	if err != nil {
		return nil, err
	}
	step.mu.Lock()
	step.scopes = append(step.scopes, unit)
	step.mu.Unlock()
	return process, nil
}

// wrapScope rewrites command to run under a systemd --user scope of its
// own: `--collect` releases the unit as soon as it stops, so a step that
// runs many commands does not accumulate stopped units for
// `systemctl --user list-units` to keep showing, and TimeoutStopSec is set
// from grace so `systemctl --user stop` escalates to SIGKILL on the same
// schedule group mode uses, without EndStep managing the escalation itself.
func wrapScope(command workspace.Command, unit string, grace time.Duration) workspace.Command {
	args := make([]string, 0, len(command.Args)+6)
	args = append(args,
		"--user", "--scope", "--unit="+unit, "--collect",
		"--property=TimeoutStopSec="+grace.String(), "--", command.Path)
	args = append(args, command.Args...)
	command.Path = "systemd-run"
	command.Args = args
	return command
}

// runTool runs one of the workspace's own management commands — probing,
// stopping a scope, sweeping a container — through w.execer directly, never
// through attemptExecer: these are the workspace's own bookkeeping, not a
// step's process, and running them through the decorator would try to
// track a `systemctl stop` invocation against the very step it is ending.
func (w *Workspace) runTool(ctx context.Context, path string, args ...string) (string, error) {
	process, err := w.execer.Exec(ctx, workspace.Command{Path: path, Args: args})
	if err != nil {
		return "", fmt.Errorf("workspace: %s %s: %w", path, strings.Join(args, " "), err)
	}
	_ = process.Stdin.Close()
	stdoutCh := make(chan string, 1)
	go func() {
		out, _ := io.ReadAll(process.Stdout)
		stdoutCh <- string(out)
	}()
	errOut, _ := io.ReadAll(process.Stderr)
	stdout := <-stdoutCh
	result := process.Wait()
	if result.Status != 0 {
		return stdout, fmt.Errorf("workspace: %s %s exited %d: %s",
			path, strings.Join(args, " "), result.Status, strings.TrimSpace(string(errOut)))
	}
	return stdout, nil
}

// stepName is the identity a step's scope units and container labels carry:
// unique across the host because it combines the workspace's own run id
// with the step's ordinal, without needing the store's StepRun id —
// workspace.StepBoundary's contract only ever hands BeginStep/EndStep the
// ordinal.
func stepName(runID string, ordinal int) string {
	return "mill-" + runID + "-" + strconv.Itoa(ordinal)
}
