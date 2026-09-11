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
	// Deletion frees space asynchronously; sync follows it with `btrfs
	// filesystem sync` on the path's filesystem, the unprivileged commit
	// that makes statfs tell the truth (docs/spikes/nspawn-per-run-sandbox.md
	// E9). A read-only subvolume must be made writable first, which is
	// SetReadOnly's.
	DeleteSubvolume(ctx context.Context, path string, sync bool) error
	// SetReadOnly is `btrfs property set -ts <path> ro true|false`: a
	// generation is read-only from promotion until the moment it is
	// deleted, because its owner cannot delete it while it is (§Promote).
	SetReadOnly(ctx context.Context, path string, readOnly bool) error
	// DiskUsage is `du -s` of a tree in bytes: the size estimate a
	// generation's manifest records at promotion, and the number a global
	// lineage's cap is compared against (§Promote). Never qgroups.
	DiskUsage(ctx context.Context, path string) (uint64, error)
	// Statfs is the filesystem under a path: what it is and what is free,
	// which is what eviction decides on and what doctor asks of the
	// sandbox root (§Eviction, §Storage).
	Statfs(path string) (Filesystem, error)

	// The verbs below are the image pipeline's (§Images): the one place the
	// worker runs a container outside a machine, and the export that turns
	// its filesystem into a subvolume a machine boots. The image directory
	// is root's on the host (homestead's mill_storage), so the subvolume
	// verbs that write it — CreateSubvolume, Rename, Setcap, the extraction
	// in ExportImage, and DeleteSubvolume — run through sudo when
	// [Commands.Sudo] names it. Snapshot never does: a cache the operator
	// cannot delete is a cache the operator cannot promote.

	// Exists reports whether a path is on the host, asked as whoever may
	// see the image directory.
	Exists(ctx context.Context, path string) (bool, error)
	// CreateSubvolume is `btrfs subvolume create <path>`: the `.building`
	// subvolume an export lands in.
	CreateSubvolume(ctx context.Context, path string) error
	// Rename moves a path, atomically when both are on one filesystem:
	// `.building` into the name a machine boots from.
	Rename(ctx context.Context, oldPath, newPath string) error
	// Setcap is `setcap <capability> <path>`: what podman export drops from
	// newuidmap and newgidmap, restored on the exported tree
	// (docs/spikes/nspawn-per-run-sandbox.md E2).
	Setcap(ctx context.Context, capability, path string) error
	// PullImage is `podman pull <reference>`: the base from Harbor, at each
	// provision.
	PullImage(ctx context.Context, reference string) error
	// ImageDigest is the registry digest of an image podman holds —
	// `sha256:<hex>` — which is what the derived digest is computed from
	// and what the generated Containerfile pins.
	ImageDigest(ctx context.Context, reference string) (string, error)
	// BuildImageOnHost is `podman build` on the host, outside any machine:
	// the only container the worker runs there, which docs/sandbox.md
	// §"Derived images" asks the code to make visible. The whole of the
	// build's output comes back with the error, because its last lines are
	// the finding a failed derived build attaches to the run.
	BuildImageOnHost(ctx context.Context, build ImageBuild) (output string, err error)
	// ExportImage is `podman create`, `podman export | tar -x` into dir, and
	// `podman rm`: the image's filesystem as a flat tree, never layers.
	ExportImage(ctx context.Context, image, dir string) error
	// RemoveImage is `podman rmi <image>`: the built image once its tree is
	// exported, so the host's container storage does not carry a copy of
	// every subvolume.
	RemoveImage(ctx context.Context, image string) error
	// Login is `podman login <registry>` with the secret on stdin, never
	// on the command line.
	Login(ctx context.Context, registry, user, secret string) error
	// Logout is `podman logout <registry>`.
	Logout(ctx context.Context, registry string) error
	// PushImage is `podman push <reference>`.
	PushImage(ctx context.Context, reference string) error
}

// ImageBuild is one `podman build` on the host.
type ImageBuild struct {
	// Context is the build context directory.
	Context string
	// Containerfile is the file to build from; empty means the context's
	// own `Containerfile`.
	Containerfile string
	// Tags are the names the result is tagged with, at least one.
	Tags []string
	// Args are the build arguments, `--build-arg NAME=VALUE`.
	Args map[string]string
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
