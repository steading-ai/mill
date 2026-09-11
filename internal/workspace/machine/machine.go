package machine

import (
	"context"

	"github.com/ivy/mill/internal/forge"
	"github.com/ivy/mill/internal/workspace"
	"github.com/ivy/mill/internal/workspace/redact"
)

// DefaultRoot is where the sandbox lives on the host: the btrfs volume
// holding run directories, cache lineages and squid's cache
// (docs/sandbox.md §Storage).
const DefaultRoot = "/var/lib/mill"

// DefaultMachines is where image subvolumes live: the `machines` subvolume
// of the sandbox volume, bind-mounted where systemd-nspawn looks
// (docs/sandbox.md §Storage, §Naming).
const DefaultMachines = "/var/lib/machines"

// Config is what the backend is given once, for every run on the worker. A
// run's own facts — its image, its caches, whether it is nested — arrive on
// each [workspace.Spec].
type Config struct {
	// Root is the sandbox root, [DefaultRoot] when empty. It must be
	// btrfs; `mill doctor` says whether it is.
	Root string
	// Machines is the image subvolume directory, [DefaultMachines] when
	// empty.
	Machines string
	// Origin is the repository to clone when a spec does not name one.
	Origin string
	// Forge is where the run delivers. Nil leaves the forge verbs answering
	// [workspace.ErrNoForge].
	Forge forge.Forge
	// Identity is the address the mill commits under.
	Identity string
	// Name is the name on the mill's commits.
	Name string
	// Redactor is scrubbed through before anything is logged. Nil means a
	// redactor of this workspace's own.
	Redactor *redact.Redactor
}

// Workspace is one run's machine workspace. Its verbs answer [ErrNotBuilt]
// until the nodes that build them land; the shape is here so the backend
// can be chosen by configuration now and honest about what it cannot do.
type Workspace struct {
	driver   Driver
	config   Config
	redactor *redact.Redactor
}

// New returns a workspace for one run, reaching the host through driver.
func New(driver Driver, config Config) *Workspace {
	if config.Root == "" {
		config.Root = DefaultRoot
	}
	if config.Machines == "" {
		config.Machines = DefaultMachines
	}
	w := &Workspace{driver: driver, config: config, redactor: config.Redactor}
	if w.redactor == nil {
		w.redactor = redact.New()
	}
	return w
}

// Check reports whether this worker could run the machine backend at all. It
// is asked before a node is claimed, the way the local backend's root is
// checked, so a worker configured for machines refuses at start rather than
// after a claim it cannot honour.
//
// Until the lifecycle node lands, what it reports is that the backend is
// not built.
func Check(_ Config) error {
	return notBuilt("the machine backend", LifecycleNode)
}

// Create is not built; the lifecycle node builds it.
func (w *Workspace) Create(context.Context, workspace.Spec) (*workspace.Handle, error) {
	return nil, notBuilt("create", LifecycleNode)
}

// Attach is not built; the lifecycle node builds it.
func (w *Workspace) Attach(context.Context, workspace.Spec) (*workspace.Handle, error) {
	return nil, notBuilt("attach", LifecycleNode)
}

// Destroy is not built; the lifecycle node builds it.
func (w *Workspace) Destroy(context.Context) error {
	return notBuilt("destroy", LifecycleNode)
}

// Rotate is not built; the lifecycle node builds it.
func (w *Workspace) Rotate(context.Context, workspace.Token) error {
	return notBuilt("rotate", LifecycleNode)
}

// Exec is not built; the exec node builds it on [Driver.Exec].
func (w *Workspace) Exec(context.Context, workspace.Command) (*workspace.Process, error) {
	return nil, notBuilt("exec", ExecNode)
}

// Read is not built; the lifecycle node builds it.
func (w *Workspace) Read(context.Context, string) ([]byte, error) {
	return nil, notBuilt("fs.read", LifecycleNode)
}

// Write is not built; the lifecycle node builds it.
func (w *Workspace) Write(context.Context, string, []byte) error {
	return notBuilt("fs.write", LifecycleNode)
}

// List is not built; the lifecycle node builds it.
func (w *Workspace) List(context.Context, string) ([]workspace.Entry, error) {
	return nil, notBuilt("fs.list", LifecycleNode)
}

// Git is the git verbs, none of them built.
func (w *Workspace) Git() workspace.Git { return gitVerbs{} }

// Forge is the forge verbs, none of them built.
func (w *Workspace) Forge() workspace.ForgeVerbs { return forgeVerbs{} }

// Redact registers values the workspace scrubs from every log line.
func (w *Workspace) Redact(values ...string) { w.redactor.Add(values...) }

// Handle is nil: nothing has been created.
func (w *Workspace) Handle() *workspace.Handle { return nil }

// GitRunner runs git in the checkout; not built.
func (w *Workspace) GitRunner() forge.Git { return gitRunner{} }

// Committer is the identity the workspace's commits will carry.
func (w *Workspace) Committer() workspace.Committer {
	return workspace.Committer{Name: w.config.Name, Email: w.config.Identity}
}

// Environment is what a process the workspace spawns runs under. Nothing is
// decided until a machine exists to decide it for.
func (w *Workspace) Environment() map[string]string { return map[string]string{} }

type gitVerbs struct{}

func (gitVerbs) Status(context.Context) (workspace.Status, error) {
	return workspace.Status{}, notBuilt("git.status", LifecycleNode)
}

func (gitVerbs) Diff(context.Context, workspace.DiffRequest) (string, error) {
	return "", notBuilt("git.diff", LifecycleNode)
}

func (gitVerbs) Log(context.Context, workspace.LogRequest) ([]workspace.CommitInfo, error) {
	return nil, notBuilt("git.log", LifecycleNode)
}

func (gitVerbs) Commit(context.Context, workspace.CommitRequest) (workspace.CommitInfo, error) {
	return workspace.CommitInfo{}, notBuilt("git.commit", LifecycleNode)
}

type forgeVerbs struct{}

func (forgeVerbs) Push(context.Context, workspace.PushRequest) (workspace.PushResult, error) {
	return workspace.PushResult{}, notBuilt("git.push", LifecycleNode)
}

func (forgeVerbs) PR(context.Context, workspace.PRRequest) (forge.PullRequest, error) {
	return forge.PullRequest{}, notBuilt("forge.pr", LifecycleNode)
}

func (forgeVerbs) Checks(context.Context, workspace.ChecksRequest) (workspace.ChecksResult, error) {
	return workspace.ChecksResult{}, notBuilt("forge.checks", LifecycleNode)
}

type gitRunner struct{}

func (gitRunner) Run(context.Context, map[string]string, ...string) (string, error) {
	return "", notBuilt("git", LifecycleNode)
}

// The workspace satisfies the interface the run loop wants of one; the
// assertion is here so the seam is checked at compile time rather than
// where it is wired.
var _ workspace.Workspace = (*Workspace)(nil)
