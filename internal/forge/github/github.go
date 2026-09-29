// Package github is the forge adapter for github.com, over its REST API.
//
// GitHub is one host. An organisation's plan changes which protections and
// features are switched on, not where the API is, so nothing about the host
// is configured: the API is https://api.github.com and the repository is the
// skift project's `vcs_url`. What does vary is the credential, and that is
// the person's.
//
// The mill delivers as the person. A GitHub App needs an organisation
// administrator to install it, and the people this serves are engineers who
// are not administrators of the organisations they work in; the token the
// `gh` CLI holds after `gh auth login` is the one most of them already have.
// So the credential is a token — a fine-grained personal access token, or
// the OAuth token `gh auth token` prints — and it reaches the adapter the
// way every forge credential does, through a [forge.Minter]: [Token] is that
// minter for a token, and an App would be another minter beside it, minting
// an installation token per call, with nothing in this file changing.
//
// GitHub's vocabulary is closer to the design's than GitLab's, and the
// mapping is still this package's whole job. A pull request that is closed
// with `merged_at` set is merged; a `merge_commit_sha` on a merged pull
// request is the commit that landed whichever merge method made it. A
// commit's checks are two things on GitHub — check runs and the older
// commit statuses — and a commit with neither has nothing to be red about,
// which is [forge.Success]. A merge GitHub declines answers 405 with a
// message, which is a [forge.MergeRefusedError] carrying that message; a
// merge that names a head the branch has moved past answers 409, which is
// a [forge.HeadMovedError] and the one merge failure worth another attempt.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/steading-ai/mill/internal/forge"
)

// API is the one host's API root. It is a constant rather than a setting:
// there is one GitHub, and a knob for it would invite configuration nobody
// needs and a code path nobody tests. A spec stands a stub in for it
// through [WithHTTPClient] and a transport of its own.
const API = "https://api.github.com"

// apiVersion is the REST API version every request names.
const apiVersion = "2022-11-28"

// Register installs this adapter in a registry, which is all it takes for a
// project on github.com to be deliverable.
func Register(registry *forge.Registry, options ...Option) {
	registry.Register(forge.KindGitHub, func(repo forge.Repo, minter forge.Minter) (forge.Forge, error) {
		return New(repo, minter, options...), nil
	})
}

// Token is the minter for a token the person holds: a fine-grained personal
// access token, or the OAuth token `gh auth token` prints. Both are sent
// the same way, as a bearer, and both push through the same credential
// helper, so the adapter does not ask which it was given. The username is
// the placeholder GitHub documents for a token used as a password; the
// token is what is checked.
//
// This is the whole of the token method. A GitHub App is a second minter —
// one that signs a JWT and exchanges it for an installation token per call
// — registered in its place, and nothing in the adapter changes.
func Token(token string) forge.Minter {
	return forge.MinterFunc(func(context.Context, forge.Step) (forge.Credential, error) {
		if strings.TrimSpace(token) == "" {
			return forge.Credential{}, errors.New("github: the mill holds no token for this repository")
		}
		return forge.Credential{Username: tokenUsername, Token: token}, nil
	})
}

// tokenUsername is the basic-auth username a token is pushed under. GitHub
// ignores it when the password is a token; this is the placeholder its
// documentation uses.
const tokenUsername = "x-access-token"

// Client is one repository on github.com. It holds no credential: every
// call mints one for the step it is bound to and drops it when the call
// returns.
type Client struct {
	repo   forge.Repo
	minter forge.Minter
	http   *http.Client
	step   forge.Step

	// repository is GitHub's answer about the repository — its default
	// branch, which merge methods it allows — which does not change
	// during a run.
	mu   *sync.Mutex
	info *repository

	// branchWait and branchPoll are how long a pull request waits for its
	// head branch to appear on the forge, and how often it looks.
	branchWait time.Duration
	branchPoll time.Duration

	// mergeableWait and mergeablePoll are how long a merge waits for GitHub
	// to finish computing mergeability before asking to merge, and how
	// often it looks.
	mergeableWait time.Duration
	mergeablePoll time.Duration
}

// Option configures a client.
type Option func(*Client)

// WithHTTPClient replaces the HTTP client, which a spec against a recorded
// API and a deployment behind a proxy both need.
func WithHTTPClient(client *http.Client) Option {
	return func(c *Client) {
		if client != nil {
			c.http = client
		}
	}
}

// WithBranchWait sets how long a pull request waits for GitHub to show its
// head branch, and how often it asks. Zero or less leaves the default.
func WithBranchWait(wait, poll time.Duration) Option {
	return func(c *Client) {
		if wait > 0 {
			c.branchWait = wait
		}
		if poll > 0 {
			c.branchPoll = poll
		}
	}
}

// How long a push is given to become visible, and how often GitHub is
// asked. A push that has landed is usually visible within one read; a
// branch still missing after this many seconds is a push that never landed.
const (
	defaultBranchWait = 15 * time.Second
	defaultBranchPoll = time.Second
)

// WithMergeableWait sets how long a merge waits for GitHub to finish
// computing mergeability before asking to merge, and how often it asks.
// Zero or less leaves the default.
func WithMergeableWait(wait, poll time.Duration) Option {
	return func(c *Client) {
		if wait > 0 {
			c.mergeableWait = wait
		}
		if poll > 0 {
			c.mergeablePoll = poll
		}
	}
}

// How long a merge waits for GitHub to finish computing mergeability, and
// how often it is asked. A push that has just landed leaves GitHub's
// `mergeable_state` at `unknown` for a few seconds while it recomputes;
// this wait is that recomputation's budget, not a hard requirement — a
// state still `unknown` past it is passed on to [Client.Merge]'s caller to
// classify.
const (
	defaultMergeableWait = 15 * time.Second
	defaultMergeablePoll = time.Second
)

// New returns a client for the repository, minting credentials through
// minter.
func New(repo forge.Repo, minter forge.Minter, options ...Option) *Client {
	c := &Client{
		repo:          repo,
		minter:        minter,
		http:          &http.Client{Timeout: 30 * time.Second},
		mu:            &sync.Mutex{},
		branchWait:    defaultBranchWait,
		branchPoll:    defaultBranchPoll,
		mergeableWait: defaultMergeableWait,
		mergeablePoll: defaultMergeablePoll,
	}
	for _, option := range options {
		option(c)
	}
	return c
}

// ForStep returns a copy bound to the step its credentials are minted
// against. The repository's own details are shared, because they are the
// repository's and not the step's.
func (c *Client) ForStep(step forge.Step) forge.Forge {
	bound := *c
	bound.step = step
	return &bound
}

// Repo is the repository this client serves.
func (c *Client) Repo() forge.Repo { return c.repo }

// owner is the repository's owner: the first element of its path, which is
// also what a pull request's `head` is qualified with.
func (c *Client) owner() string {
	owner, _, _ := strings.Cut(c.repo.Path, "/")
	return owner
}

// repository is what GitHub says about the repository itself.
type repository struct {
	FullName          string `json:"full_name"`
	DefaultBranch     string `json:"default_branch"`
	AllowMergeCommit  bool   `json:"allow_merge_commit"`
	AllowSquashMerge  bool   `json:"allow_squash_merge"`
	AllowRebaseMerge  bool   `json:"allow_rebase_merge"`
	DeleteBranchOnMrg bool   `json:"delete_branch_on_merge"`
}

// mergeMethod is the merge the repository's settings allow, in the order
// GitHub's own merge button offers them: a merge commit when allowed,
// otherwise a squash, otherwise a rebase. The mill takes the repository's
// setting rather than deciding for it.
func (r repository) mergeMethod() string {
	switch {
	case r.AllowMergeCommit:
		return "merge"
	case r.AllowSquashMerge:
		return "squash"
	default:
		return "rebase"
	}
}

// pullRequest is GitHub's pull request, as much of it as the mill reads.
type pullRequest struct {
	Number         int    `json:"number"`
	HTMLURL        string `json:"html_url"`
	State          string `json:"state"`
	MergedAt       string `json:"merged_at"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	MergeableState string `json:"mergeable_state"`
	Head           struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

// mapped puts a pull request in the design's vocabulary. A pull request
// that is closed with `merged_at` set is merged, and only then is its
// `merge_commit_sha` the commit that landed: GitHub fills the field on an
// open pull request too, with a test merge nobody asked for.
func (p pullRequest) mapped(squash bool) forge.PullRequest {
	state := stateOf(p.State, p.MergedAt)
	commit := ""
	if state == "merged" {
		commit = p.MergeCommitSHA
	}
	return forge.PullRequest{
		Number:      p.Number,
		URL:         p.HTMLURL,
		Branch:      p.Head.Ref,
		Target:      p.Base.Ref,
		HeadSHA:     p.Head.SHA,
		State:       state,
		Squash:      squash,
		MergeCommit: commit,
		Detail:      p.MergeableState,
	}
}

// stateOf maps GitHub's two states and one timestamp onto the three the
// mill knows.
func stateOf(state, mergedAt string) string {
	switch {
	case state == "open":
		return "open"
	case mergedAt != "":
		return "merged"
	default:
		return "closed"
	}
}

// Authenticate mints a credential for one git command and returns it as the
// arguments and environment git reads it through: the shared helper, so the
// token is in neither the remote URL nor argv (forge.HelperAuth).
func (c *Client) Authenticate(ctx context.Context) (forge.GitAuth, error) {
	credential, err := c.credential(ctx)
	if err != nil {
		return forge.GitAuth{}, err
	}
	if credential.Username == "" {
		credential.Username = tokenUsername
	}
	return forge.HelperAuth(credential), nil
}

// Push puts the branch on the forge with a credential minted for this call.
func (c *Client) Push(ctx context.Context, request forge.PushRequest) (string, error) {
	if request.Git == nil {
		return "", errors.New("github: push needs a git runner from the workspace")
	}
	if request.Branch == "" {
		return "", errors.New("github: push needs a branch")
	}
	auth, err := c.Authenticate(ctx)
	if err != nil {
		return "", err
	}
	args := make([]string, 0, len(auth.Args)+5)
	args = append(args, auth.Args...)
	args = append(args, "push", "--porcelain")
	if request.Force {
		args = append(args, "--force-with-lease")
	}
	args = append(args, c.repo.CloneURL, "refs/heads/"+request.Branch+":refs/heads/"+request.Branch)
	if _, err := request.Git.Run(ctx, auth.Env, args...); err != nil {
		return "", fmt.Errorf("github: push %s: %w", request.Branch, err)
	}
	head, err := request.Git.Run(ctx, nil, "rev-parse", "refs/heads/"+request.Branch)
	if err != nil {
		return "", fmt.Errorf("github: read %s: %w", request.Branch, err)
	}
	return strings.TrimSpace(head), nil
}

// OpenPR opens a pull request for the branch, or returns the one already
// open for it. Asking twice is asking once: GitHub refuses a duplicate, and
// a resumed run must be able to ask again.
//
// An existing pull request is not returned until GitHub shows its branch at
// the requested head: a branch name reused across restarts can have a pull
// request GitHub already knows about, left at whatever an earlier
// generation's push last put there (steading#17), and its cached head is
// stale until this same wait catches it up, the way the create path below
// already waits for a brand new branch.
func (c *Client) OpenPR(ctx context.Context, request forge.OpenPRRequest) (forge.PullRequest, error) {
	if request.Branch == "" {
		return forge.PullRequest{}, errors.New("github: opening a pull request needs a branch")
	}
	existing, err := c.PR(ctx, request.Branch)
	switch {
	case err == nil:
		if request.Head == "" {
			return existing, nil
		}
		if err := c.awaitBranch(ctx, request.Branch, request.Head); err != nil {
			return forge.PullRequest{}, err
		}
		return c.PR(ctx, request.Branch)
	case !errors.Is(err, forge.ErrNoPullRequest):
		return forge.PullRequest{}, err
	}

	// Nothing is created until GitHub shows the branch the pull request
	// would come from: a pull request opened before the push is visible
	// is refused for a head that does not exist.
	if request.Head != "" {
		if err := c.awaitBranch(ctx, request.Branch, request.Head); err != nil {
			return forge.PullRequest{}, err
		}
	}

	repository, err := c.repository(ctx)
	if err != nil {
		return forge.PullRequest{}, err
	}
	target := request.Target
	if target == "" {
		target = repository.DefaultBranch
	}
	title := request.Title
	if title == "" {
		title = request.Branch
	}
	body := map[string]any{
		"head":  request.Branch,
		"base":  target,
		"title": title,
		"body":  request.Description,
	}
	var opened pullRequest
	if err := c.call(ctx, http.MethodPost, c.path("pulls"), nil, body, &opened); err != nil {
		return forge.PullRequest{}, err
	}
	return opened.mapped(repository.mergeMethod() == "squash"), nil
}

// PR is the pull request for a branch. An open one wins; a merged or closed
// one is still returned, because the merge commit is read through the pull
// request after the branch is gone.
func (c *Client) PR(ctx context.Context, branch string) (forge.PullRequest, error) {
	if branch == "" {
		return forge.PullRequest{}, errors.New("github: a pull request is looked up by branch")
	}
	repository, err := c.repository(ctx)
	if err != nil {
		return forge.PullRequest{}, err
	}
	query := url.Values{
		"head":      {c.owner() + ":" + branch},
		"state":     {"all"},
		"sort":      {"updated"},
		"direction": {"desc"},
	}
	var found []pullRequest
	if err := c.call(ctx, http.MethodGet, c.path("pulls"), query, nil, &found); err != nil {
		return forge.PullRequest{}, err
	}
	if len(found) == 0 {
		return forge.PullRequest{}, fmt.Errorf("github: %s: %w", branch, forge.ErrNoPullRequest)
	}
	squash := repository.mergeMethod() == "squash"
	for _, candidate := range found {
		if candidate.State == "open" {
			return candidate.mapped(squash), nil
		}
	}
	return found[0].mapped(squash), nil
}

// checkRuns is GitHub's answer for a commit's check runs.
type checkRuns struct {
	Total int `json:"total_count"`
	Runs  []struct {
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		HTMLURL    string `json:"html_url"`
	} `json:"check_runs"`
}

// combinedStatus is GitHub's answer for a commit's older-style statuses.
type combinedStatus struct {
	State string `json:"state"`
	Total int    `json:"total_count"`
}

// commitStatus is one of GitHub's older-style per-context statuses, read by
// [Client.FailingChecks] for the name and URL the combined status GitHub
// gives [Client.Checks] does not carry.
type commitStatus struct {
	Context   string `json:"context"`
	State     string `json:"state"`
	TargetURL string `json:"target_url"`
}

// Checks is what GitHub says about a commit, across both kinds of check it
// has. A commit with no check runs and no statuses is [forge.Success]:
// there is nothing to wait for and nothing to be red. Otherwise anything
// still running is pending, and anything settled and not green is failed.
func (c *Client) Checks(ctx context.Context, sha string) (forge.Status, error) {
	if sha == "" {
		return "", errors.New("github: checks are read for a commit")
	}
	var runs checkRuns
	if err := c.call(ctx, http.MethodGet, c.path("commits", sha, "check-runs"),
		url.Values{"per_page": {"100"}}, nil, &runs); err != nil {
		return "", err
	}
	var statuses combinedStatus
	if err := c.call(ctx, http.MethodGet, c.path("commits", sha, "status"), nil, nil, &statuses); err != nil {
		return "", err
	}
	if runs.Total == 0 && statuses.Total == 0 {
		return forge.Success, nil
	}
	status := forge.Success
	for _, run := range runs.Runs {
		status = worse(status, runStatus(run.Status, run.Conclusion))
	}
	if statuses.Total > 0 {
		status = worse(status, statusOf(statuses.State))
	}
	return status, nil
}

// runStatus flattens one check run. A run that has not completed is
// pending; a completed one is green on `success`, `neutral` or `skipped`
// and red on everything else. https://docs.github.com/rest/checks/runs
// lists the values.
func runStatus(status, conclusion string) forge.Status {
	if status != "completed" {
		return forge.Pending
	}
	switch conclusion {
	case "success", "neutral", "skipped":
		return forge.Success
	default:
		return forge.Failed
	}
}

// statusOf flattens a combined commit status.
func statusOf(state string) forge.Status {
	switch state {
	case "success":
		return forge.Success
	case "pending":
		return forge.Pending
	default:
		return forge.Failed
	}
}

// worse is the more decisive of two answers: a failure over a wait, a wait
// over a pass.
func worse(a, b forge.Status) forge.Status {
	rank := map[forge.Status]int{forge.Success: 0, forge.Pending: 1, forge.Failed: 2}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// DefaultHead is the repository's default branch and its current head sha,
// the same two calls [Probe] already makes in sequence.
func (c *Client) DefaultHead(ctx context.Context) (string, string, error) {
	repository, err := c.repository(ctx)
	if err != nil {
		return "", "", err
	}
	sha, err := c.BranchHead(ctx, repository.DefaultBranch)
	if err != nil {
		return "", "", err
	}
	return repository.DefaultBranch, sha, nil
}

// FailingChecks is every check run and commit status on sha that is not
// green, by name and URL. GitHub reports checks two ways — check runs and
// the older, per-context commit statuses — and a commit missing entirely
// from one of them has nothing there to be red about.
func (c *Client) FailingChecks(ctx context.Context, sha string) ([]forge.CheckResult, error) {
	if sha == "" {
		return nil, errors.New("github: checks are read for a commit")
	}
	var runs checkRuns
	if err := c.call(ctx, http.MethodGet, c.path("commits", sha, "check-runs"),
		url.Values{"per_page": {"100"}}, nil, &runs); err != nil {
		return nil, err
	}
	var statuses []commitStatus
	if err := c.call(ctx, http.MethodGet, c.path("commits", sha, "statuses"),
		url.Values{"per_page": {"100"}}, nil, &statuses); err != nil {
		return nil, err
	}
	var failing []forge.CheckResult
	for _, run := range runs.Runs {
		if runStatus(run.Status, run.Conclusion) != forge.Success {
			failing = append(failing, forge.CheckResult{Name: run.Name, URL: run.HTMLURL})
		}
	}
	seen := map[string]bool{}
	for _, status := range statuses {
		if seen[status.Context] {
			continue
		}
		seen[status.Context] = true
		if statusOf(status.State) != forge.Success {
			failing = append(failing, forge.CheckResult{Name: status.Context, URL: status.TargetURL})
		}
	}
	return failing, nil
}

// Merge merges the pull request by the method the repository's settings
// allow. It names the head it merges: `sha` is the head as the caller read
// it, so GitHub merges the commit the run's steps checked and nothing else.
//
// A GitHub that declines — a protected branch, a required check, a review
// it wants first — answers 405 and that is a [forge.MergeRefusedError]
// naming its reason; the mill does not retry it as a fresh attempt, though
// a refusal that names its own transience through
// [forge.MergeRefusedError.Pending] or [forge.MergeRefusedError.Conflict]
// is [forge.Retryable] in place (mill#42). A head that has moved since it
// was read answers 409, which is a [forge.HeadMovedError], and the step
// tries again with the head it finds.
//
// Before asking to merge, Merge waits out GitHub's own mergeability
// recomputation — the same lag [Client.OpenPR] waits out for a just-pushed
// branch — so a catch-up's push does not spend an attempt on an answer that
// clears itself within seconds.
func (c *Client) Merge(ctx context.Context, pr forge.PullRequest) (forge.PullRequest, error) {
	if pr.Number == 0 {
		return forge.PullRequest{}, errors.New("github: merging needs a pull request number")
	}
	head, err := c.head(ctx, pr)
	if err != nil {
		return forge.PullRequest{}, err
	}
	repository, err := c.repository(ctx)
	if err != nil {
		return forge.PullRequest{}, err
	}
	method := repository.mergeMethod()
	if pr.Squash && repository.AllowSquashMerge {
		method = "squash"
	}
	state, err := c.awaitMergeable(ctx, pr.Number)
	if err != nil {
		return forge.PullRequest{}, err
	}
	body := map[string]any{"sha": head, "merge_method": method}
	err = c.call(ctx, http.MethodPut, c.path("pulls", strconv.Itoa(pr.Number), "merge"), nil, body, nil)
	if err != nil {
		var failure *apiError
		if errors.As(err, &failure) && failure.Status == http.StatusConflict {
			return forge.PullRequest{}, &forge.HeadMovedError{PR: pr.Number, SHA: head, Reason: failure.Message}
		}
		if errors.As(err, &failure) && refusal(failure.Status) {
			return forge.PullRequest{}, &forge.MergeRefusedError{
				PR: pr.Number, Status: failure.Status, Reason: failure.Message, Detail: state,
				Pending: state == "unknown", Conflict: state == "dirty",
			}
		}
		return forge.PullRequest{}, err
	}
	merged, err := c.pull(ctx, pr.Number)
	if err != nil {
		return forge.PullRequest{}, err
	}
	return merged.mapped(method == "squash"), nil
}

// awaitMergeable polls the pull request's `mergeable_state` until GitHub
// has finished computing it — anything other than `unknown` — or the
// client's wait runs out, whichever comes first. It returns the last state
// it saw either way: a state still `unknown` past the deadline is not this
// call's failure to report, since [Client.Merge]'s caller decides what to
// do with an answer that has not settled. The first read happens before
// any sleep, so a pull request already settled costs one round trip and no
// delay.
func (c *Client) awaitMergeable(ctx context.Context, number int) (string, error) {
	deadline := time.Now().Add(c.mergeableWait)
	for {
		current, err := c.pull(ctx, number)
		if err != nil {
			return "", err
		}
		if current.MergeableState != "unknown" {
			return current.MergeableState, nil
		}
		if !time.Now().Before(deadline) {
			return current.MergeableState, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(c.mergeablePoll):
		}
	}
}

// head is the commit a merge names: the one on the pull request the caller
// read. A caller that carries none has it read here rather than merging
// blind.
func (c *Client) head(ctx context.Context, pr forge.PullRequest) (string, error) {
	if pr.HeadSHA != "" {
		return pr.HeadSHA, nil
	}
	current, err := c.pull(ctx, pr.Number)
	if err != nil {
		return "", err
	}
	if current.Head.SHA == "" {
		return "", fmt.Errorf("github: #%d has no head commit to merge", pr.Number)
	}
	return current.Head.SHA, nil
}

// refusal reports whether a status is GitHub declining rather than failing.
// 405 is "not mergeable" — a protected branch, a required check, a review
// wanted first; 403 is a merge the credential may not make; 422 is the
// request being unprocessable as posed. 409 is not among them: that is the
// head moving under the merge, which [Merge] reports on its own.
func refusal(status int) bool {
	switch status {
	case http.StatusMethodNotAllowed, http.StatusForbidden, http.StatusUnprocessableEntity:
		return true
	default:
		return false
	}
}

// pull reads one pull request.
func (c *Client) pull(ctx context.Context, number int) (pullRequest, error) {
	var current pullRequest
	err := c.call(ctx, http.MethodGet, c.path("pulls", strconv.Itoa(number)), nil, nil, &current)
	return current, err
}

// MergeCommit is the commit on the default branch the pull request became.
// It is read from GitHub rather than derived from the branch, because a
// squashed merge's commit is a new one and the branch it came from may
// already be deleted.
func (c *Client) MergeCommit(ctx context.Context, pr forge.PullRequest) (string, error) {
	if pr.Number == 0 {
		return "", errors.New("github: a merge commit is read for a pull request")
	}
	current, err := c.pull(ctx, pr.Number)
	if err != nil {
		return "", err
	}
	mapped := current.mapped(pr.Squash)
	if mapped.MergeCommit == "" {
		return "", fmt.Errorf("github: #%d is %s: %w", pr.Number, mapped.State, forge.ErrNotMerged)
	}
	return mapped.MergeCommit, nil
}

// ClosePR comments on a pull request and closes it without merging. The
// comment goes first: a pull request that closes with no explanation is
// the thing a person finds a week later and cannot account for. A pull
// request already closed or merged is left as it is.
func (c *Client) ClosePR(ctx context.Context, pr forge.PullRequest, comment string) error {
	if pr.Number == 0 {
		return errors.New("github: closing needs a pull request number")
	}
	if comment != "" {
		if err := c.call(ctx, http.MethodPost,
			c.path("issues", strconv.Itoa(pr.Number), "comments"), nil,
			map[string]any{"body": comment}, nil); err != nil {
			return err
		}
	}
	current, err := c.pull(ctx, pr.Number)
	if err != nil {
		return err
	}
	if current.State != "open" {
		return nil
	}
	return c.call(ctx, http.MethodPatch, c.path("pulls", strconv.Itoa(pr.Number)), nil,
		map[string]any{"state": "closed"}, nil)
}

// BranchHead is the commit GitHub shows a branch at, empty when it has no
// such branch. A branch that is not there is not an error: "not yet" and
// "never" are the same answer one read at a time.
func (c *Client) BranchHead(ctx context.Context, name string) (string, error) {
	if name == "" {
		return "", errors.New("github: a branch is read by name")
	}
	var found struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	err := c.call(ctx, http.MethodGet, c.path("branches", url.PathEscape(name)), nil, nil, &found)
	var failure *apiError
	if errors.As(err, &failure) && failure.Status == http.StatusNotFound {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return found.Commit.SHA, nil
}

// awaitBranch blocks until GitHub shows branch at head, giving up after the
// client's wait with a *[forge.BranchMissingError] naming both. The first
// read happens before any sleep, so a forge that is already caught up costs
// one round trip and no delay.
func (c *Client) awaitBranch(ctx context.Context, name, head string) error {
	deadline := time.Now().Add(c.branchWait)
	var reads int
	var found string
	for {
		sha, err := c.BranchHead(ctx, name)
		if err != nil {
			return err
		}
		reads++
		if sha == head {
			return nil
		}
		found = sha
		if !time.Now().Before(deadline) {
			return &forge.BranchMissingError{
				Branch: name, Head: head, Found: found,
				Waited: c.branchWait, Reads: reads,
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.branchPoll):
		}
	}
}

// DeleteBranch removes a branch from the repository. A branch GitHub does
// not have is the state asked for: it answers 422 "Reference does not
// exist" for one, and 404 for a repository with no such ref, and both are
// success here.
func (c *Client) DeleteBranch(ctx context.Context, branch string) error {
	if branch == "" {
		return errors.New("github: deleting needs a branch")
	}
	err := c.call(ctx, http.MethodDelete, c.path("git", "refs", "heads", url.PathEscape(branch)), nil, nil, nil)
	var failure *apiError
	if errors.As(err, &failure) &&
		(failure.Status == http.StatusNotFound || failure.Status == http.StatusUnprocessableEntity) {
		return nil
	}
	return err
}

// repository is GitHub's record of the repository, fetched once.
func (c *Client) repository(ctx context.Context) (repository, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.info != nil {
		return *c.info, nil
	}
	var info repository
	if err := c.call(ctx, http.MethodGet, "/repos/"+c.repo.Path, nil, nil, &info); err != nil {
		return repository{}, err
	}
	c.info = &info
	return info, nil
}

// path is an endpoint under the repository.
func (c *Client) path(parts ...string) string {
	return "/repos/" + c.repo.Path + "/" + strings.Join(parts, "/")
}

// credential mints one for the step this client is bound to. Every call
// mints afresh: the credential's life is the request's.
func (c *Client) credential(ctx context.Context) (forge.Credential, error) {
	if c.minter == nil {
		return forge.Credential{}, errors.New("github: no minter: this client cannot produce a credential")
	}
	credential, err := c.minter.Mint(ctx, c.step)
	if err != nil {
		return forge.Credential{}, fmt.Errorf("github: mint a credential: %w", err)
	}
	if credential.Token == "" {
		return forge.Credential{}, errors.New("github: the minter produced no credential")
	}
	return credential, nil
}

// apiError is GitHub answering with a status the call cannot use. It
// carries the message verbatim, because GitHub's own words are what a
// refusal has to report, and the permissions the endpoint would have
// accepted, which GitHub names on a request a fine-grained token could not
// make.
type apiError struct {
	Status  int
	Method  string
	Path    string
	Message string
	// Accepted is GitHub's `X-Accepted-GitHub-Permissions` header: the
	// permission sets the endpoint accepts, such as `contents=read`.
	Accepted string
}

func (e *apiError) Error() string {
	message := e.Message
	if message == "" {
		message = http.StatusText(e.Status)
	}
	return fmt.Sprintf("github: %s %s: %d %s", e.Method, e.Path, e.Status, message)
}

// call makes one API request with a freshly minted credential and decodes
// the answer.
func (c *Client) call(ctx context.Context, method, path string, query url.Values, body, into any) error {
	credential, err := c.credential(ctx)
	if err != nil {
		return err
	}
	endpoint := API + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	var payload io.Reader = http.NoBody
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("github: encode %s %s: %w", method, path, err)
		}
		payload = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, payload)
	if err != nil {
		return fmt.Errorf("github: %s %s: %w", method, path, err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", apiVersion)
	request.Header.Set("Authorization", "Bearer "+credential.Token)

	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("github: %s %s: %w", method, path, err)
	}
	defer func() { _ = response.Body.Close() }()
	answer, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("github: %s %s: %w", method, path, err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &apiError{
			Status: response.StatusCode, Method: method, Path: path,
			Message:  messageOf(answer),
			Accepted: response.Header.Get("X-Accepted-GitHub-Permissions"),
		}
	}
	if into == nil {
		return nil
	}
	if err := json.Unmarshal(answer, into); err != nil {
		return fmt.Errorf("github: decode %s %s: %w", method, path, err)
	}
	return nil
}

// maxResponseBytes caps what one API answer may be.
const maxResponseBytes = 8 << 20

// messageOf is GitHub's own words for a failure: `message`, with the
// field errors it lists after it; the raw body is the fallback so nothing
// is lost.
func messageOf(body []byte) string {
	var envelope struct {
		Message string `json:"message"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Message == "" {
		return strings.TrimSpace(string(body))
	}
	message := envelope.Message
	for _, detail := range envelope.Errors {
		if detail.Message != "" {
			message += ": " + detail.Message
		}
	}
	return message
}

// Forge is the interface this implements.
var _ forge.Forge = (*Client)(nil)
