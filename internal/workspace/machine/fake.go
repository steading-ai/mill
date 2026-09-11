package machine

import (
	"context"
	"fmt"
	"strconv"
	"sync"

	"github.com/ivy/mill/internal/workspace"
)

// The methods of [Driver], as [FakeDriver] records and scripts them.
const (
	StartUnit       = "StartUnit"
	StopUnit        = "StopUnit"
	ShowMachine     = "ShowMachine"
	Exec            = "Exec"
	ListMachines    = "ListMachines"
	Snapshot        = "Snapshot"
	DeleteSubvolume = "DeleteSubvolume"
	Statfs          = "Statfs"
)

// Call is one call a [FakeDriver] received: the method and its arguments in
// order, as the caller passed them.
type Call struct {
	// Method is the [Driver] method's name, one of the constants above.
	Method string
	// Args are the arguments after the context, in declaration order.
	Args []any
}

// Output is what a scripted [Exec] produces: the fake spawns a real process
// that writes and exits as told, so the caller streams stdio and reads an
// exit status exactly as it would from the host.
type Output struct {
	// Stdout is written to the process's standard output.
	Stdout string
	// Stderr is written to its standard error.
	Stderr string
	// Status is what it exits with.
	Status int
}

// FakeDriver is a host that records every call and answers as scripted. A
// spec of the machine backend is written against it, and the live spec that
// proves the same scenario runs against [NewSystemd].
//
// Results are scripted per method, in order: the first call of a method
// consumes the first result queued for it. A method with nothing queued
// succeeds and returns its zero value, so a spec scripts only what it is
// about. A scripted error is returned as the call's error; a scripted value
// is returned as its result.
type FakeDriver struct {
	mu      sync.Mutex
	calls   []Call
	scripts map[string][]any
}

// NewFake is a driver with nothing scripted.
func NewFake() *FakeDriver {
	return &FakeDriver{scripts: map[string][]any{}}
}

// Script queues what the next calls of method return, one result per call.
// A result is an error, or the method's value: [Machine] for ShowMachine,
// []Machine for ListMachines, [Filesystem] for Statfs, [Output] for Exec.
func (f *FakeDriver) Script(method string, results ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripts[method] = append(f.scripts[method], results...)
}

// Calls is every call received so far, in order.
func (f *FakeDriver) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Call, len(f.calls))
	copy(out, f.calls)
	return out
}

// CallsTo is the calls received for one method, in order.
func (f *FakeDriver) CallsTo(method string) []Call {
	var out []Call
	for _, call := range f.Calls() {
		if call.Method == method {
			out = append(out, call)
		}
	}
	return out
}

// record notes a call and takes the next scripted result for it, if any.
func (f *FakeDriver) record(method string, args ...any) any {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, Call{Method: method, Args: args})
	queued := f.scripts[method]
	if len(queued) == 0 {
		return nil
	}
	f.scripts[method] = queued[1:]
	return queued[0]
}

// StartUnit records the call and returns the scripted error, if any.
func (f *FakeDriver) StartUnit(_ context.Context, unit string) error {
	return asError(f.record(StartUnit, unit))
}

// StopUnit records the call and returns the scripted error, if any.
func (f *FakeDriver) StopUnit(_ context.Context, machine, unit string) error {
	return asError(f.record(StopUnit, machine, unit))
}

// ShowMachine records the call and returns the scripted machine or error.
func (f *FakeDriver) ShowMachine(_ context.Context, name string) (Machine, error) {
	switch result := f.record(ShowMachine, name).(type) {
	case nil:
		return Machine{}, nil
	case error:
		return Machine{}, result
	case Machine:
		return result, nil
	default:
		return Machine{}, scriptMismatch(ShowMachine, result)
	}
}

// Exec records the request and runs a process that behaves as scripted: an
// [Output] becomes a real process writing that output and exiting with
// that status, so stdio streams and Wait reports as the host's would.
func (f *FakeDriver) Exec(ctx context.Context, request ExecRequest) (*workspace.Process, error) {
	switch result := f.record(Exec, request).(type) {
	case nil:
		return spawn(ctx, Output{})
	case error:
		return nil, result
	case Output:
		return spawn(ctx, result)
	default:
		return nil, scriptMismatch(Exec, result)
	}
}

// spawn is the process behind a scripted Exec. The output goes in through
// the environment rather than the script, so nothing in it is parsed by the
// shell.
func spawn(ctx context.Context, output Output) (*workspace.Process, error) {
	return workspace.Spawner{Floor: []string{}}.Exec(ctx, workspace.Command{
		Path: "/bin/sh",
		Args: []string{"-c", `printf '%s' "$FAKE_STDOUT"; printf '%s' "$FAKE_STDERR" >&2; exit "$FAKE_STATUS"`},
		Env: map[string]string{
			"FAKE_STDOUT": output.Stdout,
			"FAKE_STDERR": output.Stderr,
			"FAKE_STATUS": strconv.Itoa(output.Status),
		},
	})
}

// ListMachines records the call and returns the scripted list or error.
func (f *FakeDriver) ListMachines(_ context.Context) ([]Machine, error) {
	switch result := f.record(ListMachines).(type) {
	case nil:
		return nil, nil
	case error:
		return nil, result
	case []Machine:
		return result, nil
	default:
		return nil, scriptMismatch(ListMachines, result)
	}
}

// Snapshot records the call and returns the scripted error, if any.
func (f *FakeDriver) Snapshot(_ context.Context, source, destination string, readOnly bool) error {
	return asError(f.record(Snapshot, source, destination, readOnly))
}

// DeleteSubvolume records the call and returns the scripted error, if any.
func (f *FakeDriver) DeleteSubvolume(_ context.Context, path string) error {
	return asError(f.record(DeleteSubvolume, path))
}

// Statfs records the call and returns the scripted filesystem or error.
func (f *FakeDriver) Statfs(path string) (Filesystem, error) {
	switch result := f.record(Statfs, path).(type) {
	case nil:
		return Filesystem{}, nil
	case error:
		return Filesystem{}, result
	case Filesystem:
		return result, nil
	default:
		return Filesystem{}, scriptMismatch(Statfs, result)
	}
}

// asError is a scripted result for a method that returns only an error.
func asError(result any) error {
	switch result := result.(type) {
	case nil:
		return nil
	case error:
		return result
	default:
		return scriptMismatch("", result)
	}
}

// scriptMismatch is a spec's mistake surfaced as the call's error rather
// than swallowed: a result of a type the method cannot return.
func scriptMismatch(method string, result any) error {
	return fmt.Errorf("machine: fake driver: %s cannot return a scripted %T", method, result)
}
