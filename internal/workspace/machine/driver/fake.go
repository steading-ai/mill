package driver

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/ivy/mill/internal/workspace"
)

// The methods of [Driver], as [FakeDriver] records and scripts them.
const (
	StartUnit         = "StartUnit"
	StopUnit          = "StopUnit"
	Terminate         = "Terminate"
	ShowMachine       = "ShowMachine"
	MachineInterfaces = "MachineInterfaces"
	Exec              = "Exec"
	ListMachines      = "ListMachines"
	Snapshot          = "Snapshot"
	DeleteSubvolume   = "DeleteSubvolume"
	SetReadOnly       = "SetReadOnly"
	DiskUsage         = "DiskUsage"
	Statfs            = "Statfs"
	Container         = "Container"
	SandboxReady      = "SandboxReady"

	Exists           = "Exists"
	CreateSubvolume  = "CreateSubvolume"
	Rename           = "Rename"
	Setcap           = "Setcap"
	PullImage        = "PullImage"
	ImageDigest      = "ImageDigest"
	BuildImageOnHost = "BuildImageOnHost"
	ExportImage      = "ExportImage"
	RemoveImage      = "RemoveImage"
	Login            = "Login"
	Logout           = "Logout"
	PushImage        = "PushImage"
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
//
// The subvolume verbs — Exists, CreateSubvolume, Rename, DeleteSubvolume,
// Snapshot — act on plain directories when they succeed, so a spec's
// temporary directory stands in for the image directory or a cache
// lineage and a second caller finds what the first built: a snapshot is a
// copy of the tree, which is what a snapshot is to everyone but the disk.
// What a plain directory cannot stand in for is not modelled: SetReadOnly
// records the call and changes nothing, and DiskUsage answers what is
// scripted or the tree's apparent size. The podman verbs touch nothing.
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
// []Machine for ListMachines, []string for MachineInterfaces, [Filesystem]
// for Statfs, [Output] for Exec.
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

// Terminate records the call and returns the scripted error, if any.
func (f *FakeDriver) Terminate(_ context.Context, machine string) error {
	return asError(f.record(Terminate, machine))
}

// MachineInterfaces records the call and returns the scripted names or
// error.
func (f *FakeDriver) MachineInterfaces(_ context.Context, name string) ([]string, error) {
	switch result := f.record(MachineInterfaces, name).(type) {
	case nil:
		return nil, nil
	case error:
		return nil, result
	case []string:
		return result, nil
	default:
		return nil, scriptMismatch(MachineInterfaces, result)
	}
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

// Snapshot records the call, returns the scripted error if any, and
// otherwise copies the directory standing in for the source subvolume to
// the destination, which must not exist yet, as btrfs requires.
func (f *FakeDriver) Snapshot(_ context.Context, source, destination string, readOnly bool) error {
	if err := asError(f.record(Snapshot, source, destination, readOnly)); err != nil {
		return err
	}
	if _, err := os.Stat(destination); err == nil {
		return fmt.Errorf("machine: fake driver: snapshot destination %s exists", destination)
	}
	return os.CopyFS(destination, os.DirFS(source))
}

// DeleteSubvolume records the call, returns the scripted error if any, and
// otherwise removes the directory standing in for the subvolume.
func (f *FakeDriver) DeleteSubvolume(_ context.Context, path string, sync bool) error {
	if err := asError(f.record(DeleteSubvolume, path, sync)); err != nil {
		return err
	}
	return os.RemoveAll(path)
}

// SetReadOnly records the call and returns the scripted error, if any. A
// plain directory has no such property to set.
func (f *FakeDriver) SetReadOnly(_ context.Context, path string, readOnly bool) error {
	return asError(f.record(SetReadOnly, path, readOnly))
}

// DiskUsage records the call and answers the scripted size, or the sum of
// the regular files under the path.
func (f *FakeDriver) DiskUsage(_ context.Context, path string) (uint64, error) {
	switch result := f.record(DiskUsage, path).(type) {
	case nil:
		return apparentSize(path)
	case error:
		return 0, result
	case uint64:
		return result, nil
	case int:
		return uint64(result), nil //nolint:gosec // a scripted size is never negative
	default:
		return 0, scriptMismatch(DiskUsage, result)
	}
}

// apparentSize is the bytes of every regular file under a tree.
func apparentSize(root string) (uint64, error) {
	var total uint64
	err := filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			total += uint64(info.Size()) //nolint:gosec // a file size is never negative
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("machine: fake driver: du %s: %w", root, err)
	}
	return total, nil
}

// Exists records the call and answers the scripted value, or whether the
// path is there.
func (f *FakeDriver) Exists(_ context.Context, path string) (bool, error) {
	switch result := f.record(Exists, path).(type) {
	case nil:
		_, err := os.Stat(path)
		return err == nil, nil
	case error:
		return false, result
	case bool:
		return result, nil
	default:
		return false, scriptMismatch(Exists, result)
	}
}

// CreateSubvolume records the call, returns the scripted error if any, and
// otherwise makes a directory standing in for the subvolume.
func (f *FakeDriver) CreateSubvolume(_ context.Context, path string) error {
	if err := asError(f.record(CreateSubvolume, path)); err != nil {
		return err
	}
	return os.Mkdir(path, 0o755)
}

// Rename records the call, returns the scripted error if any, and otherwise
// renames the directory.
func (f *FakeDriver) Rename(_ context.Context, oldPath, newPath string) error {
	if err := asError(f.record(Rename, oldPath, newPath)); err != nil {
		return err
	}
	return os.Rename(oldPath, newPath)
}

// Setcap records the call and returns the scripted error, if any.
func (f *FakeDriver) Setcap(_ context.Context, capability, path string) error {
	return asError(f.record(Setcap, capability, path))
}

// PullImage records the call and returns the scripted error, if any.
func (f *FakeDriver) PullImage(_ context.Context, reference string) error {
	return asError(f.record(PullImage, reference))
}

// ImageDigest records the call and returns the scripted digest or error.
func (f *FakeDriver) ImageDigest(_ context.Context, reference string) (string, error) {
	return asString(ImageDigest, f.record(ImageDigest, reference))
}

// BuildImageOnHost records the build and returns the scripted output or
// error. A scripted [Output] is a build that failed: its Stdout is the
// build's transcript and the error carries its status.
func (f *FakeDriver) BuildImageOnHost(_ context.Context, build ImageBuild) (string, error) {
	switch result := f.record(BuildImageOnHost, build).(type) {
	case nil:
		return "", nil
	case error:
		return "", result
	case string:
		return result, nil
	case Output:
		return result.Stdout + result.Stderr, fmt.Errorf("machine: podman build exited %d", result.Status)
	default:
		return "", scriptMismatch(BuildImageOnHost, result)
	}
}

// ExportImage records the call and returns the scripted error, if any.
func (f *FakeDriver) ExportImage(_ context.Context, image, dir string) error {
	return asError(f.record(ExportImage, image, dir))
}

// RemoveImage records the call and returns the scripted error, if any.
func (f *FakeDriver) RemoveImage(_ context.Context, image string) error {
	return asError(f.record(RemoveImage, image))
}

// Login records the registry and user — never the secret — and returns the
// scripted error, if any.
func (f *FakeDriver) Login(_ context.Context, registry, user, _ string) error {
	return asError(f.record(Login, registry, user))
}

// Logout records the call and returns the scripted error, if any.
func (f *FakeDriver) Logout(_ context.Context, registry string) error {
	return asError(f.record(Logout, registry))
}

// PushImage records the call and returns the scripted error, if any.
func (f *FakeDriver) PushImage(_ context.Context, reference string) error {
	return asError(f.record(PushImage, reference))
}

// asString is a scripted result for a method that returns a string.
func asString(method string, result any) (string, error) {
	switch result := result.(type) {
	case nil:
		return "", nil
	case error:
		return "", result
	case string:
		return result, nil
	default:
		return "", scriptMismatch(method, result)
	}
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

// Container records the call and returns the scripted container manager,
// or none: the fake is a host unless told otherwise.
func (f *FakeDriver) Container(_ context.Context) (string, error) {
	return asString(Container, f.record(Container))
}

// SandboxReady defaults to ready: the fake is a sandbox-capable host unless
// told otherwise.
func (f *FakeDriver) SandboxReady(_ context.Context, root, machines string) (string, error) {
	return asString(SandboxReady, f.record(SandboxReady, root, machines))
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
