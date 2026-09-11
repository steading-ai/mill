// Package machine is the workspace of docs/architecture.md's `machine` row:
// one systemd-nspawn machine per run, booted from an ephemeral snapshot of
// an image subvolume, with the run's checkout bind-mounted into it. The
// machine itself — its image, binds, limits, network and credentials — is
// specified in docs/sandbox.md, which this package implements and defers to.
//
// The host is reached through one seam, [Driver]: a method per host command
// the specification names, so a machine's lifecycle can be specified against
// [FakeDriver] and proven against the real host under Label("live"). The
// real driver shells out through [workspace.Spawner] and nothing else; the
// forbidigo rule in .golangci.yml is what makes that checkable.
//
// The backend is being built. [New] returns a workspace whose verbs answer
// [ErrNotBuilt], each naming the node that builds it, so a worker configured
// for machines fails before it claims anything rather than after.
package machine

import (
	"context"
	"errors"
	"fmt"

	"github.com/ivy/mill/internal/workspace"
)

// The nodes that build what this package does not yet do. An error names
// one so that whoever meets it knows where the work is.
const (
	// LifecycleNode builds create, attach, destroy and rotate: a run's
	// machine through mill-machine@ (docs/sandbox.md §Lifecycle).
	LifecycleNode = "machine-lifecycle (mill#61)"
	// ExecNode builds exec under a step slice and the step boundary that
	// reaps everything (docs/sandbox.md §Exec, §"Step boundary").
	ExecNode = "machine-exec (mill#62)"
)

// ErrNotBuilt is a verb the machine backend does not do yet. Every error a
// verb returns wraps it and names the node that builds the verb; see
// [NotBuiltError].
var ErrNotBuilt = errors.New("machine: not built")

// NotBuiltError is [ErrNotBuilt] with the verb and the node attached.
type NotBuiltError struct {
	// Verb is what was asked for, such as `create`.
	Verb string
	// Node is the node that builds it: [LifecycleNode] or [ExecNode].
	Node string
}

func (e *NotBuiltError) Error() string {
	return fmt.Sprintf("machine: %s is not built yet; the %s node builds it", e.Verb, e.Node)
}

// Unwrap makes errors.Is(err, ErrNotBuilt) true.
func (e *NotBuiltError) Unwrap() error { return ErrNotBuilt }

// notBuilt is a verb's answer until its node lands.
func notBuilt(verb, node string) error {
	return &NotBuiltError{Verb: verb, Node: node}
}

// Driver is the host as the backend reaches it. Every method is one host
// command docs/sandbox.md names, in the shape it names it, and nothing the
// backend does to the host goes around it. Keeping it to that is what lets a
// spec script the host and a live spec prove the script.
//
// Which uid may run which command — the polkit rule on the template unit,
// and whether exec needs a helper — is the host's configuration
// (docs/sandbox.md §Privilege) and the driver's concern, never a caller's.
type Driver interface {
	// StartUnit is `systemctl start <unit>` on the host's manager: the
	// machine's own unit, an instance of mill-machine@ (§Provision).
	StartUnit(ctx context.Context, unit string) error
	// StopUnit is `systemctl stop <unit>`: on the host when machine is
	// empty, which destroys a machine (§Destroy); inside the machine
	// otherwise, which is how a step slice or an exec is stopped (§"Step
	// boundary", §Exec).
	StopUnit(ctx context.Context, machine, unit string) error
	// ShowMachine is `machinectl show <name>`: the machine as machined
	// knows it, which is how readiness is polled (§Provision). A name
	// machined does not know is [ErrNoMachine].
	ShowMachine(ctx context.Context, name string) (Machine, error)
	// Exec is `systemd-run --machine=<name> --pipe --wait` under a step
	// slice (§Exec). The process's stdin, stdout and stderr are the
	// caller's to stream, and its exit status is the command's own.
	Exec(ctx context.Context, request ExecRequest) (*workspace.Process, error)
	// ListMachines is `machinectl list`: every machine registered on the
	// host, which is the truth reconciliation reads (§"External state").
	ListMachines(ctx context.Context) ([]Machine, error)
	// Snapshot is `btrfs subvolume snapshot <source> <destination>`,
	// read-only when asked: how a cache is restored and how a promotion
	// candidate is taken (§Caching).
	Snapshot(ctx context.Context, source, destination string, readOnly bool) error
	// DeleteSubvolume is `btrfs subvolume delete <path>`: a run's cache
	// copy at destroy, a generation at eviction (§Destroy, §Eviction).
	DeleteSubvolume(ctx context.Context, path string) error
	// Statfs is the filesystem under a path: what it is and what is free,
	// which is what eviction decides on and what doctor asks of the
	// sandbox root (§Eviction, §Storage).
	Statfs(path string) (Filesystem, error)
}

// ErrNoMachine is a machine name machined does not know: never started, or
// already gone.
var ErrNoMachine = errors.New("machine: no such machine")

// Machine is one machine as machined reports it.
type Machine struct {
	// Name is the machine's name, `mill-<run-id>` for one of mill's
	// (docs/sandbox.md §Naming).
	Name string
	// State is machined's word for where it is: `opening`, `running`,
	// `degraded`, `closing`.
	State string
	// Class is `container` for an nspawn machine, `vm` for a vmspawn one.
	Class string
	// Service is what registered it, such as `systemd-nspawn`.
	Service string
}

// StateRunning is the state a machine reaches when it has booted, which is
// what readiness waits for before asking systemd inside.
const StateRunning = "running"

// ExecRequest is one process inside a machine: the unit it runs as, the
// slice it runs under, and the command itself (docs/sandbox.md §Exec).
type ExecRequest struct {
	// Machine is the machine to run in.
	Machine string
	// Unit is the transient service's name, `mill-exec-<uuid>`. Empty
	// lets systemd name it.
	Unit string
	// Slice is the step slice the unit runs under, `mill-step-<n>.slice`.
	// Required: an exec with no step slice is a bug, because nothing would
	// reap what it spawns.
	Slice string
	// Dir is the working directory inside the machine, `/work` for the
	// checkout.
	Dir string
	// User is who the process runs as inside the machine; `agent`.
	User string
	// Env is the process's whole environment. There is no machine-wide
	// environment for steps; what is not here is not set.
	Env map[string]string
	// Argv is the command and its arguments.
	Argv []string
}

// Filesystem is what statfs says of the filesystem under a path.
type Filesystem struct {
	// Type is the filesystem's name as the kernel's magic number spells
	// it: `btrfs`, `xfs`, `ext4`, `tmpfs`. An unknown magic is its hex.
	Type string
	// Free is the bytes available to an unprivileged writer.
	Free uint64
	// Total is the filesystem's size in bytes.
	Total uint64
}

// TypeBtrfs is the filesystem the sandbox root must be on: ephemeral roots,
// cache lineages and eviction are all subvolume operations
// (docs/sandbox.md §Storage).
const TypeBtrfs = "btrfs"

// Btrfs reports whether the filesystem is the one the sandbox needs.
func (f Filesystem) Btrfs() bool { return f.Type == TypeBtrfs }
