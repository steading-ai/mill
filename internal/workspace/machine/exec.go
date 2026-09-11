package machine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/ivy/mill/internal/workspace"
	"github.com/ivy/mill/internal/workspace/machine/config"
	"github.com/ivy/mill/internal/workspace/machine/driver"
)

// SessionUser is who every process a step runs enters the machine as:
// `agent`, uid 1000, never root (docs/sandbox.md §"Session identity"). The
// helper's policy admits it.
const SessionUser = "agent"

// execRetries is how many times an exec whose transient unit fails to start
// — systemd-run non-zero before the service runs, not the command's own
// exit — is retried before the step fails with its stderr (docs/sandbox.md
// §"Failure modes"). One retry, then the error.
const execRetries = 1

// Exec runs one process inside the machine as `agent`, under the current
// step slice, through the root-owned helper (docs/sandbox.md §Exec). The
// process comes back with its pipes open: `--pipe` connects them to the
// service and streaming them is the caller's, exactly as the local backend
// hands back a process it spawned.
//
// The exec's environment is its own: the command's, with the workspace's
// egress decisions and the step token as the gateway's bearer set over it,
// because who a session reaches out through and which credential it presents
// are the workspace's to decide and not a caller's to shadow (docs/sandbox.md
// §Network, §Credentials). There is no machine-wide environment for a step;
// what is not here is not set.
//
// An exec with no step slice is refused before anything runs: nothing would
// reap what it spawned, so it is a bug (docs/sandbox.md §Exec). Cancellation
// is the helper's stop of the exec's own unit, not a kill of the local
// client — killing the client leaves the unit running inside the machine.
func (w *Workspace) Exec(ctx context.Context, command workspace.Command) (*workspace.Process, error) {
	w.mu.RLock()
	record, slice := w.record, w.slice
	w.mu.RUnlock()
	if record == nil {
		return nil, ErrNotCreated
	}
	if slice == "" {
		return nil, errors.New("machine: exec under no step slice; every process a step runs is reaped by its slice (docs/sandbox.md §Exec)")
	}
	if command.Path == "" {
		return nil, errors.New("machine: no command to run")
	}

	dir := command.Dir
	if dir == "" {
		dir = config.Work
	}
	request := driver.ExecRequest{
		Machine: record.Machine,
		Unit:    execUnit(),
		Slice:   slice,
		Dir:     dir,
		User:    SessionUser,
		Env:     w.execEnvironment(command),
		Argv:    append([]string{command.Path}, command.Args...),
	}

	process, err := w.execWithRetry(ctx, request)
	if err != nil {
		return nil, err
	}
	// Cancellation stops the unit by name through the helper: killing the
	// systemd-run client does not stop the transient service inside the
	// machine (docs/sandbox.md §Exec). The watch ends when ctx does, which
	// a step's context always is — the run loop cancels it when the step
	// returns — so a normal completion issues one stop of a unit that has
	// already collected itself, which the helper answers harmlessly.
	go w.stopOnCancel(ctx, record.Machine, request.Unit)
	return process, nil
}

// execEnvironment is the exec's whole environment: the command's own, with
// the workspace's egress and the step token set over it.
func (w *Workspace) execEnvironment(command workspace.Command) map[string]string {
	env := map[string]string{}
	for name, value := range command.Env {
		env[name] = value
	}
	for name, value := range w.Environment() {
		env[name] = value
	}
	return env
}

// execWithRetry runs the helper's exec, retrying once when the driver fails
// to spawn it at all — the transient unit never started — before returning
// the error. A helper refusal is the command's own concern and is not
// retried; it arrives as the process's exit status, not as a spawn error.
func (w *Workspace) execWithRetry(ctx context.Context, request driver.ExecRequest) (*workspace.Process, error) {
	var err error
	for attempt := 0; attempt <= execRetries; attempt++ {
		if attempt > 0 {
			// A fresh unit name: the previous one may have half-registered.
			request.Unit = execUnit()
			w.logf("exec in %s failed to start (%v); retrying", request.Machine, err)
		}
		var process *workspace.Process
		process, err = w.host.Exec(ctx, request)
		if err == nil {
			return process, nil
		}
		if ctx.Err() != nil {
			break
		}
	}
	return nil, fmt.Errorf("machine: exec in %s did not start after %d attempts: %w", request.Machine, execRetries+1, err)
}

// stopOnCancel stops the exec's unit when ctx ends: the helper's stop of
// `mill-exec-<uuid>.service`, run under a context ctx's end cannot cancel,
// because the point of it is to reach a machine whose exec the caller has
// abandoned. It targets the one unit by name — the `.service` the helper's
// stop pattern requires, which systemd-run gave the `--unit` — so a stop
// that lands after the unit has already gone touches nothing else.
func (w *Workspace) stopOnCancel(ctx context.Context, machine, unit string) {
	<-ctx.Done()
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	_ = w.host.StopUnit(stopCtx, machine, unit+".service")
}

// execUnit is a fresh exec unit name, `mill-exec-<hex>`: unique per exec so a
// cancellation stops exactly the one it names, and in the helper's
// `^mill-exec-[a-z0-9-]+$` alphabet. systemd-run appends `.service`; the
// helper's stop pattern wants that suffix and stopOnCancel adds it.
func execUnit() string {
	var raw [16]byte
	_, _ = rand.Read(raw[:])
	return "mill-exec-" + hex.EncodeToString(raw[:])
}

// BeginStep names the slice the coming step's execs run under,
// `mill-step-<ordinal>.slice`. The previous step's slice has already
// reported inactive, because [EndStep] waited for it before returning
// (docs/sandbox.md §"Step boundary").
func (w *Workspace) BeginStep(_ context.Context, ordinal int) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.record == nil {
		return ErrNotCreated
	}
	w.slice = StepSlice(ordinal)
	return nil
}

// EndStep ends the step: it takes the promotion candidate when the step
// closed on evidence — a read-only snapshot of the run's cache copies, taken
// before the slice stops so a later evidence-closed step's candidate
// replaces it — then stops the step's slice through the helper, which reaps
// everything the step spawned, containers included, and waits for the slice
// to report inactive (docs/sandbox.md §"Step boundary"). A step that was
// cancelled, timed out or failed passes closedOnEvidence false and leaves no
// candidate.
func (w *Workspace) EndStep(ctx context.Context, ordinal int, closedOnEvidence bool) error {
	w.mu.Lock()
	record := w.record
	slice := StepSlice(ordinal)
	w.slice = ""
	w.mu.Unlock()
	if record == nil {
		return ErrNotCreated
	}
	if closedOnEvidence {
		// Before the slice stops: the candidate is the cache state as of
		// the moment the step's work was done and verified.
		if err := w.caches.Candidate(ctx, record.ID, fmt.Sprintf("step-%d", ordinal)); err != nil {
			// A candidate that cannot be taken is a warning, never a
			// failed step: the run promotes from the last one it did take,
			// or nothing (docs/sandbox.md §Promote).
			w.logf("candidate at step %d not taken: %v", ordinal, err)
		}
	}
	// The helper's stop is synchronous — `systemctl -M stop` of the slice,
	// then `cgroup.kill` for orphans of already-ended units — so the slice
	// is inactive once it returns and the next BeginStep may proceed.
	if err := w.host.StopUnit(ctx, record.Machine, slice); err != nil {
		return fmt.Errorf("machine: end step %d: stop %s: %w", ordinal, slice, err)
	}
	return nil
}

// StepSlice is the slice one step runs under: `mill-step-<ordinal>.slice`,
// in the helper's `^mill-step-[a-z0-9-]+\.slice$` pattern.
func StepSlice(ordinal int) string { return fmt.Sprintf("mill-step-%d.slice", ordinal) }
