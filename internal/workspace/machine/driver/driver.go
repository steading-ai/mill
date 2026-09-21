// Package driver is the host as the machine workspace reaches it: one seam,
// [Driver], with a method per host command docs/sandbox.md names, so a
// machine's lifecycle can be specified against [FakeDriver] and proven
// against the real host under Label("live"). The real driver, [NewSystemd],
// shells out through [workspace.Spawner] and nothing else; the forbidigo
// rule in .golangci.yml is what makes that checkable.
//
// Which uid may run which command is the host's configuration
// (docs/sandbox.md §Privilege) and this package's concern, never a
// caller's: the machine unit is started and stopped with systemctl under
// polkit, and everything that enters a running machine — exec, a stop
// inside it, terminate — goes through the root-owned helper
// /usr/libexec/mill-machine under sudo (docs/spikes/nspawn-per-run-sandbox.md
// E0: systemd-run -M wants CAP_SYS_ADMIN whatever polkit says).
package driver

import (
	"context"
	"errors"

	"github.com/ivy/mill/internal/workspace"
)

// DefaultRoot is where the sandbox lives on the host: the btrfs volume
// holding run directories, cache lineages and squid's cache
// (docs/sandbox.md §Storage).
const DefaultRoot = "/var/lib/mill"

// DefaultMachines is where image subvolumes live: the `machines` subvolume
// of the sandbox volume, bind-mounted where systemd-nspawn looks
// (docs/sandbox.md §Storage, §Naming).
const DefaultMachines = "/var/lib/machines"

// DefaultRunState is where a run's socket directory lives on the host,
// `/run/mill/<id>`, bound into the machine at `/run/mill`
// (docs/sandbox.md §Filesystem). The helper's policy admits binds under it.
const DefaultRunState = "/run/mill"

// Driver is the host as the backend reaches it. Every method is one host
// command docs/sandbox.md names, in the shape it names it, and nothing the
// backend does to the host goes around it. Keeping it to that is what lets a
// spec script the host and a live spec prove the script.
type Driver interface {
	// StartUnit is `systemctl start <unit>` on the host's manager: the
	// machine's own unit, an instance of mill-machine@, admitted to the
	// worker's uid by polkit (§Provision, §Privilege). The unit is
	// Type=notify, so the call returns once the machine's systemd is up.
	StartUnit(ctx context.Context, unit string) error
	// StopUnit stops a unit. With machine empty it is `systemctl stop
	// <unit>` on the host, which destroys a machine (§Destroy). With a
	// machine named it is the helper's `stop <machine> <unit>`, which is
	// how a step slice or an exec inside the machine is stopped (§"Step
	// boundary", §Exec): `systemctl -M stop`, then the slice's cgroup.kill.
	StopUnit(ctx context.Context, machine, unit string) error
	// Terminate is the helper's `terminate <machine>`: machinectl
	// terminate, then nspawn's second SIGTERM after a second, then SIGKILL.
	// The escape when a stop has not removed the machine (§Destroy).
	Terminate(ctx context.Context, machine string) error
	// ShowMachine is `machinectl show <name>`: the machine as machined
	// knows it, which is how readiness is polled (§Provision). A name
	// machined does not know is [ErrNoMachine].
	ShowMachine(ctx context.Context, name string) (Machine, error)
	// MachineInterfaces is the host-side network interfaces of a machine
	// as `machinectl status` lists them: the veth the ledger records
	// (§Network). A name machined does not know is [ErrNoMachine].
	MachineInterfaces(ctx context.Context, name string) ([]string, error)
	// Exec is the helper's `exec <machine>`: `systemd-run --machine
	// --pipe --wait --quiet --collect -p Delegate=yes -p KillMode=mixed`
	// under a step slice (§Exec). The process's stdin, stdout and stderr
	// are the caller's to stream, and its exit status is the command's own.
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
	// Container is `systemd-detect-virt --container`: the container
	// manager this worker runs under, [ContainerNspawn] inside a machine,
	// empty on a host. A worker inside a machine is a nested one and roots
	// its sandbox under its own ephemeral root (§Self-hosting).
	Container(ctx context.Context) (string, error)
	// SandboxReady is what a run needs of this host before sudo -n is even
	// asked: systemd-nspawn and machinectl on PATH, the mill-machine@
	// template unit installed, the helper file in place, and root on
	// btrfs with a machines subvolume (§Self-hosting, §Storage), checked
	// in that order. reason names the first of these missing; empty means
	// ready. root and machines are the caller's facts, not a default read
	// internally, so a live spec and mill doctor's workspace probe ask the
	// identical question of the identical host.
	SandboxReady(ctx context.Context, root, machines string) (reason string, err error)

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
	// ReadOnlyPaths are the paths the unit and everything it spawns see
	// read-only, in a mount namespace of their own: `/work` for a step
	// whose token lacks `write` (docs/sandbox.md §Exec). The helper admits
	// exactly `/work`.
	ReadOnlyPaths []string
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
