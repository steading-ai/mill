// Package verbs is the git and forge half of a workspace: the seven verbs a
// session reaches over the socket — `git.status`, `git.diff`, `git.log`,
// `git.commit`, `git.push`, `forge.pr`, `forge.checks` — run on the host
// against the run's checkout, with the mill's identity, the mill's signing
// key and the mill's forge credential.
//
// They live here rather than in a backend because they are the same verbs
// wherever the session works. The local backend's checkout is under its own
// run directory and the machine backend's is `runs/<id>/workspace`, bound
// idmapped into the machine so that uid 1000 inside is the worker outside
// (docs/sandbox.md §Filesystem); on the host both are a directory git can
// be pointed at. The machine is where a session works and the host is where
// the mill signs: nothing about git needs to enter the machine, so no
// credential does (docs/core-principles.md §9, docs/sandbox.md §Verbs).
//
// A backend hands the verbs a [Host] — how git is spawned — and a
// [Checkout] as it stands at each call: where the clone is, which branch is
// the run's, what signs, and which step token the forge is bound to. The
// backend's own not-created error comes back from that function when there
// is no run yet, and every verb passes it through.
package verbs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/ivy/mill/internal/forge"
	"github.com/ivy/mill/internal/workspace"
	"github.com/ivy/mill/internal/workspace/redact"
	"github.com/ivy/mill/internal/workspace/signing"
)

// Host is git on the host: the binary, the chokepoint it is spawned through,
// and the redactor its stderr is scrubbed with before it reaches an error.
type Host struct {
	// Git is the git binary. Empty means `git` on the mill's PATH.
	Git string
	// Execer spawns it. Required.
	Execer workspace.Execer
	// Redactor scrubs what git says before an error quotes it. Nil means
	// nothing is scrubbed, which is only right for a host with no
	// credential in play.
	Redactor *redact.Redactor
	// Sink records the verb's own activity (docs/activity.md's
	// verb_started/verb_output/verb). Nil records nothing. A backend wires
	// one in per attempt through [workspace.ActivitySink], so this is
	// ordinarily a [SinkVar]: a value stable across every copy of Host that
	// gets handed out (gitVerbs, forgeVerbs, runner all hold one), whose
	// target is what actually changes.
	Sink workspace.Sink
}

// SinkVar is a [workspace.Sink] whose target may be set after construction
// and read concurrently. Host is copied by value everywhere it is used
// (gitVerbs{v: v}, forgeVerbs{v: v}, runner{v: v}), so a Sink field set once
// at construction could never be changed for all of them at once, and a bare
// mutable field would race the socket's own goroutines against whichever
// step is opening or closing an attempt's writer (internal/run's
// attachActivitySink, detachActivitySink) — the same reason Host.Redactor is
// a pointer to a self-synchronized type rather than a value.
type SinkVar struct {
	mu     sync.RWMutex
	target workspace.Sink
}

// Set installs sink, or clears it with nil.
func (v *SinkVar) Set(sink workspace.Sink) {
	v.mu.Lock()
	v.target = sink
	v.mu.Unlock()
}

func (v *SinkVar) get() workspace.Sink {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.target
}

func (v *SinkVar) Started(verb string, args map[string]string) {
	if s := v.get(); s != nil {
		s.Started(verb, args)
	}
}

func (v *SinkVar) Output(verb, stream string, data []byte) {
	if s := v.get(); s != nil {
		s.Output(verb, stream, data)
	}
}

func (v *SinkVar) Ended(verb string, err error) {
	if s := v.get(); s != nil {
		s.Ended(verb, err)
	}
}

var _ workspace.Sink = (*SinkVar)(nil)

// Run runs one git command against dir and returns its standard output.
//
// The directory travels as `-C dir` in argv, so a record of the invocation
// says where it ran, and as the process's working directory, so anything
// relative resolves the same way. The environment is the workspace's and
// not the mill's: no global or system configuration is read, no terminal
// prompt can block, and extra goes over that. HOME is the directory above
// dir — the run's own directory when dir is its checkout — so that what
// git and ssh-keygen would read from a home is never inside the tree a
// session writes to, and never the operator's.
// A credential reaches git through extra and a credential helper, so it
// appears in neither the remote URL nor argv (docs/architecture.md
// §"Identity and credentials"); every value in extra is registered with the
// redactor first, so a failure that quotes git's stderr cannot quote it.
func (h Host) Run(ctx context.Context, dir string, extra map[string]string, args ...string) (out string, err error) {
	binary := h.Git
	if binary == "" {
		binary = "git"
	}
	subcommand := gitVerb(args)
	verb := "git." + subcommand
	if h.Sink != nil {
		// No arguments: there is no verb-agnostic way to turn raw argv into
		// VerbStarted's "never a session's free text" (a commit's own
		// `--message` is exactly that), so this chokepoint reports none.
		h.Sink.Started(verb, nil)
		defer func() { h.Sink.Ended(verb, err) }()
	}
	env := map[string]string{
		"PATH":                os.Getenv("PATH"),
		"HOME":                filepath.Dir(dir),
		"GIT_CONFIG_GLOBAL":   os.DevNull,
		"GIT_CONFIG_SYSTEM":   os.DevNull,
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_ASKPASS":         "true",
		"LC_ALL":              "C",
	}
	for name, value := range extra {
		env[name] = value
		if h.Redactor != nil {
			h.Redactor.Add(value)
		}
	}
	process, err := h.Execer.Exec(ctx, workspace.Command{
		Path: binary,
		Args: append([]string{"-C", dir}, args...),
		Dir:  dir,
		Env:  env,
	})
	if err != nil {
		err = fmt.Errorf("workspace: git %s: %w", subcommand, err)
		return "", err
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
	if h.Sink != nil {
		if len(stdout) > 0 {
			h.Sink.Output(verb, "stdout", stdout)
		}
		if len(stderr) > 0 {
			h.Sink.Output(verb, "stderr", stderr)
		}
	}
	if stdoutErr != nil {
		err = fmt.Errorf("workspace: git %s: %w", subcommand, stdoutErr)
		return "", err
	}
	if stderrErr != nil {
		err = fmt.Errorf("workspace: git %s: %w", subcommand, stderrErr)
		return "", err
	}
	if result.Status != 0 {
		err = fmt.Errorf("workspace: git %s exited %d: %s",
			subcommand, result.Status, h.redacted(strings.TrimSpace(string(stderr))))
		return string(stdout), err
	}
	return string(stdout), nil
}

func (h Host) redacted(s string) string {
	if h.Redactor == nil {
		return s
	}
	return h.Redactor.String(s)
}

// BranchOf is the branch a checkout is on, empty when it is detached.
func (h Host) BranchOf(ctx context.Context, dir string) (string, error) {
	out, err := h.Run(ctx, dir, nil, "branch", "--show-current")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// gitVerb is the subcommand in a git invocation, for naming it in an error.
// It is not args[0] whenever the command carries a credential helper, which
// arrives as git's own `-c` options ahead of the subcommand.
func gitVerb(args []string) string {
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "-c", "-C", "--config-env":
			index++
		default:
			if strings.HasPrefix(args[index], "-") {
				continue
			}
			return args[index]
		}
	}
	return "git"
}

// Checkout is the run's checkout as the backend reports it at the moment a
// verb is called.
type Checkout struct {
	// Dir is the clone on the host.
	Dir string
	// Branch is the run's branch, the only one the verbs write to or push.
	Branch string
	// Signer signs every commit the verbs make and is what a push holds
	// the branch's commits to. Nil signs nothing and holds nothing, which
	// only a workspace that has arranged no key is.
	Signer *signing.Key
	// Token is the step token as it stands: what the forge is bound to
	// when a verb mints a credential.
	Token workspace.Token
}

// Verbs are the git and forge verbs over one workspace's checkout.
type Verbs struct {
	// Host spawns git.
	Host Host
	// Forge is where the run delivers. Nil leaves the forge verbs answering
	// [workspace.ErrNoForge].
	Forge forge.Forge
	// Checkout reports the checkout as it stands, or the backend's own error
	// for a workspace that has no run yet.
	Checkout func() (Checkout, error)
}

// Git is the four git verbs.
func (v Verbs) Git() workspace.Git { return gitVerbs{v: v} }

// ForgeVerbs is the three forge verbs, bound to the step whose token the
// checkout reports at each call. Binding per call rather than at
// construction is what makes a credential the step's: the client mints one
// for whichever step is live when the call is made, and a workspace between
// steps has no step to mint for.
func (v Verbs) ForgeVerbs() workspace.ForgeVerbs { return forgeVerbs{v: v} }

// Runner is git in the checkout as [forge.Git]: what the forge pushes
// through, and what the `commit` verifier asks about the repository
// (internal/evidence.Git). Each call reads the checkout afresh, so a runner
// made before Create answers with the backend's not-created error rather
// than a stale path.
func (v Verbs) Runner() forge.Git { return runner{v: v} }

// OriginAuth is the credential a git command against origin runs with — the
// clone, above all — bound to no step, because none is live when a clone
// runs: the worker's own credential, as the push later carries it.
//
// The zero value — no credential, no arguments — is what a run whose project
// has no forge gets, and what a run pointed at some other origin gets: the
// mill presents its forge credential to its forge's repository and to
// nothing else, so `--origin` cannot be used to send the token somewhere it
// does not belong (docs/prd.md R-37).
func (v Verbs) OriginAuth(ctx context.Context, origin string) (forge.GitAuth, error) {
	if v.Forge == nil || !SameRepo(origin, v.Remote()) {
		return forge.GitAuth{}, nil
	}
	auth, err := forge.Bind(v.Forge, forge.Step{}).Authenticate(ctx)
	if err != nil {
		return forge.GitAuth{}, err
	}
	if v.Host.Redactor != nil {
		for _, value := range auth.Env {
			v.Host.Redactor.Add(value)
		}
	}
	return auth, nil
}

// CatchUp brings the run's branch up to date with request.Target, the mill's
// own act at the merge gate rather than a session's verb. Fetching, deciding,
// and (when it can) finishing the merge itself all happen here rather than
// stepwise over the socket, because the credential a fetch or a push needs is
// exactly the one docs/sandbox.md keeps out of the machine a session runs in.
//
// The two sides are compared by the files each changed since they diverged,
// not by asking git to merge and seeing what happens: a clean git merge can
// still silently combine two changes to the same function into nonsense, so
// any file both sides touched routes back to a human by way of `implement`
// and `commit`, merged or left with conflict markers as git finds it.
func (v Verbs) CatchUp(ctx context.Context, request workspace.CatchUpRequest) (workspace.CatchUpResult, error) {
	if v.Forge == nil {
		return workspace.CatchUpResult{}, workspace.ErrNoForge
	}
	checkout, err := v.Checkout()
	if err != nil {
		return workspace.CatchUpResult{}, err
	}
	if checkout.Branch == "" {
		return workspace.CatchUpResult{}, errors.New("workspace: the checkout is on no branch to catch up")
	}

	auth, err := v.OriginAuth(ctx, v.Remote())
	if err != nil {
		return workspace.CatchUpResult{}, err
	}
	fetchRef := request.Target
	if fetchRef == "" {
		fetchRef = "HEAD"
	}
	fetch := append(append([]string{}, auth.Args...), "fetch", "origin", fetchRef)
	if _, err := v.Host.Run(ctx, checkout.Dir, auth.Env, fetch...); err != nil {
		return workspace.CatchUpResult{}, err
	}
	target, err := runTrim(ctx, v.Host, checkout.Dir, "rev-parse", "FETCH_HEAD")
	if err != nil {
		return workspace.CatchUpResult{}, err
	}

	base, err := runTrim(ctx, v.Host, checkout.Dir, "merge-base", "HEAD", target)
	if err != nil {
		return workspace.CatchUpResult{}, err
	}
	if base == target {
		return workspace.CatchUpResult{UpToDate: true}, nil
	}

	ours, err := changedFiles(ctx, v.Host, checkout.Dir, base, "HEAD")
	if err != nil {
		return workspace.CatchUpResult{}, err
	}
	theirs, err := changedFiles(ctx, v.Host, checkout.Dir, base, target)
	if err != nil {
		return workspace.CatchUpResult{}, err
	}
	overlap := overlapping(ours, theirs)

	if len(overlap) > 0 {
		// The identity travels with the staged merge too: git refuses to
		// begin a merge it could not commit, and the checkout carries no
		// identity of its own (the mill's is the signer's, per invocation).
		stage := append(checkout.Signer.GitConfig(), "merge", "--no-commit", "--no-ff", target)
		if _, err := v.Host.Run(ctx, checkout.Dir, nil, stage...); err != nil {
			if !mergeInProgress(ctx, v.Host, checkout.Dir) {
				return workspace.CatchUpResult{}, err
			}
		}
		return workspace.CatchUpResult{Overlap: overlap}, nil
	}

	merge := append(checkout.Signer.GitConfig(), "merge", "--no-ff", "--no-edit", target)
	if _, err := v.Host.Run(ctx, checkout.Dir, nil, merge...); err != nil {
		return workspace.CatchUpResult{}, err
	}
	if err := (forgeVerbs{v: v}).signed(ctx, checkout); err != nil {
		return workspace.CatchUpResult{}, err
	}
	sha, err := forge.Bind(v.Forge, forge.Step{}).Push(ctx, forge.PushRequest{
		Git:    v.Runner(),
		Branch: checkout.Branch,
	})
	if err != nil {
		return workspace.CatchUpResult{}, err
	}
	return workspace.CatchUpResult{Merged: true, SHA: sha}, nil
}

// runTrim runs a git command and trims the trailing newline git leaves on
// output meant to be read back as a single token, like a SHA.
func runTrim(ctx context.Context, host Host, dir string, args ...string) (string, error) {
	out, err := host.Run(ctx, dir, nil, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// changedFiles is the files ref changed since base.
func changedFiles(ctx context.Context, host Host, dir, base, ref string) ([]string, error) {
	out, err := host.Run(ctx, dir, nil, "diff", "--name-only", base, ref)
	if err != nil {
		return nil, err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// mergeInProgress is whether the checkout has a merge staged: `git merge
// --no-commit` (or one left conflicted) sets MERGE_HEAD until a commit
// finishes or aborts it.
func mergeInProgress(ctx context.Context, host Host, dir string) bool {
	_, err := host.Run(ctx, dir, nil, "rev-parse", "--verify", "-q", "MERGE_HEAD")
	return err == nil
}

// overlapping is the files present in both lists, sorted for a stable finding.
func overlapping(ours, theirs []string) []string {
	theirSet := make(map[string]bool, len(theirs))
	for _, f := range theirs {
		theirSet[f] = true
	}
	var overlap []string
	for _, f := range ours {
		if theirSet[f] {
			overlap = append(overlap, f)
		}
	}
	sort.Strings(overlap)
	return overlap
}

// Remote is the repository the run pushes to, for the record: the forge's
// clone URL, which carries no credential. Empty for a forge that names none.
func (v Verbs) Remote() string {
	type repoer interface{ Repo() forge.Repo }
	if named, ok := v.Forge.(repoer); ok {
		return named.Repo().CloneURL
	}
	return ""
}

// SameRepo reports whether two remotes name the same repository, allowing
// for the `.git` suffix and the trailing slash git treats as noise.
func SameRepo(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	trim := func(url string) string {
		return strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(url, "/"), ".git"), "/")
	}
	return trim(a) == trim(b)
}

// client is the forge bound to the live step, or ErrNoForge.
func (v Verbs) client(token workspace.Token) (forge.Forge, error) {
	if v.Forge == nil {
		return nil, workspace.ErrNoForge
	}
	var capabilities []string
	for _, capability := range token.Capabilities {
		capabilities = append(capabilities, string(capability))
	}
	return forge.Bind(v.Forge, forge.Step{
		TokenID:      token.ID,
		TokenHash:    token.Hash,
		Capabilities: capabilities,
	}), nil
}

// runner is [forge.Git] over the checkout.
type runner struct{ v Verbs }

func (r runner) Run(ctx context.Context, env map[string]string, args ...string) (string, error) {
	checkout, err := r.v.Checkout()
	if err != nil {
		return "", err
	}
	return r.v.Host.Run(ctx, checkout.Dir, env, args...)
}

// gitVerbs implements workspace.Git against the checkout.
type gitVerbs struct{ v Verbs }

// Status reports the branch and every path git sees as changed.
func (g gitVerbs) Status(ctx context.Context) (workspace.Status, error) {
	checkout, err := g.v.Checkout()
	if err != nil {
		return workspace.Status{}, err
	}
	out, err := g.v.Host.Run(ctx, checkout.Dir, nil, "status", "--porcelain=v1")
	if err != nil {
		return workspace.Status{}, err
	}
	status := workspace.Status{Branch: checkout.Branch, Clean: true}
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
	checkout, err := g.v.Checkout()
	if err != nil {
		return "", err
	}
	args := []string{"diff"}
	if request.Staged {
		args = append(args, "--staged")
	}
	args = append(args, pathspec(request.Paths)...)
	return g.v.Host.Run(ctx, checkout.Dir, nil, args...)
}

// logFormat separates a commit's fields with the unit separator, which no
// field of a commit can contain.
const logFormat = "--format=%H%x1f%an <%ae>%x1f%aI%x1f%s"

// Log reads the branch's history, newest first.
func (g gitVerbs) Log(ctx context.Context, request workspace.LogRequest) ([]workspace.CommitInfo, error) {
	checkout, err := g.v.Checkout()
	if err != nil {
		return nil, err
	}
	limit := request.Limit
	if limit <= 0 {
		limit = 20
	}
	args := append([]string{"log", "-n", strconv.Itoa(limit), logFormat}, pathspec(request.Paths)...)
	out, err := g.v.Host.Run(ctx, checkout.Dir, nil, args...)
	if err != nil {
		return nil, err
	}
	var commits []workspace.CommitInfo
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if info, ok := parseCommit(line); ok {
			commits = append(commits, info)
		}
	}
	return commits, nil
}

// parseCommit reads one line of logFormat.
func parseCommit(line string) (workspace.CommitInfo, bool) {
	fields := strings.Split(line, "\x1f")
	if len(fields) != 4 {
		return workspace.CommitInfo{}, false
	}
	return workspace.CommitInfo{SHA: fields[0], Author: fields[1], Date: fields[2], Subject: fields[3]}, true
}

// Commit stages and commits on the run's branch, signed with the mill's key.
// A checkout that has wandered off the run's branch is refused rather than
// committed to: the branch is the run's whole claim on the repository.
func (g gitVerbs) Commit(ctx context.Context, request workspace.CommitRequest) (workspace.CommitInfo, error) {
	checkout, err := g.v.Checkout()
	if err != nil {
		return workspace.CommitInfo{}, err
	}
	if request.Message == "" {
		return workspace.CommitInfo{}, errors.New("workspace: a commit needs a message")
	}
	current, err := g.v.Host.BranchOf(ctx, checkout.Dir)
	if err != nil {
		return workspace.CommitInfo{}, err
	}
	if current != checkout.Branch {
		return workspace.CommitInfo{}, fmt.Errorf("workspace: checkout is on %q, not the run's branch %q", current, checkout.Branch)
	}

	add := append([]string{"add", "--all", "--"}, request.Paths...)
	if len(request.Paths) == 0 {
		add = []string{"add", "--all", "."}
	}
	if _, err := g.v.Host.Run(ctx, checkout.Dir, nil, add...); err != nil {
		return workspace.CommitInfo{}, err
	}

	commit := append(checkout.Signer.GitConfig(), "commit", "--message", request.Message)
	if request.AllowEmpty {
		commit = append(commit, "--allow-empty")
	}
	// A pathspec on `commit` is a partial commit, which git refuses outright
	// while a merge is staged (CatchUp leaves one, conflicted or not, for
	// this commit to finish) — everything relevant is already staged by the
	// `add` above, so the commit is left unrestricted instead.
	if len(request.Paths) > 0 && !mergeInProgress(ctx, g.v.Host, checkout.Dir) {
		commit = append(commit, pathspec(request.Paths)...)
	}
	if _, err := g.v.Host.Run(ctx, checkout.Dir, nil, commit...); err != nil {
		return workspace.CommitInfo{}, err
	}

	out, err := g.v.Host.Run(ctx, checkout.Dir, nil, "log", "-n", "1", logFormat)
	if err != nil {
		return workspace.CommitInfo{}, err
	}
	info, ok := parseCommit(strings.TrimSpace(out))
	if !ok {
		return workspace.CommitInfo{}, fmt.Errorf("workspace: unreadable commit record %q", out)
	}
	return info, nil
}

// pathspec renders paths as git's `-- <path>…`, or nothing.
func pathspec(paths []string) []string {
	if len(paths) == 0 {
		return nil
	}
	return append([]string{"--"}, paths...)
}

// ErrUnsigned is a push refused because a commit it would publish is not
// signed by the mill's key. A session's own `git commit` inside a machine
// makes one: the key never enters the machine, so the commit verb on the
// host is what signs (docs/sandbox.md §Verbs). The error names the commit.
var ErrUnsigned = errors.New("workspace: refusing to push a commit the mill did not sign")

// forgeVerbs implements workspace.ForgeVerbs against the checkout.
type forgeVerbs struct{ v Verbs }

// Push puts the run's branch on the forge. The branch is the workspace's,
// not the request's: a session may push, but only what the run is working
// on — and only commits the mill signed (ErrUnsigned).
func (f forgeVerbs) Push(ctx context.Context, request workspace.PushRequest) (workspace.PushResult, error) {
	checkout, err := f.v.Checkout()
	if err != nil {
		return workspace.PushResult{}, err
	}
	if checkout.Branch == "" {
		return workspace.PushResult{}, errors.New("workspace: the checkout is on no branch to push")
	}
	client, err := f.v.client(checkout.Token)
	if err != nil {
		return workspace.PushResult{}, err
	}
	if err := f.signed(ctx, checkout); err != nil {
		return workspace.PushResult{}, err
	}
	sha, err := client.Push(ctx, forge.PushRequest{
		Git:    f.v.Runner(),
		Branch: checkout.Branch,
		Force:  request.Force,
	})
	if err != nil {
		return workspace.PushResult{}, err
	}
	return workspace.PushResult{Branch: checkout.Branch, SHA: sha, Remote: f.v.Remote()}, nil
}

// signed holds every commit the push would publish — reachable from HEAD
// and from no remote-tracking ref — to a good signature under the mill's
// allowed_signers. `%G?` is git's verdict per commit: `G` good, `N` none,
// anything else a signature the key does not vouch for. A workspace with
// no signer holds nothing.
func (f forgeVerbs) signed(ctx context.Context, checkout Checkout) error {
	if checkout.Signer == nil {
		return nil
	}
	args := append(checkout.Signer.GitConfig(), "log", "--format=%H%x1f%G?", "HEAD", "--not", "--remotes")
	out, err := f.v.Host.Run(ctx, checkout.Dir, nil, args...)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		sha, verdict, found := strings.Cut(line, "\x1f")
		if !found || verdict == "G" {
			continue
		}
		reason := "carries a signature the mill's key does not vouch for"
		if verdict == "N" {
			reason = "is unsigned"
		}
		return fmt.Errorf("%w: %s on %s %s; commit through the workspace's commit verb, which signs on the host",
			ErrUnsigned, sha, checkout.Branch, reason)
	}
	return nil
}

// PR is the run branch's pull request, opened if the forge has none. The
// checkout's head travels with it: a session that has just pushed asks for
// the pull request in the next breath, and the forge is not always finished
// with the push (mill#46).
func (f forgeVerbs) PR(ctx context.Context, request workspace.PRRequest) (forge.PullRequest, error) {
	checkout, err := f.v.Checkout()
	if err != nil {
		return forge.PullRequest{}, err
	}
	if checkout.Branch == "" {
		return forge.PullRequest{}, errors.New("workspace: the checkout is on no branch to open a pull request for")
	}
	client, err := f.v.client(checkout.Token)
	if err != nil {
		return forge.PullRequest{}, err
	}
	head, err := f.v.Host.Run(ctx, checkout.Dir, nil, "rev-parse", "HEAD")
	if err != nil {
		return forge.PullRequest{}, err
	}
	return client.OpenPR(ctx, forge.OpenPRRequest{
		Branch:      checkout.Branch,
		Head:        strings.TrimSpace(head),
		Target:      request.Target,
		Title:       request.Title,
		Description: request.Description,
	})
}

// Checks is what the forge says about a commit, the run branch's head by
// default.
func (f forgeVerbs) Checks(ctx context.Context, request workspace.ChecksRequest) (workspace.ChecksResult, error) {
	checkout, err := f.v.Checkout()
	if err != nil {
		return workspace.ChecksResult{}, err
	}
	client, err := f.v.client(checkout.Token)
	if err != nil {
		return workspace.ChecksResult{}, err
	}
	sha := strings.TrimSpace(request.SHA)
	if sha == "" {
		head, err := f.v.Host.Run(ctx, checkout.Dir, nil, "rev-parse", "HEAD")
		if err != nil {
			return workspace.ChecksResult{}, err
		}
		sha = strings.TrimSpace(head)
	}
	status, err := client.Checks(ctx, sha)
	if err != nil {
		return workspace.ChecksResult{}, err
	}
	return workspace.ChecksResult{SHA: sha, Status: status}, nil
}

var (
	_ workspace.Git        = gitVerbs{}
	_ workspace.ForgeVerbs = forgeVerbs{}
	_ forge.Git            = runner{}
)
