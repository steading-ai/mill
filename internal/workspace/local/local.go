// Package local is the workspace of docs/architecture.md's `local` row: a
// fresh clone per run under the mill's own account, on a branch derived from
// the node, reached over a unix socket.
//
// Isolation here is the account and the tool policy, and nothing more. The
// container and remote workspaces implement the same interface with a real
// boundary; nothing in a workflow definition changes across them, which is
// the point of the interface being where it is.
//
// One value serves one run. Create makes the run's directory, Attach reopens
// it, Destroy removes it, and the socket lives for as long as the value does.
package local

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/ivy/mill/internal/workspace"
	"github.com/ivy/mill/internal/workspace/redact"
	"github.com/ivy/mill/internal/workspace/signing"
	"github.com/ivy/mill/internal/workspace/socket"
)

// Names inside a run's directory. The socket and the signing material sit
// beside the checkout, never in it: a session that can write files must not
// be able to reach the mill's key, and `git status` must not report the
// workspace's own furniture as a change.
const (
	checkoutDir = "checkout"
	socketName  = "ws.sock"
	signingDir  = "signing"
)

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
}

// Workspace is one run's local workspace.
type Workspace struct {
	options  Options
	redactor *redact.Redactor
	execer   workspace.Execer
	server   *socket.Server

	mu     sync.RWMutex
	handle *workspace.Handle
	token  workspace.Token
	signer *signing.Key
}

// keyMu serialises generating the mill's signing key, which every workspace
// under one Root shares. Two runs starting at once would otherwise race to
// write it. Workers on the same host in different processes are not a case
// MVP has: one mill runs per account.
var keyMu sync.Mutex

// New returns a workspace for one run. Nothing exists on disk until Create.
func New(options Options) *Workspace {
	w := &Workspace{options: options, redactor: options.Redactor, signer: options.Signer}
	if w.redactor == nil {
		w.redactor = redact.New()
	}
	w.execer = options.Execer
	if w.execer == nil {
		// A floor of no variables at all, so every child — git, ssh-keygen,
		// a step's own command — sees exactly what this package names for
		// it and nothing the mill's own process happens to carry.
		w.execer = workspace.Spawner{Floor: []string{}}
	}
	if w.options.Name == "" {
		w.options.Name = "mill"
	}
	if w.options.Identity == "" {
		w.options.Identity = "mill@localhost"
	}
	w.server = socket.New(w, socket.WithLog(options.Log, w.redactor))
	return w
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

	clone := []string{"clone", "--quiet"}
	if spec.Ref != "" {
		clone = append(clone, "--branch", spec.Ref)
	}
	clone = append(clone, origin, filepath.Join(dir, checkoutDir))
	if _, err := w.git(ctx, w.options.Root, clone...); err != nil {
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
	current, err := w.branchOf(ctx, checkout)
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

	handle := &workspace.Handle{
		RunID:  spec.RunID,
		Kind:   workspace.KindLocal,
		Root:   checkout,
		Ref:    ref,
		Branch: current,
		Socket: filepath.Join(dir, socketName),
	}
	w.mu.Lock()
	w.handle = handle
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
	keyMu.Lock()
	defer keyMu.Unlock()
	key, err := signing.Generate(ctx, w.execer, filepath.Join(w.options.Root, signingDir), w.options.Name, w.options.Identity)
	if err != nil {
		return err
	}
	w.mu.Lock()
	w.signer = key
	w.mu.Unlock()
	return nil
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
// socket path and the current step token — nothing of the mill's own process
// reaches the child, so a transcript that leaks holds nothing reusable.
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
	return w.execer.Exec(ctx, workspace.Command{
		Path: command.Path,
		Args: command.Args,
		Dir:  dir,
		Env: map[string]string{
			"WORKSPACE_SOCKET": handle.Socket,
			"STEP_TOKEN":       token.Value,
		},
	})
}

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
func (w *Workspace) Git() workspace.Git { return gitVerbs{w: w} }

// Redact registers values the workspace must scrub from every line it logs
// from here on.
func (w *Workspace) Redact(values ...string) { w.redactor.Add(values...) }

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

// git runs one git command and returns its standard output. Its environment
// is the workspace's, not the mill's: no global or system config is read, no
// terminal prompt can block it, and the identity is the mill's own.
func (w *Workspace) git(ctx context.Context, dir string, args ...string) (string, error) {
	binary := w.options.Git
	if binary == "" {
		binary = "git"
	}
	process, err := w.execer.Exec(ctx, workspace.Command{
		Path: binary,
		Args: args,
		Dir:  dir,
		Env:  w.gitEnv(dir),
	})
	if err != nil {
		return "", fmt.Errorf("workspace: git %s: %w", args[0], err)
	}
	_ = process.Stdin.Close()
	var stderr []byte
	var stderrErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		stderr, stderrErr = io.ReadAll(process.Stderr)
	}()
	stdout, stdoutErr := io.ReadAll(process.Stdout)
	<-done
	result := process.Wait()
	if stdoutErr != nil {
		return "", fmt.Errorf("workspace: git %s: %w", args[0], stdoutErr)
	}
	if stderrErr != nil {
		return "", fmt.Errorf("workspace: git %s: %w", args[0], stderrErr)
	}
	if result.Status != 0 {
		return string(stdout), fmt.Errorf("workspace: git %s exited %d: %s",
			args[0], result.Status, w.redactor.String(strings.TrimSpace(string(stderr))))
	}
	return string(stdout), nil
}

// gitEnv is what git and everything it spawns — ssh-keygen for a signature,
// above all — is given. HOME points inside the workspace so nothing of the
// operator's own configuration is read.
func (w *Workspace) gitEnv(dir string) map[string]string {
	home := dir
	if handle := w.snapshot(); handle != nil {
		home = filepath.Dir(handle.Root)
	}
	return map[string]string{
		"PATH":                os.Getenv("PATH"),
		"HOME":                home,
		"GIT_CONFIG_GLOBAL":   os.DevNull,
		"GIT_CONFIG_SYSTEM":   os.DevNull,
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_ASKPASS":         "true",
		"LC_ALL":              "C",
	}
}

// branchOf is the branch a checkout is on, empty when it is detached.
func (w *Workspace) branchOf(ctx context.Context, checkout string) (string, error) {
	out, err := w.git(ctx, checkout, "branch", "--show-current")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// config is the identity and signing arguments every git invocation that
// writes history carries.
func (w *Workspace) config() []string {
	w.mu.RLock()
	signer := w.signer
	w.mu.RUnlock()
	return signer.GitConfig()
}

// gitVerbs implements workspace.Git against the run's checkout.
type gitVerbs struct{ w *Workspace }

// Status reports the branch and every path git sees as changed.
func (g gitVerbs) Status(ctx context.Context) (workspace.Status, error) {
	handle := g.w.snapshot()
	if handle == nil {
		return workspace.Status{}, ErrNotCreated
	}
	out, err := g.w.git(ctx, handle.Root, "status", "--porcelain=v1")
	if err != nil {
		return workspace.Status{}, err
	}
	status := workspace.Status{Branch: handle.Branch, Clean: true}
	for _, line := range strings.Split(out, "\n") {
		if len(line) < 4 {
			continue
		}
		status.Clean = false
		status.Files = append(status.Files, workspace.FileStatus{Code: line[:2], Path: strings.TrimSpace(line[3:])})
	}
	return status, nil
}

// Diff renders a unified diff of the working tree, or of the index.
func (g gitVerbs) Diff(ctx context.Context, request workspace.DiffRequest) (string, error) {
	handle := g.w.snapshot()
	if handle == nil {
		return "", ErrNotCreated
	}
	args := []string{"diff"}
	if request.Staged {
		args = append(args, "--staged")
	}
	args = append(args, pathspec(request.Paths)...)
	return g.w.git(ctx, handle.Root, args...)
}

// logFormat separates a commit's fields with the unit separator, which no
// field of a commit can contain.
const logFormat = "--format=%H%x1f%an <%ae>%x1f%aI%x1f%s"

// Log reads the branch's history, newest first.
func (g gitVerbs) Log(ctx context.Context, request workspace.LogRequest) ([]workspace.CommitInfo, error) {
	handle := g.w.snapshot()
	if handle == nil {
		return nil, ErrNotCreated
	}
	limit := request.Limit
	if limit <= 0 {
		limit = 20
	}
	args := append([]string{"log", "-n", strconv.Itoa(limit), logFormat}, pathspec(request.Paths)...)
	out, err := g.w.git(ctx, handle.Root, args...)
	if err != nil {
		return nil, err
	}
	var commits []workspace.CommitInfo
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Split(line, "\x1f")
		if len(fields) != 4 {
			continue
		}
		commits = append(commits, workspace.CommitInfo{SHA: fields[0], Author: fields[1], Date: fields[2], Subject: fields[3]})
	}
	return commits, nil
}

// Commit stages and commits on the run's branch, signed with the mill's key.
// A checkout that has wandered off the run's branch is refused rather than
// committed to: the branch is the run's whole claim on the repository.
func (g gitVerbs) Commit(ctx context.Context, request workspace.CommitRequest) (workspace.CommitInfo, error) {
	handle := g.w.snapshot()
	if handle == nil {
		return workspace.CommitInfo{}, ErrNotCreated
	}
	if request.Message == "" {
		return workspace.CommitInfo{}, errors.New("workspace: a commit needs a message")
	}
	current, err := g.w.branchOf(ctx, handle.Root)
	if err != nil {
		return workspace.CommitInfo{}, err
	}
	if current != handle.Branch {
		return workspace.CommitInfo{}, fmt.Errorf("workspace: checkout is on %q, not the run's branch %q", current, handle.Branch)
	}

	add := append([]string{"add", "--all", "--"}, request.Paths...)
	if len(request.Paths) == 0 {
		add = []string{"add", "--all", "."}
	}
	if _, err := g.w.git(ctx, handle.Root, add...); err != nil {
		return workspace.CommitInfo{}, err
	}

	commit := append(g.w.config(), "commit", "--message", request.Message)
	if request.AllowEmpty {
		commit = append(commit, "--allow-empty")
	}
	if len(request.Paths) > 0 {
		commit = append(commit, pathspec(request.Paths)...)
	}
	if _, err := g.w.git(ctx, handle.Root, commit...); err != nil {
		return workspace.CommitInfo{}, err
	}

	out, err := g.w.git(ctx, handle.Root, "log", "-n", "1", logFormat)
	if err != nil {
		return workspace.CommitInfo{}, err
	}
	fields := strings.Split(strings.TrimSpace(out), "\x1f")
	if len(fields) != 4 {
		return workspace.CommitInfo{}, fmt.Errorf("workspace: unreadable commit record %q", out)
	}
	return workspace.CommitInfo{SHA: fields[0], Author: fields[1], Date: fields[2], Subject: fields[3]}, nil
}

// pathspec renders paths as git's `-- <path>…`, or nothing.
func pathspec(paths []string) []string {
	if len(paths) == 0 {
		return nil
	}
	return append([]string{"--"}, paths...)
}

// Workspace is the interface this implements; a compile-time check, because
// the worker holds the interface and never this type.
var _ workspace.Workspace = (*Workspace)(nil)
