// Package gitlab is the forge adapter for a self-hosted GitLab, over its REST
// API v4.
//
// GitLab's vocabulary is not the design's. A *merge request* is a pull
// request, its `iid` is the number, `detailed_merge_status` is why it will
// not merge, and a squashed merge lands as `squash_commit_sha` rather than
// `merge_commit_sha`. The mapping is this package's whole job: nothing
// outside it says "merge request", and nothing inside it decides policy.
//
// Three GitLab facts shape the code. A project with no `.gitlab-ci.yml` has no
// pipelines at all rather than a green one, so an empty pipeline list is
// [forge.Success] — there is nothing to be red. And a merge GitLab declines
// answers 405 or 406 with a message, which becomes a
// [forge.MergeRefusedError] carrying that message and the merge request's
// `detailed_merge_status`: the mill reports the forge's reason and does not
// retry. And a merge names the commit it merges: GitLab 19 requires `sha` on
// the merge and answers 409 when it is not the branch's head, which is how
// the mill learns the head moved and the merge is worth another attempt.
package gitlab

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

	"github.com/ivy/mill/internal/forge"
)

// Register installs this adapter in a registry, which is all it takes for a
// project on a GitLab host to be deliverable.
func Register(registry *forge.Registry, options ...Option) {
	registry.Register(forge.KindGitLab, func(repo forge.Repo, minter forge.Minter) (forge.Forge, error) {
		return New(repo, minter, options...), nil
	})
}

// Client is one repository on one GitLab. It holds no credential: every call
// mints one for the step it is bound to and drops it when the call returns.
type Client struct {
	repo   forge.Repo
	minter forge.Minter
	http   *http.Client
	step   forge.Step

	// project is GitLab's answer about the repository — its id, its default
	// branch, its squash setting — which does not change during a run.
	mu      *sync.Mutex
	project *projectInfo
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

// New returns a client for the repository, minting credentials through
// minter.
func New(repo forge.Repo, minter forge.Minter, options ...Option) *Client {
	c := &Client{
		repo:   repo,
		minter: minter,
		http:   &http.Client{Timeout: 30 * time.Second},
		mu:     &sync.Mutex{},
	}
	for _, option := range options {
		option(c)
	}
	return c
}

// ForStep returns a copy bound to the step its credentials are minted
// against. The GitLab project's own details are shared, because they are the
// repository's and not the step's.
func (c *Client) ForStep(step forge.Step) forge.Forge {
	bound := *c
	bound.step = step
	return &bound
}

// Repo is the repository this client serves.
func (c *Client) Repo() forge.Repo { return c.repo }

// projectInfo is what GitLab says about the repository itself.
type projectInfo struct {
	ID            int    `json:"id"`
	Path          string `json:"path_with_namespace"`
	DefaultBranch string `json:"default_branch"`
	SquashOption  string `json:"squash_option"`
	RemoveBranch  bool   `json:"remove_source_branch_after_merge"`
}

// squashes reports whether the project's own setting says a merge request
// merges squashed. `always` and `default_on` are the two that do; the mill
// takes the project's default rather than deciding for it.
func (p projectInfo) squashes() bool {
	return p.SquashOption == "always" || p.SquashOption == "default_on"
}

// mergeRequest is GitLab's merge request, as much of it as the mill reads.
type mergeRequest struct {
	IID                 int    `json:"iid"`
	WebURL              string `json:"web_url"`
	SourceBranch        string `json:"source_branch"`
	TargetBranch        string `json:"target_branch"`
	SHA                 string `json:"sha"`
	State               string `json:"state"`
	Squash              bool   `json:"squash"`
	MergeCommitSHA      string `json:"merge_commit_sha"`
	SquashCommitSHA     string `json:"squash_commit_sha"`
	DetailedMergeStatus string `json:"detailed_merge_status"`
}

// pullRequest maps a merge request onto the design's vocabulary. A squashed
// merge's commit is `squash_commit_sha`; `merge_commit_sha` is empty for it,
// which is the whole reason the merge commit is read through the merge
// request rather than assumed to be the branch's head.
func (m mergeRequest) pullRequest() forge.PullRequest {
	commit := m.SquashCommitSHA
	if commit == "" {
		commit = m.MergeCommitSHA
	}
	return forge.PullRequest{
		Number:      m.IID,
		URL:         m.WebURL,
		Branch:      m.SourceBranch,
		Target:      m.TargetBranch,
		HeadSHA:     m.SHA,
		State:       stateOf(m.State),
		Squash:      m.Squash,
		MergeCommit: commit,
		Detail:      m.DetailedMergeStatus,
	}
}

// stateOf maps GitLab's merge request states onto the three the mill knows.
// `locked` is a merge in flight, which is still open as far as the mill is
// concerned.
func stateOf(state string) string {
	switch state {
	case "opened", "locked":
		return "open"
	case "merged":
		return "merged"
	default:
		return "closed"
	}
}

// pipeline is a pipeline's status for a commit.
type pipeline struct {
	ID     int    `json:"id"`
	SHA    string `json:"sha"`
	Status string `json:"status"`
}

// Authenticate mints a credential for one git command and returns it as the
// arguments and environment git reads it through.
//
// The credential reaches git through the environment and a credential helper,
// never through the remote URL and never through argv: the clone's `origin`
// stays the plain repository URL, so a session that runs `git remote -v` — or
// reads `.git/config`, or lists the process table — finds no token. The
// helper is written inline, so nothing has to be installed on the host's
// PATH for a private repository to be readable.
func (c *Client) Authenticate(ctx context.Context) (forge.GitAuth, error) {
	credential, err := c.credential(ctx)
	if err != nil {
		return forge.GitAuth{}, err
	}
	return forge.GitAuth{Args: credentialArgs(), Env: credentialEnv(credential)}, nil
}

// Push puts the branch on the forge with a credential minted for this call.
func (c *Client) Push(ctx context.Context, request forge.PushRequest) (string, error) {
	if request.Git == nil {
		return "", errors.New("gitlab: push needs a git runner from the workspace")
	}
	if request.Branch == "" {
		return "", errors.New("gitlab: push needs a branch")
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
		return "", fmt.Errorf("gitlab: push %s: %w", request.Branch, err)
	}
	head, err := request.Git.Run(ctx, nil, "rev-parse", "refs/heads/"+request.Branch)
	if err != nil {
		return "", fmt.Errorf("gitlab: read %s: %w", request.Branch, err)
	}
	return strings.TrimSpace(head), nil
}

// Environment variable names the credential helper below reads. They exist
// only in the git child's environment, for the length of one push.
const (
	envUsername = "MILL_FORGE_USERNAME"
	envToken    = "MILL_FORGE_TOKEN" //nolint:gosec // the name of a variable, not a credential
)

// credentialArgs configure git to answer its own credential prompt from the
// environment. The empty helper first clears any inherited one, so the
// mill's answer is the only one; the helper itself contains no secret, which
// is why it is safe in argv.
func credentialArgs() []string {
	const helper = `!f() { test "$1" = get && printf 'username=%s\npassword=%s\n' "$` + envUsername + `" "$` + envToken + `"; }; f`
	return []string{"-c", "credential.helper=", "-c", "credential.helper=" + helper}
}

// credentialEnv is what the helper reads.
func credentialEnv(credential forge.Credential) map[string]string {
	username := credential.Username
	if username == "" {
		username = "oauth2"
	}
	return map[string]string{envUsername: username, envToken: credential.Token}
}

// OpenPR opens a merge request for the branch, or returns the one already
// open for it. Asking twice is asking once: GitLab refuses a duplicate, and a
// resumed run must be able to ask again.
func (c *Client) OpenPR(ctx context.Context, request forge.OpenPRRequest) (forge.PullRequest, error) {
	if request.Branch == "" {
		return forge.PullRequest{}, errors.New("gitlab: opening a merge request needs a branch")
	}
	existing, err := c.PR(ctx, request.Branch)
	switch {
	case err == nil:
		return existing, nil
	case !errors.Is(err, forge.ErrNoPullRequest):
		return forge.PullRequest{}, err
	}

	project, err := c.projectInfo(ctx)
	if err != nil {
		return forge.PullRequest{}, err
	}
	target := request.Target
	if target == "" {
		target = project.DefaultBranch
	}
	title := request.Title
	if title == "" {
		title = request.Branch
	}
	body := map[string]any{
		"source_branch":        request.Branch,
		"target_branch":        target,
		"title":                title,
		"description":          request.Description,
		"squash":               project.squashes(),
		"remove_source_branch": project.RemoveBranch,
	}
	var opened mergeRequest
	if err := c.call(ctx, http.MethodPost, c.path("merge_requests"), nil, body, &opened); err != nil {
		return forge.PullRequest{}, err
	}
	return opened.pullRequest(), nil
}

// PR is the merge request for a branch. An open one wins; a merged or closed
// one is still returned, because the merge commit is read through the merge
// request after the branch is gone.
func (c *Client) PR(ctx context.Context, branch string) (forge.PullRequest, error) {
	if branch == "" {
		return forge.PullRequest{}, errors.New("gitlab: a merge request is looked up by branch")
	}
	query := url.Values{
		"source_branch": {branch},
		"order_by":      {"updated_at"},
		"sort":          {"desc"},
	}
	var found []mergeRequest
	if err := c.call(ctx, http.MethodGet, c.path("merge_requests"), query, nil, &found); err != nil {
		return forge.PullRequest{}, err
	}
	if len(found) == 0 {
		return forge.PullRequest{}, fmt.Errorf("gitlab: %s: %w", branch, forge.ErrNoPullRequest)
	}
	for _, candidate := range found {
		if stateOf(candidate.State) == "open" {
			return candidate.pullRequest(), nil
		}
	}
	return found[0].pullRequest(), nil
}

// Checks is what GitLab's pipelines say about a commit.
//
// A project with no `.gitlab-ci.yml` has no pipelines for any commit, which
// is [forge.Success]: there is no check to wait for and nothing to be red.
func (c *Client) Checks(ctx context.Context, sha string) (forge.Status, error) {
	if sha == "" {
		return "", errors.New("gitlab: checks are read for a commit")
	}
	var pipelines []pipeline
	query := url.Values{"sha": {sha}, "order_by": {"id"}, "sort": {"desc"}}
	if err := c.call(ctx, http.MethodGet, c.path("pipelines"), query, nil, &pipelines); err != nil {
		return "", err
	}
	if len(pipelines) == 0 {
		return forge.Success, nil
	}
	return statusOf(pipelines[0].Status), nil
}

// statusOf flattens a GitLab pipeline status onto the three answers a `ci`
// step acts on. Anything not settled is pending; anything settled and not
// green is failed, except a pipeline that was skipped, which had nothing to
// run. https://docs.gitlab.com/ee/api/pipelines.html lists the values.
func statusOf(status string) forge.Status {
	switch status {
	case "created", "waiting_for_resource", "preparing", "pending", "running", "scheduled", "manual":
		return forge.Pending
	case "success", "skipped":
		return forge.Success
	default:
		return forge.Failed
	}
}

// Merge merges the merge request, squashing when the project's setting says
// so. It names the head it merges: `sha` is the source branch's head as the
// caller read it, so GitLab merges the commit the run's steps checked and
// nothing else. GitLab 19 requires the field — a merge without it is
// answered `400 SHA must be provided when merging` — and it is what makes the
// merge safe rather than merely accepted.
//
// A GitLab that declines yields a [forge.MergeRefusedError] naming its
// reason; the mill does not retry it. A head that has moved since it was read
// is the one exception: GitLab answers 409, and that is a
// [forge.HeadMovedError], which the step tries again with the head it finds.
func (c *Client) Merge(ctx context.Context, pr forge.PullRequest) (forge.PullRequest, error) {
	if pr.Number == 0 {
		return forge.PullRequest{}, errors.New("gitlab: merging needs a merge request number")
	}
	head, err := c.head(ctx, pr)
	if err != nil {
		return forge.PullRequest{}, err
	}
	project, err := c.projectInfo(ctx)
	if err != nil {
		return forge.PullRequest{}, err
	}
	body := map[string]any{
		"sha":                         head,
		"squash":                      project.squashes() || pr.Squash,
		"should_remove_source_branch": project.RemoveBranch,
	}
	var merged mergeRequest
	err = c.call(ctx, http.MethodPut, c.path("merge_requests", strconv.Itoa(pr.Number), "merge"), nil, body, &merged)
	if err != nil {
		var failure *apiError
		if errors.As(err, &failure) && failure.Status == http.StatusConflict {
			return forge.PullRequest{}, &forge.HeadMovedError{
				PR:     pr.Number,
				SHA:    head,
				Reason: failure.Message,
			}
		}
		if errors.As(err, &failure) && refusal(failure.Status) {
			return forge.PullRequest{}, &forge.MergeRefusedError{
				PR:     pr.Number,
				Status: failure.Status,
				Reason: failure.Message,
				Detail: pr.Detail,
			}
		}
		return forge.PullRequest{}, err
	}
	return merged.pullRequest(), nil
}

// head is the commit a merge names: the one on the pull request the caller
// read. A caller that carries no head — a resumed run holding nothing but a
// number — has it read here rather than merging blind, and a merge request
// GitLab reports no head for is refused, because a merge that names no commit
// is a merge of whatever the branch is at when GitLab gets to it.
func (c *Client) head(ctx context.Context, pr forge.PullRequest) (string, error) {
	if pr.HeadSHA != "" {
		return pr.HeadSHA, nil
	}
	var current mergeRequest
	if err := c.call(ctx, http.MethodGet,
		c.path("merge_requests", strconv.Itoa(pr.Number)), nil, nil, &current); err != nil {
		return "", err
	}
	if current.SHA == "" {
		return "", fmt.Errorf("gitlab: !%d has no head commit to merge", pr.Number)
	}
	return current.SHA, nil
}

// refusal reports whether a status is GitLab declining rather than failing.
// 405 is "not mergeable" — a protected branch, an unapproved request, a
// pipeline that must pass; 406 is a branch that cannot be merged; 422 is the
// request being unprocessable as posed. 409 is not among them: that is the
// head moving under the merge, which [Merge] reports on its own.
func refusal(status int) bool {
	switch status {
	case http.StatusMethodNotAllowed, http.StatusNotAcceptable, http.StatusUnprocessableEntity:
		return true
	default:
		return false
	}
}

// MergeCommit is the commit on the default branch the merge request became.
// It is read from GitLab rather than derived from the branch, because a
// squashed merge's commit is a new one and the branch it came from may
// already be deleted.
func (c *Client) MergeCommit(ctx context.Context, pr forge.PullRequest) (string, error) {
	if pr.Number == 0 {
		return "", errors.New("gitlab: a merge commit is read for a merge request")
	}
	var current mergeRequest
	if err := c.call(ctx, http.MethodGet, c.path("merge_requests", strconv.Itoa(pr.Number)), nil, nil, &current); err != nil {
		return "", err
	}
	mapped := current.pullRequest()
	if mapped.MergeCommit == "" {
		return "", fmt.Errorf("gitlab: !%d is %s: %w", pr.Number, mapped.State, forge.ErrNotMerged)
	}
	return mapped.MergeCommit, nil
}

// ClosePR comments on a merge request and closes it without merging. The
// comment goes first: a merge request that closes with no explanation is the
// thing a person finds a week later and cannot account for.
//
// A merge request that is already closed or merged is left as it is. This is
// `mill restart` tidying up after a run that went sideways, and it is asked
// for an end state, not for a transition.
func (c *Client) ClosePR(ctx context.Context, pr forge.PullRequest, comment string) error {
	if pr.Number == 0 {
		return errors.New("gitlab: closing needs a merge request number")
	}
	if comment != "" {
		if err := c.call(ctx, http.MethodPost,
			c.path("merge_requests", strconv.Itoa(pr.Number), "notes"), nil,
			map[string]any{"body": comment}, nil); err != nil {
			return err
		}
	}
	var current mergeRequest
	if err := c.call(ctx, http.MethodGet, c.path("merge_requests", strconv.Itoa(pr.Number)), nil, nil, &current); err != nil {
		return err
	}
	if state := current.pullRequest().State; state != "open" {
		return nil
	}
	return c.call(ctx, http.MethodPut, c.path("merge_requests", strconv.Itoa(pr.Number)), nil,
		map[string]any{"state_event": "close"}, nil)
}

// DeleteBranch removes a branch from the repository. A branch GitLab does
// not have is the state asked for, so a 404 is success.
func (c *Client) DeleteBranch(ctx context.Context, branch string) error {
	if branch == "" {
		return errors.New("gitlab: deleting needs a branch")
	}
	err := c.call(ctx, http.MethodDelete,
		c.path("repository", "branches", url.PathEscape(branch)), nil, nil, nil)
	var failure *apiError
	if errors.As(err, &failure) && failure.Status == http.StatusNotFound {
		return nil
	}
	return err
}

// projectInfo is GitLab's record of the repository, fetched once.
func (c *Client) projectInfo(ctx context.Context) (projectInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.project != nil {
		return *c.project, nil
	}
	var info projectInfo
	if err := c.call(ctx, http.MethodGet, "/api/v4/projects/"+url.PathEscape(c.repo.Path), nil, nil, &info); err != nil {
		return projectInfo{}, err
	}
	c.project = &info
	return info, nil
}

// path is an endpoint under the repository. The project is addressed by its
// URL-encoded path, so no id has to be resolved before the first call.
func (c *Client) path(parts ...string) string {
	return "/api/v4/projects/" + url.PathEscape(c.repo.Path) + "/" + strings.Join(parts, "/")
}

// credential mints one for the step this client is bound to. Every call mints
// afresh: the credential's life is the request's.
func (c *Client) credential(ctx context.Context) (forge.Credential, error) {
	if c.minter == nil {
		return forge.Credential{}, errors.New("gitlab: no minter: this client cannot produce a credential")
	}
	credential, err := c.minter.Mint(ctx, c.step)
	if err != nil {
		return forge.Credential{}, fmt.Errorf("gitlab: mint a credential: %w", err)
	}
	if credential.Token == "" {
		return forge.Credential{}, errors.New("gitlab: the minter produced no credential")
	}
	return credential, nil
}

// apiError is GitLab answering with a status the call cannot use. It carries
// the message verbatim, because GitLab's own words are what a refusal has to
// report.
type apiError struct {
	Status  int
	Method  string
	Path    string
	Message string
}

func (e *apiError) Error() string {
	message := e.Message
	if message == "" {
		message = http.StatusText(e.Status)
	}
	return fmt.Sprintf("gitlab: %s %s: %d %s", e.Method, e.Path, e.Status, message)
}

// call makes one API request with a freshly minted credential and decodes the
// answer.
func (c *Client) call(ctx context.Context, method, path string, query url.Values, body, into any) error {
	credential, err := c.credential(ctx)
	if err != nil {
		return err
	}
	endpoint := strings.TrimSuffix(c.repo.BaseURL, "/") + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	var payload io.Reader = http.NoBody
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("gitlab: encode %s %s: %w", method, path, err)
		}
		payload = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, payload)
	if err != nil {
		return fmt.Errorf("gitlab: %s %s: %w", method, path, err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Accept", "application/json")
	authorize(request, credential)

	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("gitlab: %s %s: %w", method, path, err)
	}
	defer func() { _ = response.Body.Close() }()
	answer, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("gitlab: %s %s: %w", method, path, err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &apiError{Status: response.StatusCode, Method: method, Path: path, Message: messageOf(answer)}
	}
	if into == nil {
		return nil
	}
	if err := json.Unmarshal(answer, into); err != nil {
		return fmt.Errorf("gitlab: decode %s %s: %w", method, path, err)
	}
	return nil
}

// maxResponseBytes caps what one API answer may be. A merge request list is
// the largest of them by far.
const maxResponseBytes = 8 << 20

// authorize puts the credential on the request. A personal, group or project
// access token is a PRIVATE-TOKEN; an OAuth or CI job token is a bearer, and
// the minter says which by the username it mints under.
func authorize(request *http.Request, credential forge.Credential) {
	if credential.Username == "oauth2" {
		request.Header.Set("Authorization", "Bearer "+credential.Token)
		return
	}
	request.Header.Set("PRIVATE-TOKEN", credential.Token)
}

// messageOf is GitLab's own words for a failure. It answers `message` or
// `error`, either of which may be a string, an object of field errors, or an
// array; the raw body is the fallback so nothing is lost.
func messageOf(body []byte) string {
	var envelope struct {
		Message json.RawMessage `json:"message"`
		Error   json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return strings.TrimSpace(string(body))
	}
	for _, raw := range []json.RawMessage{envelope.Message, envelope.Error} {
		if len(raw) == 0 {
			continue
		}
		var text string
		if err := json.Unmarshal(raw, &text); err == nil {
			return text
		}
		return strings.TrimSpace(string(raw))
	}
	return strings.TrimSpace(string(body))
}

// Forge is the interface this implements.
var _ forge.Forge = (*Client)(nil)
