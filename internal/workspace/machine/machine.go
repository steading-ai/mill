// Package machine is the workspace of docs/architecture.md's `machine` row:
// one systemd-nspawn machine per run, booted from an ephemeral snapshot of
// an image subvolume, with the run's checkout bind-mounted into it. The
// machine itself — its image, binds, limits, network and credentials — is
// specified in docs/sandbox.md, which this package implements and defers to.
//
// The host is reached through one seam, [driver.Driver], so a machine's
// lifecycle is specified against [driver.FakeDriver] and proven against the
// real host under Label("live"). The images a machine boots are
// [image.Store]'s, the caches restored into it [cache.Store]'s, and what the
// repository declares about both [config.Load]'s; this package is the
// sequence that puts them together (docs/sandbox.md §"Sequence of a run").
//
// One value serves one run. Create makes the run directory and boots the
// machine, Attach reopens one a previous worker on this host left behind,
// Destroy stops the machine and removes everything but promoted cache
// generations, and the socket lives for as long as the value does.
package machine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ivy/mill/internal/forge"
	"github.com/ivy/mill/internal/workspace"
	"github.com/ivy/mill/internal/workspace/machine/cache"
	"github.com/ivy/mill/internal/workspace/machine/driver"
	"github.com/ivy/mill/internal/workspace/machine/image"
	"github.com/ivy/mill/internal/workspace/redact"
	"github.com/ivy/mill/internal/workspace/socket"
)

// The host's layout, as the driver names it (docs/sandbox.md §Storage).
const (
	DefaultRoot     = driver.DefaultRoot
	DefaultMachines = driver.DefaultMachines
	DefaultRunState = driver.DefaultRunState
	// DefaultDaggerCheckout is where the host keeps the dagger checkout a
	// nested machine's test fixture reads migrations from, bound read-only
	// at [DaggerCheckoutTarget] (docs/sandbox.md §Testing, §Self-hosting).
	// The helper's policy names it among the nested read-only sources.
	DefaultDaggerCheckout = DefaultRoot + "/dagger"
	// DaggerCheckoutTarget is where a nested machine sees it: the same
	// path, so `MILL_DAGGER_REPO` reads alike inside and out.
	DaggerCheckoutTarget = DefaultRoot + "/dagger"
	// NestedRoot is the sandbox root of a worker inside a machine
	// (docs/sandbox.md §Self-hosting).
	NestedRoot = driver.NestedRoot
)

// The network the host's mill_egress role fixes for every machine
// (docs/sandbox.md §Network, §Credentials): squid and the credential
// gateway on the `mill-host` dummy link, and the loopback names
// testcontainers dials that must not be sent to squid.
const (
	DefaultProxy   = "http://10.99.255.1:3128"
	DefaultNoProxy = "localhost,127.0.0.1"
	DefaultGateway = "http://10.99.255.1:8080"
)

// The bounds on a machine's lifecycle.
const (
	// DefaultReadyTimeout is how long a machine has to reach ready before
	// it is destroyed and the run fails (docs/sandbox.md §Provision).
	DefaultReadyTimeout = 2 * time.Minute
	// DefaultStopTimeout bounds `systemctl stop` of the machine unit; the
	// unit's own TimeoutStopSec is 60 s and its helper terminates a
	// machine that will not halt at 20 s, so a stop that outlives this is
	// one the helper's terminate finishes (docs/sandbox.md §Destroy).
	DefaultStopTimeout = 90 * time.Second
)

// ExecNode built exec under a step slice and the step boundary that reaps
// everything (docs/sandbox.md §Exec, §"Step boundary"). It is named here
// because [VerbsNode], which follows it, refers to it in the error a
// not-yet-built git or forge verb returns.
const ExecNode = "machine-exec (mill#62)"

// VerbsNode is what builds the session-facing git and forge verbs of a
// machine workspace, which follow exec: a session in a machine reaches its
// checkout through the socket exactly as one on the local backend does,
// once there is a session in a machine to do so.
const VerbsNode = "the machine verbs node, after " + ExecNode

// ErrNotBuilt is a verb the machine backend does not do yet. Every error a
// verb returns wraps it and names the node that builds the verb; see
// [NotBuiltError].
var ErrNotBuilt = errors.New("machine: not built")

// NotBuiltError is [ErrNotBuilt] with the verb and the node attached.
type NotBuiltError struct {
	// Verb is what was asked for, such as `exec`.
	Verb string
	// Node is the node that builds it.
	Node string
}

func (e *NotBuiltError) Error() string {
	return fmt.Sprintf("machine: %s is not built yet; %s builds it", e.Verb, e.Node)
}

// Unwrap makes errors.Is(err, ErrNotBuilt) true.
func (e *NotBuiltError) Unwrap() error { return ErrNotBuilt }

func notBuilt(verb string) error { return &NotBuiltError{Verb: verb, Node: VerbsNode} }

// ErrNotCreated is a verb called before Create or Attach.
var ErrNotCreated = errors.New("machine: workspace not created")

// ErrExists is Create for a run whose directory is already there.
var ErrExists = errors.New("machine: run directory already exists")

// ErrLostSandbox is Attach for a run whose machine machined no longer
// knows: the run fails with that as evidence and nothing resumes into a
// fresh machine (docs/sandbox.md §Reconciliation). It is the workspace
// package's error, so the run loop tells a lost sandbox from any other
// failure to open a workspace without knowing which backend it has.
var ErrLostSandbox = workspace.ErrLostSandbox

// ErrStillRegistered is a machine machined still lists after a stop and a
// terminate. A run is not finished while its machine is registered
// (docs/sandbox.md §Destroy), so Destroy returns this rather than pretend.
var ErrStillRegistered = errors.New("machine: still registered with machined")

// ReadyTimeoutError is a machine that did not reach ready in time. It
// carries the timeout and the digest, which are the run's evidence.
type ReadyTimeoutError struct {
	// Machine is the machine's name.
	Machine string
	// Image is the digest it booted.
	Image string
	// Timeout is how long it was given.
	Timeout time.Duration
	// Last is what the last readiness probe said.
	Last string
}

func (e *ReadyTimeoutError) Error() string {
	msg := fmt.Sprintf("machine: %s did not reach ready within %s (image %s)", e.Machine, e.Timeout, e.Image)
	if e.Last != "" {
		msg += ": last probe: " + e.Last
	}
	return msg
}

// Config is what the backend is given once, for every run on the worker. A
// run's own facts arrive on each [workspace.Spec] and in its checkout.
type Config struct {
	// Root is the sandbox root, [DefaultRoot] when empty. It must be
	// btrfs; [Check] and `mill doctor` say whether it is.
	Root string
	// Machines is the image subvolume directory, [DefaultMachines] when
	// empty.
	Machines string
	// RunState is where a run's socket directory is made, [DefaultRunState]
	// when empty. The host makes it; the worker cannot create `/run/mill`.
	RunState string
	// Skill is the worker's skill directory, bound read-only into every
	// machine (docs/sandbox.md §"Skill mount"). It must be one of the
	// read-only sources the helper's policy names.
	Skill string
	// Origin is the repository to clone when a spec does not name one.
	Origin string
	// Forge is where the run delivers, and whose credential the clone
	// carries. Nil leaves the forge verbs answering [workspace.ErrNoForge].
	Forge forge.Forge
	// Identity is the address the mill commits under; Name the name.
	Identity string
	Name     string
	// Git is the git binary on the host; `git` when empty.
	Git string
	// Execer spawns git. Nil means a spawner with an empty floor.
	Execer workspace.Execer
	// Redactor is scrubbed through before anything is logged. Nil means
	// one of this workspace's own.
	Redactor *redact.Redactor
	// Log receives the workspace's own lines — a promotion, a stop that
	// needed terminate, a boot the image store had no record of. Nil
	// discards them.
	Log io.Writer
	// Images configures the image store; Root and Machines are filled from
	// this configuration when empty.
	Images image.Config
	// Caches configures the cache store; Root is filled when empty. Its
	// expiry and watermarks are the eviction policy (docs/sandbox.md
	// §Eviction).
	Caches cache.Config
	// ImageExpiry is how long an image goes unbooted before eviction
	// deletes it; zero is the caches' expiry.
	ImageExpiry time.Duration
	// CacheDriver is the driver the cache store reaches the host through.
	// Lineages and a run's copies are the worker's own subvolumes and are
	// created, snapshotted and deleted as its uid, never through sudo — a
	// copy made as root is one the machine's agent cannot write — so on a
	// host where the image verbs need sudo this is the same host without
	// it. Nil means the driver New was given.
	CacheDriver driver.Driver
	// Proxy, NoProxy and Gateway are the run's egress when a spec decides
	// none of its own: [DefaultProxy], [DefaultNoProxy], [DefaultGateway].
	Proxy   string
	NoProxy string
	Gateway string
	// Limits are the unit's resource limits. Empty leaves the unit's
	// defaults.
	Limits Limits
	// NestedRepos lists the repositories whose `[machine].nested` request
	// is granted, each as its path on the forge (`wellspring/mill`) or as
	// its origin URL. An ungranted request is a finding (docs/sandbox.md
	// §mill.toml, §Self-hosting).
	NestedRepos []string
	// DaggerCheckout is the dagger checkout bound read-only into a nested
	// machine; [DefaultDaggerCheckout] when empty.
	DaggerCheckout string
	// ReadyTimeout bounds readiness; [DefaultReadyTimeout] when zero.
	ReadyTimeout time.Duration
	// StopTimeout bounds the machine unit's stop; [DefaultStopTimeout]
	// when zero.
	StopTimeout time.Duration
	// ScratchBudget is how old a scratch project must be before
	// reconciliation sweeps it; [DefaultScratchBudget] when zero.
	ScratchBudget time.Duration
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

func (c Config) withDefaults() Config {
	if c.Root == "" {
		c.Root = DefaultRoot
	}
	if c.Machines == "" {
		c.Machines = DefaultMachines
	}
	if c.RunState == "" {
		c.RunState = DefaultRunState
	}
	if c.DaggerCheckout == "" {
		c.DaggerCheckout = DefaultDaggerCheckout
	}
	if c.Skill == "" {
		if home, err := os.UserHomeDir(); err == nil {
			c.Skill = filepath.Join(home, ".local", "share", "mill", "skill")
		}
	}
	if c.Name == "" {
		c.Name = "mill"
	}
	if c.Identity == "" {
		c.Identity = "mill@localhost"
	}
	if c.Images.Root == "" {
		c.Images.Root = c.Root
	}
	if c.Images.Machines == "" {
		c.Images.Machines = c.Machines
	}
	if c.Caches.Root == "" {
		c.Caches.Root = c.Root
	}
	if c.Proxy == "" {
		c.Proxy = DefaultProxy
	}
	if c.NoProxy == "" {
		c.NoProxy = DefaultNoProxy
	}
	if c.Gateway == "" {
		c.Gateway = DefaultGateway
	}
	if c.ReadyTimeout <= 0 {
		c.ReadyTimeout = DefaultReadyTimeout
	}
	if c.StopTimeout <= 0 {
		c.StopTimeout = DefaultStopTimeout
	}
	if c.ScratchBudget <= 0 {
		c.ScratchBudget = DefaultScratchBudget
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

// runsDir is `<root>/runs`.
func (c Config) runsDir() string { return filepath.Join(c.Root, "runs") }

// runDir is `<root>/runs/<id>`.
func (c Config) runDir(id string) string { return filepath.Join(c.runsDir(), id) }

// SkillDir is the directory bound read-only into every machine as the mill
// skill, with the default applied: what the worker materialises the skill
// into before [Check] asks whether it is there (internal/skill).
func (c Config) SkillDir() string { return c.withDefaults().Skill }

// socketDir is `<run state>/<id>`, bound into the machine at `/run/mill`.
func (c Config) socketDir(id string) string { return filepath.Join(c.RunState, id) }

// Check reports whether this worker could run the machine backend at all:
// the sandbox root is btrfs — and, for a worker inside a machine, under
// the machine's own ephemeral root — its runs directory and the run-state
// directory are the worker's to write, the skill directory is there, and a
// run's socket path fits. It is asked before a node is claimed, so a worker
// configured for machines refuses at start rather than after a claim it
// cannot honour.
func Check(host driver.Driver, c Config) error {
	c = c.withDefaults()
	if InsideMachine(context.Background(), host) {
		if err := driver.CheckEphemeralRoot(c.Root); err != nil {
			return err
		}
	}
	fs, err := host.Statfs(c.Root)
	if err != nil {
		return err
	}
	if !fs.Btrfs() {
		return fmt.Errorf("machine: sandbox root %s is %s, not btrfs (docs/sandbox.md §Storage)", c.Root, fs.Type)
	}
	if err := os.MkdirAll(c.runsDir(), 0o750); err != nil {
		return fmt.Errorf("machine: %w", err)
	}
	if err := writable(c.runsDir()); err != nil {
		return err
	}
	if err := writable(c.RunState); err != nil {
		return fmt.Errorf("%w; the host makes it (homestead's mill_machine role), the worker cannot", err)
	}
	if info, err := os.Stat(c.Skill); err != nil || !info.IsDir() {
		return fmt.Errorf("machine: skill directory %s is not a directory the machine can bind", c.Skill)
	}
	return socket.CheckPath(filepath.Join(c.socketDir(strings.Repeat("0", 26)), SocketName))
}

// InsideMachine is whether this worker runs inside a machine, as
// `systemd-detect-virt --container` says. Such a worker is a nested one:
// its sandbox root defaults to [NestedRoot] and may not leave the
// ephemeral root (docs/sandbox.md §Self-hosting). A host that cannot
// answer — no systemd — is not a machine.
func InsideMachine(ctx context.Context, host driver.Driver) bool {
	manager, err := host.Container(ctx)
	return err == nil && manager == driver.ContainerNspawn
}

// writable is whether the worker may create entries in a directory.
func writable(dir string) error {
	probe, err := os.CreateTemp(dir, ".mill-check-*")
	if err != nil {
		return fmt.Errorf("machine: %s is not a directory the worker can write: %w", dir, err)
	}
	_ = probe.Close()
	return os.Remove(probe.Name())
}

// Workspace is one run's machine workspace.
type Workspace struct {
	host     driver.Driver
	config   Config
	redactor *redact.Redactor
	execer   workspace.Execer
	server   *socket.Server
	images   *image.Store
	caches   *cache.Store

	mu     sync.RWMutex
	handle *workspace.Handle
	token  workspace.Token
	egress workspace.Egress
	record *Record
	// slice is the step slice execs run under, set by BeginStep and
	// cleared by EndStep. An exec with none is refused: nothing would reap
	// what it spawns (docs/sandbox.md §Exec).
	slice string
	// recreate is the ordinal of the slice Attach stopped, which the next
	// BeginStep recreates; zero when there is none.
	recreate int
	// installs are the shim's reports not yet drained into the run's
	// provenance (workspace.Reporter).
	installs []workspace.Install
}

// New returns a workspace for one run, reaching the host through host.
// Nothing exists until Create or Attach.
func New(host driver.Driver, c Config) *Workspace {
	c = c.withDefaults()
	w := &Workspace{host: host, config: c, redactor: c.Redactor, execer: c.Execer}
	if w.redactor == nil {
		w.redactor = redact.New()
	}
	if w.execer == nil {
		// An empty floor: git sees exactly what this package names for it.
		w.execer = workspace.Spawner{Floor: []string{}}
	}
	w.images = image.New(host, c.Images)
	cacheHost := c.CacheDriver
	if cacheHost == nil {
		cacheHost = host
	}
	w.caches = cache.New(cacheHost, c.Caches)
	w.server = socket.New(w, socket.WithLog(nil, w.redactor))
	return w
}

// Attach reopens the workspace of a run this host already has a machine
// for: the record is read, machined is asked for the machine, the step
// slice a previous worker left in flight is stopped, and the socket is
// started again. A machine machined no longer knows is [ErrLostSandbox];
// nothing boots a fresh one for a run that had one.
//
// The slice is stopped because whatever was running in it when the worker
// died is not trusted to still be the step; the next BeginStep recreates
// it under the same ordinal, which [Workspace.NextOrdinal] reports
// (docs/sandbox.md §Reconciliation).
func (w *Workspace) Attach(ctx context.Context, spec workspace.Spec) (*workspace.Handle, error) {
	id, err := IDFor(spec.RunID)
	if err != nil {
		return nil, err
	}
	record, err := readRecord(w.config.runDir(id))
	if err != nil {
		return nil, fmt.Errorf("machine: attach %s: %w", spec.RunID, err)
	}
	shown, err := w.host.ShowMachine(ctx, record.Machine)
	if err != nil {
		if errors.Is(err, driver.ErrNoMachine) {
			return nil, fmt.Errorf("%w: %s is not registered with machined", ErrLostSandbox, record.Machine)
		}
		return nil, err
	}
	if shown.State != driver.StateRunning {
		return nil, fmt.Errorf("%w: %s is %s, not running", ErrLostSandbox, record.Machine, shown.State)
	}
	if record.Slice != "" {
		if err := w.host.StopUnit(ctx, record.Machine, record.Slice); err != nil {
			return nil, fmt.Errorf("machine: attach %s: stop %s left in flight: %w", spec.RunID, record.Slice, err)
		}
		w.logf("stopped %s left in flight in %s; the step resumes in a fresh slice", record.Slice, record.Machine)
		record.Slice = ""
		if err := writeRecord(w.config.runDir(id), record); err != nil {
			return nil, err
		}
		w.mu.Lock()
		w.recreate = record.Ordinal
		w.mu.Unlock()
	}
	return w.open(&record, spec)
}

// open installs the record and the handle and starts the socket: the half
// Create and Attach share.
func (w *Workspace) open(record *Record, spec workspace.Spec) (*workspace.Handle, error) {
	handle := &workspace.Handle{
		RunID:   spec.RunID,
		Kind:    workspace.KindMachine,
		Root:    filepath.Join(w.config.runDir(record.ID), WorkspaceDir),
		Ref:     record.Ref,
		Branch:  record.Branch,
		Socket:  filepath.Join(w.config.socketDir(record.ID), SocketName),
		Sandbox: record.Sandbox(),
	}
	egress := spec.Egress
	if egress.Proxy == "" {
		egress.Proxy, egress.NoProxy = w.config.Proxy, w.config.NoProxy
	}
	if egress.Gateway == "" {
		egress.Gateway = w.config.Gateway
	}
	w.mu.Lock()
	w.record, w.handle, w.egress = record, handle, egress
	w.mu.Unlock()
	if err := w.server.Listen(handle.Socket); err != nil {
		return nil, err
	}
	return w.snapshot(), nil
}

// Rotate installs the step token: the socket authenticates against it from
// the next request, the environment hands it to the gateway, and the
// redactor is seeded with it before it can reach any log.
func (w *Workspace) Rotate(_ context.Context, token workspace.Token) error {
	w.redactor.Add(token.Value)
	w.mu.Lock()
	w.token = token
	if w.handle != nil {
		w.handle.Capabilities = token.Capabilities
	}
	w.mu.Unlock()
	w.server.Rotate(token)
	return nil
}

// Handle is the workspace as it stands, or nil before Create or Attach.
func (w *Workspace) Handle() *workspace.Handle { return w.snapshot() }

// Record is the run's record as it stands, or nil before Create or Attach.
func (w *Workspace) Record() *Record {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.record == nil {
		return nil
	}
	record := *w.record
	return &record
}

// Findings are what the run's first session is told about its
// provisioning: an invalid mill.toml, a derived build that fell back to
// base, an ungranted nested request.
func (w *Workspace) Findings() []string {
	if record := w.Record(); record != nil {
		return append([]string(nil), record.Findings...)
	}
	return nil
}

// ReportInstall takes the `dnf` shim's report of what a session installed
// into the machine, to be drained by [Workspace.Installs]. The packages are
// kept as named: the report is provenance, and provenance says what was
// said.
func (w *Workspace) ReportInstall(_ context.Context, packages []string) error {
	named := make([]string, 0, len(packages))
	for _, pkg := range packages {
		if pkg = strings.TrimSpace(pkg); pkg != "" {
			named = append(named, pkg)
		}
	}
	if len(named) == 0 {
		return errors.New("machine: report.install names no package")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.installs = append(w.installs, workspace.Install{Packages: named})
	return nil
}

// Installs hands over the reports taken since the last call and forgets
// them.
func (w *Workspace) Installs() []workspace.Install {
	w.mu.Lock()
	defer w.mu.Unlock()
	installs := w.installs
	w.installs = nil
	return installs
}

// Committer is the identity the workspace's commits carry.
func (w *Workspace) Committer() workspace.Committer {
	return workspace.Committer{Name: w.config.Name, Email: w.config.Identity}
}

// Environment is what a process the workspace or its harness spawns must
// run under: the route to the world and to the model the machine decided
// (workspace.Egress), with the step token as the gateway's bearer. A
// machine's git identity is the exec node's to decide, because the
// session's git runs inside the machine where no host path reaches.
func (w *Workspace) Environment() map[string]string {
	w.mu.RLock()
	token, egress := w.token, w.egress
	w.mu.RUnlock()
	return egress.Environment(token.Value)
}

// Redact registers values the workspace scrubs from every log line.
func (w *Workspace) Redact(values ...string) { w.redactor.Add(values...) }

// GitRunner runs git in the checkout on the host, through the chokepoint.
func (w *Workspace) GitRunner() forge.Git {
	handle := w.snapshot()
	if handle == nil {
		return gitRunner{w: w}
	}
	return gitRunner{w: w, dir: handle.Root}
}

// Candidate snapshots the run's cache copies as the promotion candidate of
// an evidence-closed step (workspace.CacheCandidate).
func (w *Workspace) Candidate(ctx context.Context, step string) error {
	record := w.Record()
	if record == nil {
		return ErrNotCreated
	}
	return w.caches.Candidate(ctx, record.ID, step)
}

// Read reads a file in the checkout.
func (w *Workspace) Read(_ context.Context, path string) ([]byte, error) {
	resolved, err := w.resolve(path)
	if err != nil {
		return nil, err
	}
	content, err := os.ReadFile(resolved) //nolint:gosec // resolve confines the path to the checkout
	if err != nil {
		return nil, fmt.Errorf("machine: read %s: %w", path, err)
	}
	return content, nil
}

// Write writes a file in the checkout, creating the directories above it.
func (w *Workspace) Write(_ context.Context, path string, content []byte) error {
	resolved, err := w.resolve(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
		return fmt.Errorf("machine: write %s: %w", path, err)
	}
	if err := os.WriteFile(resolved, content, 0o644); err != nil { //nolint:gosec // a file in a checkout the machine's agent reads
		return fmt.Errorf("machine: write %s: %w", path, err)
	}
	return nil
}

// List lists a directory in the checkout.
func (w *Workspace) List(_ context.Context, path string) ([]workspace.Entry, error) {
	resolved, err := w.resolve(path)
	if err != nil {
		return nil, err
	}
	read, err := os.ReadDir(resolved)
	if err != nil {
		return nil, fmt.Errorf("machine: list %s: %w", path, err)
	}
	entries := make([]workspace.Entry, 0, len(read))
	for _, entry := range read {
		listed := workspace.Entry{Name: entry.Name(), Dir: entry.IsDir()}
		if info, err := entry.Info(); err == nil && !entry.IsDir() {
			listed.Size = info.Size()
		}
		entries = append(entries, listed)
	}
	return entries, nil
}

// Git is the git verbs, not built.
func (w *Workspace) Git() workspace.Git { return gitVerbs{} }

// Forge is the forge verbs, not built.
func (w *Workspace) Forge() workspace.ForgeVerbs { return forgeVerbs{} }

// snapshot is the handle, copied.
func (w *Workspace) snapshot() *workspace.Handle {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.handle == nil {
		return nil
	}
	handle := *w.handle
	handle.Capabilities = append([]workspace.Capability(nil), w.handle.Capabilities...)
	if w.handle.Sandbox != nil {
		sandbox := *w.handle.Sandbox
		sandbox.Lineages = append([]string(nil), w.handle.Sandbox.Lineages...)
		handle.Sandbox = &sandbox
	}
	return &handle
}

// resolve turns a path a session named into an absolute one inside the
// checkout, or refuses. Symlinks are followed on the part that exists.
func (w *Workspace) resolve(path string) (string, error) {
	handle := w.snapshot()
	if handle == nil {
		return "", ErrNotCreated
	}
	root, err := filepath.EvalSymlinks(handle.Root)
	if err != nil {
		return "", fmt.Errorf("machine: resolve checkout: %w", err)
	}
	candidate := path
	if candidate == "" {
		candidate = "."
	}
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(root, candidate)
	}
	candidate = filepath.Clean(candidate)
	if !within(root, candidate) {
		return "", fmt.Errorf("machine: %s: %w", path, workspace.ErrOutsideTree)
	}
	existing := candidate
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			break
		}
		existing = parent
	}
	resolved, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", fmt.Errorf("machine: %s: %w", path, err)
	}
	if !within(root, resolved) {
		return "", fmt.Errorf("machine: %s: %w", path, workspace.ErrOutsideTree)
	}
	return candidate, nil
}

// within reports whether path is root or below it.
func within(root, path string) bool {
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

type gitVerbs struct{}

func (gitVerbs) Status(context.Context) (workspace.Status, error) {
	return workspace.Status{}, notBuilt("git.status")
}

func (gitVerbs) Diff(context.Context, workspace.DiffRequest) (string, error) {
	return "", notBuilt("git.diff")
}

func (gitVerbs) Log(context.Context, workspace.LogRequest) ([]workspace.CommitInfo, error) {
	return nil, notBuilt("git.log")
}

func (gitVerbs) Commit(context.Context, workspace.CommitRequest) (workspace.CommitInfo, error) {
	return workspace.CommitInfo{}, notBuilt("git.commit")
}

type forgeVerbs struct{}

func (forgeVerbs) Push(context.Context, workspace.PushRequest) (workspace.PushResult, error) {
	return workspace.PushResult{}, notBuilt("git.push")
}

func (forgeVerbs) PR(context.Context, workspace.PRRequest) (forge.PullRequest, error) {
	return forge.PullRequest{}, notBuilt("forge.pr")
}

func (forgeVerbs) Checks(context.Context, workspace.ChecksRequest) (workspace.ChecksResult, error) {
	return workspace.ChecksResult{}, notBuilt("forge.checks")
}

// The workspace satisfies the interfaces the run loop wants of one.
var (
	_ workspace.Workspace      = (*Workspace)(nil)
	_ workspace.CacheCandidate = (*Workspace)(nil)
	_ workspace.StepBoundary   = (*Workspace)(nil)
	_ workspace.StepResumer    = (*Workspace)(nil)
	_ workspace.Provisioned    = (*Workspace)(nil)
	_ workspace.Reporter       = (*Workspace)(nil)
)
