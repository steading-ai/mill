// Package forge is where delivery ends: the branch pushed, the pull request
// opened, the checks read, the merge made, and the merge commit read back off
// the default branch. Those five facts are the evidence a `push`, a `pr`, a
// `ci` and a `merge` step close on (docs/domain-model.md §Evidence).
//
// Three rules shape the interface.
//
// The forge never touches a filesystem. A push is a git command, and every
// process the mill spawns goes through the workspace's chokepoint, so [Forge]
// is handed a [Git] to run it with and supplies only the remote and the
// credential. The credential reaches git through its environment and a
// credential helper, never through the remote URL, so a clone's `origin` is
// the plain repository URL and `git remote -v` in a session's checkout shows
// nothing worth stealing.
//
// A credential is minted per request and dies with the step. The forge holds
// a [Minter], not a token: every call asks for one, so the credential's life
// is the call's and a step token that has been revoked cannot produce one
// (docs/architecture.md §"Identity and credentials").
//
// Auto-merge is the default, and a forge that refuses is believed. A merge
// GitLab declines — a protected branch, a missing approval — is a
// [MergeRefusedError] naming the forge's own reason and is not retried:
// repository protections are the repository's job (docs/core-principles.md
// §1). Only the transport's own failures and a head that moved under the
// merge are worth another attempt, which is what [Retryable] reports.
//
// A merge names the commit it merges. [Forge.Merge] is given the pull request
// the caller read, and the head on it is the head that merges: a forge that
// finds the branch somewhere else refuses with a [HeadMovedError] rather than
// merging work no step ever checked.
//
// GitLab says *merge request* where the design says PR. The names here are
// the design's; the GitLab adapter maps.
package forge

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Kind is which forge implementation serves a repository.
type Kind string

const (
	// KindGitLab is a self-hosted GitLab, the forge O0 delivers to.
	KindGitLab Kind = "gitlab"
	// KindGitHub is github.com: one host, so nothing about it is
	// configured but the credential (internal/forge/github).
	KindGitHub Kind = "github"
)

// Kinds is every forge the mill has an adapter for, in the order a message
// lists them.
var Kinds = []Kind{KindGitHub, KindGitLab}

// ParseKind reads a configured forge kind. Empty is not an error: it means
// the repository's host decides. Anything else must name an adapter, and
// what does not is an *[UnknownKindError] naming what was written.
func ParseKind(raw string) (Kind, error) {
	kind := Kind(strings.ToLower(strings.TrimSpace(raw)))
	if kind == "" {
		return "", nil
	}
	for _, known := range Kinds {
		if kind == known {
			return kind, nil
		}
	}
	return "", &UnknownKindError{Raw: raw}
}

// Status is what the checks on a commit amount to, flattened to the three
// answers a `ci` step can act on. Anything a forge reports that has not
// settled is Pending; anything that settled badly is Failed.
type Status string

const (
	// Pending is at least one check still running, or not yet started.
	Pending Status = "pending"
	// Success is every check green — including a project with no pipeline
	// configured at all, which has nothing to be red about.
	Success Status = "success"
	// Failed is at least one check red, cancelled, or otherwise not green.
	Failed Status = "failed"
)

// Project is a dagger project as the forge needs it: a name for errors, and
// the repository it delivers to.
//
// VCSURL is dagger's own `vcs_url` on the project (dagger's
// docs/domain-model.md §Project) and nothing else feeds it: the mill carries
// no repository of its own, so two orchestrators and a person read the same
// answer from the same place. A project with none is a [NoVCSURLError],
// which names the `dg` command that sets it.
type Project struct {
	// Slug is the dagger project, named in every error this package raises.
	Slug string
	// VCSURL is the repository the project delivers to.
	VCSURL string
	// Kind is the forge the configuration names, when it names one. Empty
	// means the repository's host decides, which is right for the hosts
	// the mill can recognise; a GitLab on a host that says nothing about
	// itself is what the setting is for.
	Kind Kind
}

// Repo is one repository on one forge: enough to reach its API and to push to
// it, and nothing about the working copy.
type Repo struct {
	// Kind is which forge serves it.
	Kind Kind
	// BaseURL is the forge's root, such as `https://gitlab.example.test`.
	BaseURL string
	// Path is the repository's path on the forge, such as `group/thing`.
	Path string
	// CloneURL is what git is given as a remote. It carries no credential;
	// see the package doc.
	CloneURL string
}

// Git runs one git command inside a run's checkout. The workspace implements
// it, because the workspace is the only place in the mill that may spawn a
// process; the forge supplies the arguments and the environment the
// credential travels in.
type Git interface {
	// Run executes git with args, adding env to the environment the
	// workspace already sets, and returns its standard output.
	Run(ctx context.Context, env map[string]string, args ...string) (string, error)
}

// PushRequest is one branch, pushed to the project's repository.
type PushRequest struct {
	// Git runs the command. Required.
	Git Git
	// Branch is the run's branch, pushed to the same name on the forge.
	Branch string
	// Force overwrites the remote branch, which a rebased run needs.
	Force bool
}

// OpenPRRequest opens a pull request for the run's branch.
type OpenPRRequest struct {
	// Branch is the source branch, already pushed.
	Branch string
	// Head is the commit the caller pushed the branch to. A forge is not
	// asked for a pull request until it shows the branch at this commit,
	// because a push the forge has not caught up with yet is a pull
	// request it refuses for a source branch that "does not exist"
	// (mill#46). Empty means the caller has no head to wait for and the
	// pull request is asked for straight away.
	Head string
	// Target is the branch to merge into. Empty means the repository's
	// default branch.
	Target string
	// Title is the pull request's title.
	Title string
	// Description is its body.
	Description string
}

// PullRequest is the forge's pull request — GitLab's merge request — as the
// mill records it. URL is the `url` evidence a PR step closes on.
type PullRequest struct {
	// Number is the forge's own number for it: GitLab's `iid`.
	Number int `json:"number"`
	// URL is the web URL, and the evidence.
	URL string `json:"url"`
	// Branch is the source branch.
	Branch string `json:"branch"`
	// Target is the branch it merges into.
	Target string `json:"target"`
	// HeadSHA is the source branch's head as the forge last saw it.
	HeadSHA string `json:"head_sha"`
	// State is the forge's state, mapped: `open`, `merged`, `closed`.
	State string `json:"state"`
	// Squash is whether the project's settings say this merges squashed.
	Squash bool `json:"squash"`
	// MergeCommit is the commit on the default branch, once it exists.
	MergeCommit string `json:"merge_commit,omitempty"`
	// Detail is the forge's own word for why this cannot merge yet, kept
	// verbatim so a refusal names the forge's reason rather than the
	// mill's guess at it.
	Detail string `json:"detail,omitempty"`
}

// Open reports whether the pull request is still open.
func (p PullRequest) Open() bool { return p.State == "open" }

// Merged reports whether it has landed.
func (p PullRequest) Merged() bool { return p.State == "merged" }

// Credential is a forge credential as one request holds it. It is minted
// against a live step token, used for that request, and dropped; nothing
// writes it to disk and nothing puts it in a URL.
type Credential struct {
	// Username is the basic-auth user git is given. GitLab wants a
	// placeholder beside a token.
	Username string
	// Token is the secret.
	Token string
	// ExpiresAt is when the forge stops honouring it, zero when unbounded.
	ExpiresAt time.Time
}

// GitAuth is what one git command needs to authenticate to the repository:
// arguments that install a credential helper, and the environment that helper
// reads. It is the shape a credential travels in wherever git talks to the
// forge — the clone that starts a run, a fetch, the push at the end of one —
// so the credential appears in neither the remote URL nor argv, and no
// helper has to exist on the host.
//
// Args go before git's subcommand; Env is added to the environment the
// workspace already sets for the child, and nothing else.
type GitAuth struct {
	// Args are git's own options, such as a `-c credential.helper=`.
	Args []string
	// Env is the environment the helper reads. Every value in it is a
	// secret: the workspace registers them with its redactor.
	Env map[string]string
}

// Environment variable names the credential helper reads. They exist only
// in the git child's environment, for the length of one command.
const (
	EnvUsername = "MILL_FORGE_USERNAME"
	EnvToken    = "MILL_FORGE_TOKEN" //nolint:gosec // the name of a variable, not a credential
)

// HelperAuth is a credential as git reads it: a credential helper, written
// inline so nothing has to be installed on the host, that answers git's own
// prompt from the environment. The empty helper first clears any inherited
// one, so the mill's answer is the only one; the helper itself contains no
// secret, which is why it is safe in argv. Every adapter hands git its
// credential this way, so a token appears in neither a remote URL nor a
// process listing whichever forge it is for.
func HelperAuth(credential Credential) GitAuth {
	const helper = `!f() { test "$1" = get && printf 'username=%s\npassword=%s\n' "$` + EnvUsername + `" "$` + EnvToken + `"; }; f`
	return GitAuth{
		Args: []string{"-c", "credential.helper=", "-c", "credential.helper=" + helper},
		Env:  map[string]string{EnvUsername: credential.Username, EnvToken: credential.Token},
	}
}

// Step is the step a credential is minted for: the bottom rung of the
// credential ladder, identified by what the store keeps rather than by the
// bearer, so a minter never needs the bearer itself.
type Step struct {
	// TokenID is the step token's ULID.
	TokenID string
	// TokenHash is the hex SHA-256 the store holds.
	TokenHash string
	// Capabilities is what the step token grants, for a minter that scopes
	// the credential by them.
	Capabilities []string
}

// Minter produces a forge credential for a step. The forge holds one of these
// rather than a token, so every call mints afresh and a revoked step token
// yields nothing.
type Minter interface {
	Mint(ctx context.Context, step Step) (Credential, error)
}

// Static is the minter O0 has: one credential the mill holds for the
// repository, handed out per request. It is the bottom of the ladder until a
// secrets service can issue a GitLab token scoped to one step and expiring
// with it — the credential is per-request in shape, so replacing this changes
// nothing above it (docs/architecture.md §"Identity and credentials").
func Static(username, token string) Minter {
	return MinterFunc(func(context.Context, Step) (Credential, error) {
		if token == "" {
			return Credential{}, errors.New("forge: the mill holds no credential for this repository")
		}
		return Credential{Username: username, Token: token}, nil
	})
}

// MinterFunc adapts a function to [Minter].
type MinterFunc func(ctx context.Context, step Step) (Credential, error)

// Mint calls f.
func (f MinterFunc) Mint(ctx context.Context, step Step) (Credential, error) { return f(ctx, step) }

// Forge is what delivery needs from a forge, and nothing else. None of the
// methods holds state between calls, so a worker that died mid-run can ask
// again and get the same answers. Every one of them is a single round trip
// but [Forge.OpenPR], which waits for the forge to show the branch it is
// asked to open a pull request for.
type Forge interface {
	// Authenticate mints a credential for one git command against the
	// repository and returns the arguments and environment that hand it to
	// git. The workspace asks for it before a clone or a fetch of an
	// origin nothing may read anonymously; [Forge.Push] asks for it too,
	// so the three reach the forge the same way.
	Authenticate(ctx context.Context) (GitAuth, error)
	// Push puts the run's branch on the forge, with a credential minted for
	// this call and never written to disk. It returns the SHA that landed.
	Push(ctx context.Context, request PushRequest) (string, error)
	// OpenPR opens a pull request for the branch, or returns the one that
	// is already open for it. Opening twice is the same as opening once.
	// A request naming a head waits for the forge to show the branch at
	// it before opening anything, and gives up with a
	// *[BranchMissingError] when it never appears.
	OpenPR(ctx context.Context, request OpenPRRequest) (PullRequest, error)
	// PR is the pull request for a branch, [ErrNoPullRequest] when there is
	// none. A merged one is still found, because the merge commit is read
	// through it after the branch is gone.
	PR(ctx context.Context, branch string) (PullRequest, error)
	// Checks is what the forge's checks say about a commit.
	Checks(ctx context.Context, sha string) (Status, error)
	// Merge merges a pull request, squashing when the project's settings
	// say so. The merge names pr's head, so what merges is the commit the
	// caller read: a head that moved since yields a *[HeadMovedError], and
	// a forge that declines for its own reasons a *[MergeRefusedError].
	Merge(ctx context.Context, pr PullRequest) (PullRequest, error)
	// MergeCommit is the SHA on the default branch that the pull request
	// became: the squash commit when it was squashed, the merge commit
	// otherwise. [ErrNotMerged] until it has landed.
	MergeCommit(ctx context.Context, pr PullRequest) (string, error)
	// ClosePR comments on a pull request and closes it without merging.
	// `mill restart` uses it, and only it: a run that has gone sideways
	// leaves a pull request nobody should merge, and the comment is what
	// tells the next reader why it was abandoned. Closing one that is
	// already closed or merged is not an error.
	ClosePR(ctx context.Context, pr PullRequest, comment string) error
	// DeleteBranch removes a branch from the forge. A branch that is not
	// there is not an error: the point is that it is gone.
	DeleteBranch(ctx context.Context, branch string) error
}

// Account is who a forge says a credential belongs to, for `mill doctor`.
type Account struct {
	// Login is the account's name on the forge.
	Login string
}

// Prober is a forge that can say whether a credential will serve a
// delivery before a run spends work finding out. `mill doctor` asks it: the
// answer names the account the credential authenticates as, and a failure
// names what the credential lacks — an expiry, a permission the repository
// needs — rather than the round trip that surfaced it.
type Prober interface {
	Probe(ctx context.Context) (Account, error)
}

// ContentReader is a forge that can read one file off a repository's
// default branch without cloning it. `mill doctor`'s signing probe asks it
// for `allowed_signers` when the host has no checkout to look in yet — a
// fresh host, before its first run, still has a key and a repository, and
// this is the cheap way to ask the repository rather than a run's own git
// clone. [ErrNoSuchFile] is what a repository with no such file answers.
type ContentReader interface {
	ReadFile(ctx context.Context, path string) (string, error)
}

// Scratch is a forge's scratch namespace: the group a suite makes its
// throwaway projects in, named `mill-acceptance-<scenario>-<nonce>`, and
// the verbs the worker's reconciliation sweep needs to take them out again
// by name (docs/sandbox.md §"External state"). It is separate from [Forge],
// which serves one repository; this serves the namespace those
// repositories are made in, and only that namespace.
type Scratch interface {
	// Namespace is the group's path, for the report.
	Namespace() string
	// ListProjects is every project in the namespace, matched or not; the
	// sweep decides which are stale.
	ListProjects(ctx context.Context) ([]ScratchProject, error)
	// DeleteProject removes one. A project the forge no longer has is the
	// state asked for, not an error.
	DeleteProject(ctx context.Context, project ScratchProject) error
}

// ScratchProject is one project in a scratch namespace.
type ScratchProject struct {
	// ID is the forge's own identifier, which a delete is addressed by.
	ID string
	// Name is the project's name, the last element of its path: what the
	// sweep's pattern is matched against.
	Name string
	// Path is the full path, `<namespace>/<name>`.
	Path string
	// CreatedAt is when the forge made it.
	CreatedAt time.Time
}

// The failures a caller distinguishes.
var (
	// ErrNoPullRequest is a branch with no pull request on the forge.
	ErrNoPullRequest = errors.New("forge: no pull request for the branch")
	// ErrNoSuchFile is a path a ContentReader's repository does not have.
	ErrNoSuchFile = errors.New("forge: no such file in the repository")
	// ErrNotMerged is a merge commit asked for before the merge.
	ErrNotMerged = errors.New("forge: the pull request has not merged")
)

// NoVCSURLError is a dagger project the mill cannot find a repository for. It
// names the project, because the fix is on the project and a run that cannot
// say which one is a bug report nobody can act on.
type NoVCSURLError struct {
	// Project is the dagger project's slug.
	Project string
}

func (e *NoVCSURLError) Error() string {
	return fmt.Sprintf("forge: project %s has no repository: set it with `dg project update %s --vcs-url <url>`",
		e.Project, e.Project)
}

// UnknownKindError is a configured forge that names no adapter. It carries
// what was written, verbatim, because the fix is in the configuration and
// the message has to point at the value rather than at the concept.
type UnknownKindError struct {
	// Raw is the value as configured.
	Raw string
}

func (e *UnknownKindError) Error() string {
	names := make([]string, len(Kinds))
	for i, kind := range Kinds {
		names[i] = string(kind)
	}
	return fmt.Sprintf("forge: %q is not a forge this mill has an adapter for: %s",
		e.Raw, strings.Join(names, ", "))
}

// UnsupportedError is a repository on a forge the mill has no adapter for.
type UnsupportedError struct {
	// Kind is the forge kind asked for.
	Kind Kind
	// Project is the dagger project that named it.
	Project string
}

func (e *UnsupportedError) Error() string {
	return fmt.Sprintf("forge: project %s wants the %s forge, which this mill has no adapter for", e.Project, e.Kind)
}

// MergeRefusedError is the forge declining to merge: a protected branch, a
// required approval, a check that has to pass first. The mill does not retry
// it and does not argue with it — the repository's protections are the
// repository's job — so the error carries the forge's own words for a human
// and for the run's record.
type MergeRefusedError struct {
	// PR is the pull request that would not merge.
	PR int
	// Status is the HTTP status the forge answered with.
	Status int
	// Reason is the forge's message, verbatim.
	Reason string
	// Detail is the forge's machine-readable reason when it gave one:
	// GitLab's `detailed_merge_status`, such as `not_approved`.
	Detail string
}

// BranchMissingError is the forge not showing the run's branch at the head
// the mill pushed, after waiting for it to catch up. A push through the
// workspace and the merge request that follows it are two requests to two
// different parts of a forge, and GitLab answers the second one
// `source_branch does not exist` while it is still indexing the first
// (mill#46). Waiting is what tells the two apart: a forge that catches up
// opens the pull request, and a branch that never appears is a push that
// never landed.
//
// The error names the branch and the head it expected, because that pair is
// what an operator reads off `mill logs <node> --step pr` and checks against
// the forge by hand.
type BranchMissingError struct {
	// Branch is the branch the pull request would have come from.
	Branch string
	// Head is the commit the mill pushed it to.
	Head string
	// Found is the commit the forge does show the branch at, empty when it
	// has no such branch at all.
	Found string
	// Waited is how long the mill gave the forge to catch up.
	Waited time.Duration
	// Reads is how many times it asked in that time.
	Reads int
}

func (e *BranchMissingError) Error() string {
	found := "the forge has no branch by that name"
	if e.Found != "" {
		found = "the forge shows it at " + e.Found
	}
	return fmt.Sprintf("forge: branch %s is not on the forge at %s after %s and %d reads: %s: the push never landed",
		e.Branch, e.Head, e.Waited, e.Reads, found)
}

// HeadMovedError is the forge declining a merge because the head the mill
// named is no longer the branch's. The mill merges a commit it has read and
// checked, never whatever the branch happens to be at (docs/domain-model.md
// §Evidence), so a head that moved under it is not an error to hide: the
// work the run pushed is still there, the facts about it are simply one
// commit stale. Reading the pull request again and merging the new head is
// what the next attempt does, which is why this is [Retryable].
type HeadMovedError struct {
	// PR is the pull request that would not merge.
	PR int
	// SHA is the head the mill merged against.
	SHA string
	// Reason is the forge's message, verbatim.
	Reason string
}

func (e *HeadMovedError) Error() string {
	reason := e.Reason
	if reason == "" {
		reason = "the head moved"
	}
	return fmt.Sprintf("forge: !%d is no longer at %s: %s", e.PR, e.SHA, reason)
}

func (e *MergeRefusedError) Error() string {
	reason := e.Reason
	if e.Detail != "" {
		if reason == "" {
			reason = e.Detail
		} else {
			reason = fmt.Sprintf("%s (%s)", reason, e.Detail)
		}
	}
	if reason == "" {
		reason = "no reason given"
	}
	return fmt.Sprintf("forge: the forge refused to merge !%d: %s", e.PR, reason)
}

// Retryable reports whether trying err again could plausibly succeed. A
// refusal to merge never is: nothing the mill can do changes the forge's
// answer, and a retry loop against a protected branch is a way to look busy.
// A [HeadMovedError] is the one merge failure that is: the next attempt reads
// the pull request again and merges the head it finds.
func Retryable(err error) bool {
	if err == nil {
		return false
	}
	var refused *MergeRefusedError
	if errors.As(err, &refused) {
		return false
	}
	var missing *NoVCSURLError
	if errors.As(err, &missing) {
		return false
	}
	var unsupported *UnsupportedError
	if errors.As(err, &unsupported) {
		return false
	}
	return !errors.Is(err, ErrNoPullRequest) && !errors.Is(err, ErrNotMerged)
}

// Factory builds a forge client for a repository, minting its credentials
// through minter.
type Factory func(repo Repo, minter Minter) (Forge, error)

// Registry maps a forge kind to its implementation. Adding a forge is
// registering a factory; nothing else in the mill names an implementation
// (docs/roadmap.md §"Adding a forge is configuration").
type Registry struct {
	mu        sync.RWMutex
	factories map[Kind]Factory
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{factories: map[Kind]Factory{}} }

// Register installs the factory for a kind, replacing any before it.
func (r *Registry) Register(kind Kind, factory Factory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.factories == nil {
		r.factories = map[Kind]Factory{}
	}
	r.factories[kind] = factory
}

// Kinds is every kind registered.
func (r *Registry) Kinds() []Kind {
	r.mu.RLock()
	defer r.mu.RUnlock()
	kinds := make([]Kind, 0, len(r.factories))
	for kind := range r.factories {
		kinds = append(kinds, kind)
	}
	return kinds
}

// For resolves a dagger project to a forge client. This is what a run start
// calls, and where a project with no repository fails: a *[NoVCSURLError]
// naming the project, before a workspace is created or a session is spent.
func (r *Registry) For(project Project, minter Minter) (Forge, error) {
	repo, err := RepoFor(project)
	if err != nil {
		return nil, err
	}
	r.mu.RLock()
	factory := r.factories[repo.Kind]
	r.mu.RUnlock()
	if factory == nil {
		return nil, &UnsupportedError{Kind: repo.Kind, Project: project.Slug}
	}
	return factory(repo, minter)
}

// RepoFor resolves the repository a dagger project delivers to, which is the
// URL the run was given and nothing else.
func RepoFor(project Project) (Repo, error) {
	raw := strings.TrimSpace(project.VCSURL)
	if raw == "" {
		return Repo{}, &NoVCSURLError{Project: project.Slug}
	}
	repo, err := ParseRepo(raw)
	if err != nil {
		return Repo{}, fmt.Errorf("forge: project %s: %w", project.Slug, err)
	}
	if project.Kind != "" {
		repo.Kind = project.Kind
	}
	return repo, nil
}

// ParseRepo reads a repository URL in either spelling git accepts — an HTTPS
// URL, or the scp-like `git@host:path` — into the forge's base URL, the
// repository's path, and the URL git is given as a remote.
func ParseRepo(raw string) (Repo, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Repo{}, errors.New("forge: empty repository URL")
	}
	host, path, err := splitRepo(raw)
	if err != nil {
		return Repo{}, err
	}
	path = strings.Trim(strings.TrimSuffix(path, ".git"), "/")
	if host == "" || path == "" {
		return Repo{}, fmt.Errorf("forge: %q names no repository", raw)
	}
	base := "https://" + host
	return Repo{
		Kind:     kindOfHost(host),
		BaseURL:  base,
		Path:     path,
		CloneURL: base + "/" + path + ".git",
	}, nil
}

// splitRepo separates the host from the repository path, whichever spelling
// the URL uses. An scp-like address has no scheme and a colon before the
// first slash, which is the one case url.Parse reads wrongly.
func splitRepo(raw string) (host, path string, err error) {
	if !strings.Contains(raw, "://") {
		address, rest, found := strings.Cut(raw, ":")
		if !found {
			return "", "", fmt.Errorf("forge: %q is not a repository URL", raw)
		}
		if _, user, hasUser := strings.Cut(address, "@"); hasUser {
			address = user
		}
		return address, rest, nil
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("forge: %q is not a repository URL: %w", raw, err)
	}
	return parsed.Host, parsed.Path, nil
}

// kindOfHost guesses the forge from its host, which is right for the two
// forges that exist. A self-hosted GitLab has no name the mill could
// recognise, so GitLab is the default rather than the exception.
func kindOfHost(host string) Kind {
	name := host
	if index := strings.IndexByte(name, ':'); index >= 0 {
		name = name[:index]
	}
	if name == "github.com" || strings.HasSuffix(name, ".github.com") {
		return KindGitHub
	}
	return KindGitLab
}

// StepBound is a forge client that can be bound to the step whose token its
// credentials are minted against. An implementation returns a copy, so one
// client per project serves every step without them sharing a credential.
type StepBound interface {
	ForStep(step Step) Forge
}

// Bind returns f bound to step, or f unchanged when its implementation does
// not care which step it is serving.
func Bind(f Forge, step Step) Forge {
	if bound, ok := f.(StepBound); ok {
		return bound.ForStep(step)
	}
	return f
}
