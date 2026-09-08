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
// §1). Only the transport's own failures are worth another attempt, which is
// what [Retryable] reports.
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
	// KindGitHub is github.com. Not implemented; it is named so that a
	// project pointing at one fails by name rather than by silence.
	KindGitHub Kind = "github"
)

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
// somewhere to find the repository it delivers to.
//
// dagger's project model carries no VCS URL field (dagger's
// docs/domain-model.md §Project), so O0 reads it from a `vcs_url` label and
// the field is filed against dagger (docs/roadmap.md, the deferred table).
// VCSURL is here for the day the field exists; Labels is what is read today.
type Project struct {
	// Slug is the dagger project, named in every error this package raises.
	Slug string
	// VCSURL is the repository URL when dagger carries one. Empty until it
	// does.
	VCSURL string
	// Labels are the project's labels. `vcs_url` is read when VCSURL is
	// empty, and `forge_kind` overrides the kind inferred from the host.
	Labels map[string]string
}

// Label names read off a dagger project. Both disappear when dagger's project
// model carries the fields itself.
const (
	// VCSURLLabel is where a project's repository URL is read from.
	VCSURLLabel = "vcs_url"
	// ForgeKindLabel names the forge when the host does not give it away.
	ForgeKindLabel = "forge_kind"
)

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

// Forge is what delivery needs from a forge, and nothing else. Every method
// is one round trip; none of them holds state between calls, so a worker that
// died mid-run can ask again and get the same answers.
type Forge interface {
	// Push puts the run's branch on the forge, with a credential minted for
	// this call and never written to disk. It returns the SHA that landed.
	Push(ctx context.Context, request PushRequest) (string, error)
	// OpenPR opens a pull request for the branch, or returns the one that
	// is already open for it. Opening twice is the same as opening once.
	OpenPR(ctx context.Context, request OpenPRRequest) (PullRequest, error)
	// PR is the pull request for a branch, [ErrNoPullRequest] when there is
	// none. A merged one is still found, because the merge commit is read
	// through it after the branch is gone.
	PR(ctx context.Context, branch string) (PullRequest, error)
	// Checks is what the forge's checks say about a commit.
	Checks(ctx context.Context, sha string) (Status, error)
	// Merge merges a pull request, squashing when the project's settings
	// say so. A forge that refuses yields a [MergeRefusedError].
	Merge(ctx context.Context, pr PullRequest) (PullRequest, error)
	// MergeCommit is the SHA on the default branch that the pull request
	// became: the squash commit when it was squashed, the merge commit
	// otherwise. [ErrNotMerged] until it has landed.
	MergeCommit(ctx context.Context, pr PullRequest) (string, error)
}

// The failures a caller distinguishes.
var (
	// ErrNoPullRequest is a branch with no pull request on the forge.
	ErrNoPullRequest = errors.New("forge: no pull request for the branch")
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
	return fmt.Sprintf("forge: project %s has no VCS URL: set a %s label on it", e.Project, VCSURLLabel)
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

// RepoFor resolves the repository a dagger project delivers to, from the
// project's VCS URL or, until dagger's project model carries one, from its
// `vcs_url` label.
func RepoFor(project Project) (Repo, error) {
	raw := strings.TrimSpace(project.VCSURL)
	if raw == "" {
		raw = strings.TrimSpace(project.Labels[VCSURLLabel])
	}
	if raw == "" {
		return Repo{}, &NoVCSURLError{Project: project.Slug}
	}
	repo, err := ParseRepo(raw)
	if err != nil {
		return Repo{}, fmt.Errorf("forge: project %s: %w", project.Slug, err)
	}
	if kind := strings.TrimSpace(project.Labels[ForgeKindLabel]); kind != "" {
		repo.Kind = Kind(kind)
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
// forges that exist and overridable by the `forge_kind` label for anything
// else. A self-hosted GitLab has no name the mill could recognise, so GitLab
// is the default rather than the exception.
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
