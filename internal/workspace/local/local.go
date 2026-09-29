// Package local is the workspace of docs/architecture.md's `local` row: a
// fresh clone per run under the mill's own account, on a branch derived from
// the node, reached over a unix socket.
//
// Isolation here is the account and the tool policy, and nothing more. The
// machine and remote workspaces implement the same interface with a real
// boundary; nothing in a workflow definition changes across them, which is
// the point of the interface being where it is.
//
// One value serves one run. Create makes the run's directory, Attach reopens
// it, Destroy removes it, and the socket lives for as long as the value does.
package local

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/steading-ai/mill/internal/forge"
	"github.com/steading-ai/mill/internal/workspace"
	"github.com/steading-ai/mill/internal/workspace/redact"
	"github.com/steading-ai/mill/internal/workspace/signing"
	"github.com/steading-ai/mill/internal/workspace/socket"
	"github.com/steading-ai/mill/internal/workspace/verbs"
)

// Names inside a run's directory. The socket and the signing material sit
// beside the checkout, never in it: a session that can write files must not
// be able to reach the mill's key, and `git status` must not report the
// workspace's own furniture as a change.
const (
	checkoutDir = "checkout"
	socketName  = "ws.sock"
	signingDir  = "signing"
	// browserDir is a mounted MCP server's working directory: per run, like
	// checkoutDir and unlike signingDir, so it is swept away with the rest
	// of the run's directory rather than shared across every run the way
	// the account's signing key is.
	browserDir = "browser"
	// stepDirName holds each step's podman/docker PATH shims, under a
	// subdirectory named for the step's ordinal; the step's TMPDIR is
	// elsewhere, where its path is short (stepTmp). BeginStep creates both
	// and EndStep removes both, so neither outlives the step that opened it
	// (workspace.StepBoundary).
	stepDirName = "step"
	// gitConfigFile is the git configuration every process the workspace
	// spawns runs under, the session's own git included. It carries the
	// run's identity and the mill's signing key, so who a commit is by is
	// the workspace's decision and not the session's, and it sits beside
	// the checkout so the session runs under it without being able to
	// rewrite it.
	gitConfigFile = "gitconfig"
)

// runIDLen is how long the run id in a workspace's path is: the store mints
// run ids as canonical ULIDs, which are 26 characters. It is here so
// [CheckRoot] can answer before a run exists to be named.
const runIDLen = 26

// CheckRoot reports whether a run's workspace socket under root could be
// bound, as a *[socket.PathTooLongError] when it could not.
//
// It is asked before a node is claimed. The socket's path is derived from the
// root and the run's id, so a root that is too long by one byte takes a
// claim, a lease, a clone and a session before failing on the bind — which is
// the failure this exists to move earlier.
func CheckRoot(root string) error {
	return socket.CheckPath(filepath.Join(root, strings.Repeat("0", runIDLen), socketName))
}

// ErrNotCreated is a verb called before Create or Attach.
var ErrNotCreated = errors.New("workspace: not created")

// ErrExists is Create for a run whose directory another run — or an earlier
// generation of this one — already holds.
var ErrExists = errors.New("workspace: directory already exists")

// Options configure a local workspace.
type Options struct {
	// Root is the directory the mill keeps workspaces under, one
	// subdirectory per run. Created 0700 if it does not exist.
	Root string
	// Origin is the repository to clone when a spec does not name one.
	Origin string
	// Git is the git binary. Empty means `git` on the mill's PATH.
	Git string
	// Execer spawns every process, including git's. Nil means a spawner
	// that gives a child nothing but the environment the workspace names.
	Execer workspace.Execer
	// Redactor is scrubbed through before anything is logged. It is seeded
	// with each step token as it is rotated in, and extended by the `redact`
	// verb. Nil means a redactor of this workspace's own.
	Redactor *redact.Redactor
	// Log receives one summary line per verb, through the redactor. Nil
	// discards them.
	Log io.Writer
	// Signer is the mill's commit signing key. Nil means one generated
	// under Root, which is what a development host wants; a deployment
	// passes the account's own key.
	Signer *signing.Key
	// Identity is the address the mill commits under when it generates its
	// own key.
	Identity string
	// Name is the name on the mill's commits.
	Name string
	// Forge is where the run delivers: the push, the pull request, the
	// checks. Nil leaves the forge verbs answering
	// [workspace.ErrNoForge], which is what a run on a project with no
	// repository gets.
	Forge forge.Forge
	// Grace is how long a step's processes may take to stop cleanly once
	// EndStep asks them to, before it escalates to SIGKILL (group mode) or
	// lets systemd's own TimeoutStopSec escalate (scope mode). Zero means
	// [DefaultGrace].
	Grace time.Duration
}

// Workspace is one run's local workspace.
type Workspace struct {
	options  Options
	redactor *redact.Redactor
	execer   workspace.Execer
	verbs    verbs.Verbs
	verbSink *verbs.SinkVar
	server   *socket.Server

	mu        sync.RWMutex
	handle    *workspace.Handle
	token     workspace.Token
	signer    *signing.Key
	gitConfig string
	egress    workspace.Egress

	killOnce sync.Once
	killMode killMode

	stepMu sync.Mutex
	step   *stepState
}

// New returns a workspace for one run. Nothing exists on disk until Create.
func New(options Options) *Workspace {
	w := &Workspace{options: options, redactor: options.Redactor, signer: options.Signer}
	if w.redactor == nil {
		w.redactor = redact.New()
	}
	w.execer = options.Execer
	if w.execer == nil {
		// The floor every child — git, ssh-keygen, a step's own command —
		// stands on is the mill's own environment cut to
		// [workspace.FloorNames]: PATH, so a check finds the toolchain the
		// deployment put on the unit's PATH; the locale and the account;
		// and nothing that names a credential, a database or a project.
		// The rest of what a child sees is what this package names for it.
		w.execer = workspace.Spawner{Floor: workspace.Floor(os.Environ())}
	}
	if w.options.Name == "" {
		w.options.Name = "mill"
	}
	if w.options.Identity == "" {
		w.options.Identity = "mill@localhost"
	}
	w.verbSink = &verbs.SinkVar{}
	w.verbs = verbs.Verbs{
		Host:     verbs.Host{Git: options.Git, Execer: w.execer, Redactor: w.redactor, Sink: w.verbSink},
		Forge:    options.Forge,
		Checkout: w.checkout,
	}
	w.server = socket.New(w, socket.WithLog(options.Log, w.redactor))
	return w
}

// checkout is the run's checkout as the verbs see it at each call: the
// clone, the run's branch, the mill's key and the live step token. It is
// what makes the verbs the same code on every backend
// (internal/workspace/verbs).
func (w *Workspace) checkout() (verbs.Checkout, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.handle == nil {
		return verbs.Checkout{}, ErrNotCreated
	}
	return verbs.Checkout{Dir: w.handle.Root, Branch: w.handle.Branch, Signer: w.signer, Token: w.token}, nil
}

// Create makes the run's workspace: its own directory under Root, readable by
// the mill's account alone, a clone at the spec's ref, and the run's branch
// checked out. The directory is named for the run and the branch derived from
// the node, so two runs can share neither.
func (w *Workspace) Create(ctx context.Context, spec workspace.Spec) (*workspace.Handle, error) {
	if spec.RunID == "" {
		return nil, errors.New("workspace: no run to create a workspace for")
	}
	origin := spec.Origin
	if origin == "" {
		origin = w.options.Origin
	}
	if origin == "" {
		return nil, errors.New("workspace: no origin to clone")
	}
	// Before the clone: a socket that cannot be bound is a workspace that
	// cannot serve, and finding that out after a clone costs the clone.
	if err := socket.CheckPath(filepath.Join(w.dirFor(spec.RunID), socketName)); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(w.options.Root, 0o700); err != nil {
		return nil, fmt.Errorf("workspace: create %s: %w", w.options.Root, err)
	}
	dir := w.dirFor(spec.RunID)
	// Mkdir, not MkdirAll: the run's directory is this run's, and a name
	// already taken is an error rather than a workspace shared with whoever
	// took it.
	if err := os.Mkdir(dir, 0o700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("workspace: %s: %w", dir, ErrExists)
		}
		return nil, fmt.Errorf("workspace: create %s: %w", dir, err)
	}

	// The clone carries the forge's credential, because a private
	// repository refuses an anonymous read and a run that cannot clone
	// never starts.
	auth, err := w.verbs.OriginAuth(ctx, origin)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	clone := make([]string, 0, len(auth.Args)+5)
	clone = append(clone, auth.Args...)
	clone = append(clone, "clone", "--quiet")
	if spec.Ref != "" {
		clone = append(clone, "--branch", spec.Ref)
	}
	clone = append(clone, origin, filepath.Join(dir, checkoutDir))
	if _, err := w.verbs.Host.Run(ctx, w.options.Root, auth.Env, clone...); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	handle, err := w.open(ctx, dir, spec)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return handle, nil
}

// Attach reopens the workspace a run already has on this host, for a resume.
// A worker elsewhere has no directory to attach to and calls Create with the
// run's branch as the spec's ref instead.
func (w *Workspace) Attach(ctx context.Context, spec workspace.Spec) (*workspace.Handle, error) {
	dir := w.dirFor(spec.RunID)
	if _, err := os.Stat(filepath.Join(dir, checkoutDir)); err != nil {
		return nil, fmt.Errorf("workspace: attach %s: %w", spec.RunID, err)
	}
	return w.open(ctx, dir, spec)
}

// open puts the run's branch in place, arranges the signing key, and starts
// the socket. It is the half Create and Attach share.
func (w *Workspace) open(ctx context.Context, dir string, spec workspace.Spec) (*workspace.Handle, error) {
	branch := workspace.BranchFor(spec.NodeRef)
	checkout := filepath.Join(dir, checkoutDir)
	current, err := w.verbs.Host.BranchOf(ctx, checkout)
	if err != nil {
		return nil, err
	}
	// What the clone landed on is the ref the workspace was created at,
	// unless the caller named one; docs/domain-model.md §Workspace records
	// it either way.
	ref := spec.Ref
	if ref == "" {
		ref = current
	}
	if branch != "" && current != branch {
		// -B rather than -b: a workspace recreated from the run's branch
		// already has it, and a resume must land on it either way.
		if _, err := w.git(ctx, checkout, "checkout", "-B", branch); err != nil {
			return nil, err
		}
		current = branch
	}
	if err := w.arrangeSigner(ctx); err != nil {
		return nil, err
	}
	config, err := w.writeGitConfig(dir)
	if err != nil {
		return nil, err
	}
	browser := filepath.Join(dir, browserDir)
	if err := os.MkdirAll(browser, 0o700); err != nil {
		return nil, fmt.Errorf("workspace: create %s: %w", browser, err)
	}

	handle := &workspace.Handle{
		RunID:  spec.RunID,
		Kind:   workspace.KindLocal,
		Root:   checkout,
		Ref:    ref,
		Branch: current,
		Socket: filepath.Join(dir, socketName),
		McpDir: browser,
	}
	w.mu.Lock()
	w.handle = handle
	w.gitConfig = config
	w.egress = spec.Egress
	w.mu.Unlock()

	if err := w.server.Listen(handle.Socket); err != nil {
		return nil, err
	}
	return w.snapshot(), nil
}

// arrangeSigner finds or makes the key every commit is signed with. It lives
// under Root, not under the run, because it is the mill account's key and not
// this run's.
func (w *Workspace) arrangeSigner(ctx context.Context) error {
	w.mu.RLock()
	signer := w.signer
	w.mu.RUnlock()
	if signer != nil {
		return nil
	}
	key, err := signing.Generate(ctx, w.execer, filepath.Join(w.options.Root, signingDir), w.options.Name, w.options.Identity)
	if err != nil {
		return err
	}
	w.mu.Lock()
	w.signer = key
	w.mu.Unlock()
	return nil
}

// writeGitConfig writes the git configuration every process this workspace
// spawns runs under, and returns its path.
//
// It is the same decision [signing.Key.GitConfig] makes for the mill's own
// git — the run's name and address, the mill's key, `commit.gpgsign` — in
// the one form a process the mill did not itself invoke can be held to: a
// file named by `GIT_CONFIG_GLOBAL`. A session commits under the run's
// identity because its git was pointed here, not because the session chose
// to (docs/architecture.md §"The workspace").
//
// The file sits in the run's directory rather than in the checkout, beside
// the key it names. A session may read it and may not change what it says,
// and `git status` never reports it.
func (w *Workspace) writeGitConfig(dir string) (string, error) {
	w.mu.RLock()
	signer := w.signer
	w.mu.RUnlock()
	identity := w.Committer()

	var b strings.Builder
	b.WriteString("# Written by mill for one run. The identity and the key a\n")
	b.WriteString("# commit in this workspace carries are the mill's, not the session's.\n")
	b.WriteString("[user]\n")
	b.WriteString("\tname = " + gitConfigValue(identity.Name) + "\n")
	b.WriteString("\temail = " + gitConfigValue(identity.Email) + "\n")
	if signer != nil {
		b.WriteString("\tsigningkey = " + gitConfigValue(signer.Path) + "\n")
		b.WriteString("[gpg]\n\tformat = ssh\n")
		b.WriteString("[gpg \"ssh\"]\n\tallowedSignersFile = " + gitConfigValue(signer.AllowedSigners) + "\n")
		b.WriteString("[commit]\n\tgpgsign = true\n")
		b.WriteString("[tag]\n\tgpgsign = true\n")
	}

	path := filepath.Join(dir, gitConfigFile)
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return "", fmt.Errorf("workspace: write %s: %w", path, err)
	}
	return path, nil
}

// gitConfigValue renders a value the way git's configuration parser reads it
// back: quoted, with the two characters that would otherwise end or escape
// the string escaped.
func gitConfigValue(value string) string {
	escaped := strings.ReplaceAll(value, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `"` + escaped + `"`
}

// GitEnvironment is what a process the workspace spawns must run git under.
// It is the workspace's answer to "who is this commit by, and what signs
// it", and it is an environment rather than a checkout-local configuration
// because a session that could edit the answer would be deciding it.
//
// `GIT_CONFIG_GLOBAL` displaces the account's `~/.gitconfig` and
// `~/.config/git/config` both, and `GIT_CONFIG_SYSTEM` the host's, so
// nothing outside the workspace is read. The four identity variables say the
// same thing again where no configuration file can be overridden at all,
// which is what makes the answer hold in a checkout a session has written a
// `user.name` into.
//
// Nil before Create or Attach: there is no run to have an identity yet.
func (w *Workspace) GitEnvironment() map[string]string {
	w.mu.RLock()
	config := w.gitConfig
	w.mu.RUnlock()
	if config == "" {
		return nil
	}
	identity := w.Committer()
	return map[string]string{
		"GIT_CONFIG_GLOBAL":   config,
		"GIT_CONFIG_SYSTEM":   os.DevNull,
		"GIT_AUTHOR_NAME":     identity.Name,
		"GIT_AUTHOR_EMAIL":    identity.Email,
		"GIT_COMMITTER_NAME":  identity.Name,
		"GIT_COMMITTER_EMAIL": identity.Email,
	}
}

// Environment is everything the workspace decided for a process it or its
// harness spawns: the git identity of [Workspace.GitEnvironment], and the
// route to the world and to the model the spec named (workspace.Egress).
// The gateway's bearer is the step token as it stands, so a call made after
// Rotate carries the new token and the one it replaced is nowhere but in
// processes that have already ended.
//
// It is read per step, not per run: the run loop asks for it as each session
// is composed, which is what makes the token in a sidecar's environment the
// token the socket is authenticating against.
func (w *Workspace) Environment() map[string]string {
	w.mu.RLock()
	token := w.token
	w.mu.RUnlock()
	return w.environmentFor(token)
}

// environmentFor is [Workspace.Environment] for a token the caller has
// already read, so that a child's STEP_TOKEN and its ANTHROPIC_AUTH_TOKEN
// are one read and cannot straddle a rotation.
func (w *Workspace) environmentFor(token workspace.Token) map[string]string {
	env := w.GitEnvironment()
	if env == nil {
		env = map[string]string{}
	}
	w.mu.RLock()
	egress, handle := w.egress, w.handle
	w.mu.RUnlock()
	for name, value := range egress.Environment(token.Value) {
		env[name] = value
	}
	// SKIFT_SOCKET and MILL_SOCKET are real, dead paths under the run's own
	// directory, so a child that names neither flag and finds neither in a
	// real environment does not fall back to the compiled-in production
	// sockets, which the local backend's shared filesystem and uid make
	// reachable (workspace.DeadSockets).
	workspace.MergeDeadSockets(env, handle)
	return env
}

// Close stops the socket and leaves the checkout where it is. It is what a
// worker does when it releases a lease at a gate or shuts down: a run that is
// waiting or parked keeps its workspace, so the worker that resumes it — or
// the human inspecting it — finds the tree intact (docs/domain-model.md
// §Workspace). Attach reopens it.
func (w *Workspace) Close(ctx context.Context) error {
	err := w.server.Close(ctx)
	w.mu.Lock()
	w.token = workspace.Token{}
	w.mu.Unlock()
	w.server.Rotate(workspace.Token{})
	return err
}

// Destroy stops the socket and removes the run's directory. The signing key,
// which lives above it, survives.
func (w *Workspace) Destroy(ctx context.Context) error {
	err := w.server.Close(ctx)
	w.mu.Lock()
	handle := w.handle
	w.handle = nil
	w.token = workspace.Token{}
	w.mu.Unlock()
	if handle == nil {
		return err
	}
	if removeErr := os.RemoveAll(filepath.Dir(handle.Root)); removeErr != nil && err == nil {
		err = removeErr
	}
	return err
}

// Rotate installs the step token: the socket authenticates against it from
// the next request, Exec hands it to children, and the redactor is seeded
// with it before it can reach any log. The token it replaces stops working at
// once, which is what closing a step's access means.
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

// Handle is the workspace as it stands, or nil before Create.
func (w *Workspace) Handle() *workspace.Handle { return w.snapshot() }

// SigningKey is the key commits are signed with, so a deployment can publish
// its allowed_signers line. Nil before Create.
func (w *Workspace) SigningKey() *signing.Key {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.signer
}

// Exec runs a command inside the checkout. This is the chokepoint: cwd is the
// clone, the account is the mill's own, and the environment is exactly the
// socket path, the current step token, the run's git identity and the route
// the workspace decided to the world and the model — nothing of the mill's
// own process reaches the child, so a transcript that leaks holds nothing
// reusable.
//
// The command's own name is still resolved against the mill's PATH, because
// resolution happens in the parent; what the child is given is the map below
// and nothing else.
func (w *Workspace) Exec(ctx context.Context, command workspace.Command) (*workspace.Process, error) {
	handle := w.snapshot()
	if handle == nil {
		return nil, ErrNotCreated
	}
	dir, err := w.resolve(command.Dir)
	if err != nil {
		return nil, err
	}
	w.mu.RLock()
	token := w.token
	w.mu.RUnlock()
	env := map[string]string{
		"WORKSPACE_SOCKET": handle.Socket,
		"STEP_TOKEN":       token.Value,
	}
	// A child that runs git commits as the run, signed with the mill's key,
	// and reads no configuration of the account's; a child that reaches out
	// goes through the proxy and the gateway the run was given. Those are
	// the workspace's decisions wherever the process runs, so they are made
	// here rather than left to whatever spawned it.
	for name, value := range w.environmentFor(token) {
		env[name] = value
	}
	return attemptExecer{w: w}.Exec(ctx, workspace.Command{
		Path: command.Path,
		Args: command.Args,
		Dir:  dir,
		Env:  env,
	})
}

// SessionExec is the execer a skill step's session sidecar — and everything
// it spawns beneath it, an e2e stack, a browser, a `go build` — runs
// through, reached by [workspace.SessionExecer] rather than by Exec above:
// the harness adapter already builds a session's whole environment and
// calls with ClearEnv true (docs/architecture.md §"Skill steps: the harness
// adapter"), so this hands back the step boundary's own decorator
// undecorated by Exec's WORKSPACE_SOCKET/STEP_TOKEN construction, which
// only adds to whatever Command.Env the caller already built rather than
// replacing it.
func (w *Workspace) SessionExec() workspace.Execer { return attemptExecer{w: w} }

// Read reads a file in the checkout.
func (w *Workspace) Read(_ context.Context, path string) ([]byte, error) {
	resolved, err := w.resolve(path)
	if err != nil {
		return nil, err
	}
	content, err := os.ReadFile(resolved) //nolint:gosec // resolve confines the path to the checkout
	if err != nil {
		return nil, fmt.Errorf("workspace: read %s: %w", path, err)
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
		return fmt.Errorf("workspace: write %s: %w", path, err)
	}
	if err := os.WriteFile(resolved, content, 0o644); err != nil { //nolint:gosec // a file in a checkout, inside a directory only the mill's account can read
		return fmt.Errorf("workspace: write %s: %w", path, err)
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
		return nil, fmt.Errorf("workspace: list %s: %w", path, err)
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

// Git is the git verbs, run against this checkout with the mill's identity
// and signing key.
func (w *Workspace) Git() workspace.Git { return w.verbs.Git() }

// Forge is the forge verbs, bound to the step whose token the workspace
// currently holds.
func (w *Workspace) Forge() workspace.ForgeVerbs { return w.verbs.ForgeVerbs() }

// GitRunner runs git in the run's checkout through the same chokepoint the
// forge uses. The `commit` verifier needs it — asking git whether an object
// exists and whether it is on the run's branch is a fact about the
// repository, not about the session — and the run loop is what wires the two
// together (internal/evidence.Git).
func (w *Workspace) GitRunner() forge.Git { return w.verbs.Runner() }

// CatchUp brings the run's branch up to date with request.Target, the merge
// gate's own act (internal/run/gate.go).
func (w *Workspace) CatchUp(ctx context.Context, request workspace.CatchUpRequest) (workspace.CatchUpResult, error) {
	return w.verbs.CatchUp(ctx, request)
}

// Committer is the identity every commit this workspace makes carries. It is
// read from the signing key when one is arranged, because that key's name and
// address are the ones git is handed, and from the options until then.
func (w *Workspace) Committer() workspace.Committer {
	w.mu.RLock()
	signer := w.signer
	w.mu.RUnlock()
	if signer != nil {
		return workspace.Committer{Name: signer.Name, Email: signer.Identity}
	}
	return workspace.Committer{Name: w.options.Name, Email: w.options.Identity}
}

// Redact registers values the workspace must scrub from every line it logs
// from here on.
func (w *Workspace) Redact(values ...string) { w.redactor.Add(values...) }

// SetActivitySink installs sink, or clears it with nil (workspace.ActivitySink).
func (w *Workspace) SetActivitySink(sink workspace.Sink) { w.verbSink.Set(sink) }

// CaptureTwin implements workspace.TwinSource by delegating to CaptureTwin,
// the free function, with this workspace's own root and run id — the run
// id recovery (which has no live workspace) parses back out of the live
// activity file's path instead.
func (w *Workspace) CaptureTwin(ctx context.Context, sessionID string) (io.Reader, error) {
	return CaptureTwin(ctx, w.options.Root, w.snapshot().RunID, sessionID)
}

// snapshot is the handle, copied, so a caller cannot edit the workspace's own.
func (w *Workspace) snapshot() *workspace.Handle {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.handle == nil {
		return nil
	}
	handle := *w.handle
	handle.Capabilities = append([]workspace.Capability(nil), w.handle.Capabilities...)
	return &handle
}

func (w *Workspace) dirFor(runID string) string {
	return filepath.Join(w.options.Root, runID)
}

// CaptureTwin tars every transcript file Claude Code left for root's runID:
// every `.jsonl` under `$HOME/.claude/projects/<cwd-slug>`, cwd being the
// run's checkout directory and cwd-slug its encoding
// (docs/spikes/sdk-sessions.md §Q1) — not only sessionID's own file, since a
// subagent's session id is not known in advance. sessionID names nothing
// here; it is part of the signature to match [workspace.TwinSource], which
// a backend needing it to select files would use.
//
// A directory with nothing in it — an encoding drift, or a session that
// never wrote a transcript — returns an empty tar and no error: a missing
// twin is the caller's call to make (docs/activity.md), not this
// function's.
func CaptureTwin(ctx context.Context, root, runID, sessionID string) (io.Reader, error) {
	_ = sessionID
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("capture twin: %w", err)
	}
	dir := filepath.Join(home, ".claude", "projects", cwdSlug(filepath.Join(root, runID, checkoutDir)))
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("capture twin: %w", err)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("capture twin: %s: %w", entry.Name(), err)
		}
		if err := tw.WriteHeader(&tar.Header{Name: entry.Name(), Size: int64(len(data)), Mode: 0o640}); err != nil {
			return nil, fmt.Errorf("capture twin: %s: %w", entry.Name(), err)
		}
		if _, err := tw.Write(data); err != nil {
			return nil, fmt.Errorf("capture twin: %s: %w", entry.Name(), err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("capture twin: %w", err)
	}
	return &buf, nil
}

// cwdSlug is Claude Code's own transcript-directory naming rule
// (docs/spikes/sdk-sessions.md §Q1, observed CLI behavior rather than a
// documented contract): every '/' and '.' in cwd becomes '-'.
func cwdSlug(cwd string) string {
	slug := strings.ReplaceAll(cwd, "/", "-")
	return strings.ReplaceAll(slug, ".", "-")
}

// resolve turns a path a session named into an absolute one inside the
// checkout, or refuses. Symlinks are followed on the part of the path that
// exists, so a link out of the tree is caught rather than trusted.
func (w *Workspace) resolve(path string) (string, error) {
	handle := w.snapshot()
	if handle == nil {
		return "", ErrNotCreated
	}
	root, err := filepath.EvalSymlinks(handle.Root)
	if err != nil {
		return "", fmt.Errorf("workspace: resolve checkout: %w", err)
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
		return "", fmt.Errorf("workspace: %s: %w", path, workspace.ErrOutsideTree)
	}
	// Whatever of the path already exists may be a symlink; the part that
	// does not cannot be one yet.
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
		return "", fmt.Errorf("workspace: %s: %w", path, err)
	}
	if !within(root, resolved) {
		return "", fmt.Errorf("workspace: %s: %w", path, workspace.ErrOutsideTree)
	}
	return candidate, nil
}

// within reports whether path is root or below it.
func within(root, path string) bool {
	if path == root {
		return true
	}
	return strings.HasPrefix(path, root+string(filepath.Separator))
}

// git runs one git command against dir and returns its standard output,
// through the verbs' host git: the inspector's read of a checkout no run
// holds.
func (w *Workspace) git(ctx context.Context, dir string, args ...string) (string, error) {
	return w.verbs.Host.Run(ctx, dir, nil, args...)
}

// Workspace is the interface this implements; a compile-time check, because
// the worker holds the interface and never this type.
var _ workspace.Workspace = (*Workspace)(nil)

var _ workspace.ActivitySink = (*Workspace)(nil)

var _ workspace.StepBoundary = (*Workspace)(nil)

var _ workspace.SessionExecer = (*Workspace)(nil)
