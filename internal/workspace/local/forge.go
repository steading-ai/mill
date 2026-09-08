package local

import (
	"context"
	"errors"
	"strings"

	"github.com/ivy/mill/internal/forge"
	"github.com/ivy/mill/internal/workspace"
)

// Forge is the forge half of the verbs, bound to the step whose token the
// workspace currently holds. Binding here rather than at construction is what
// makes a credential the step's: the client mints one per call, for whichever
// step is live when the call is made, and a workspace between steps has no
// step to mint for.
func (w *Workspace) Forge() workspace.ForgeVerbs { return forgeVerbs{w: w} }

type forgeVerbs struct{ w *Workspace }

// client is the forge bound to the live step, or ErrNoForge.
func (f forgeVerbs) client() (forge.Forge, error) {
	if f.w.options.Forge == nil {
		return nil, workspace.ErrNoForge
	}
	f.w.mu.RLock()
	token := f.w.token
	f.w.mu.RUnlock()
	capabilities := make([]string, 0, len(token.Capabilities))
	for _, capability := range token.Capabilities {
		capabilities = append(capabilities, string(capability))
	}
	return forge.Bind(f.w.options.Forge, forge.Step{
		TokenID:      token.ID,
		TokenHash:    token.Hash,
		Capabilities: capabilities,
	}), nil
}

// Push puts the run's branch on the forge. The branch is the workspace's, not
// the request's: a session may push, but only what the run is working on.
func (f forgeVerbs) Push(ctx context.Context, request workspace.PushRequest) (workspace.PushResult, error) {
	handle := f.w.snapshot()
	if handle == nil {
		return workspace.PushResult{}, ErrNotCreated
	}
	if handle.Branch == "" {
		return workspace.PushResult{}, errors.New("workspace: the checkout is on no branch to push")
	}
	client, err := f.client()
	if err != nil {
		return workspace.PushResult{}, err
	}
	sha, err := client.Push(ctx, forge.PushRequest{
		Git:    gitRunner{w: f.w, dir: handle.Root},
		Branch: handle.Branch,
		Force:  request.Force,
	})
	if err != nil {
		return workspace.PushResult{}, err
	}
	return workspace.PushResult{Branch: handle.Branch, SHA: sha, Remote: f.remote()}, nil
}

// PR is the run branch's pull request, opened if the forge has none.
func (f forgeVerbs) PR(ctx context.Context, request workspace.PRRequest) (forge.PullRequest, error) {
	handle := f.w.snapshot()
	if handle == nil {
		return forge.PullRequest{}, ErrNotCreated
	}
	if handle.Branch == "" {
		return forge.PullRequest{}, errors.New("workspace: the checkout is on no branch to open a pull request for")
	}
	client, err := f.client()
	if err != nil {
		return forge.PullRequest{}, err
	}
	return client.OpenPR(ctx, forge.OpenPRRequest{
		Branch:      handle.Branch,
		Target:      request.Target,
		Title:       request.Title,
		Description: request.Description,
	})
}

// Checks is what the forge says about a commit, the run branch's head by
// default.
func (f forgeVerbs) Checks(ctx context.Context, request workspace.ChecksRequest) (workspace.ChecksResult, error) {
	handle := f.w.snapshot()
	if handle == nil {
		return workspace.ChecksResult{}, ErrNotCreated
	}
	client, err := f.client()
	if err != nil {
		return workspace.ChecksResult{}, err
	}
	sha := strings.TrimSpace(request.SHA)
	if sha == "" {
		head, err := f.w.git(ctx, handle.Root, "rev-parse", "HEAD")
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

// remote is the repository the run pushes to, for the record. It is the
// forge's clone URL and carries no credential.
func (f forgeVerbs) remote() string {
	type repoer interface{ Repo() forge.Repo }
	if named, ok := f.w.options.Forge.(repoer); ok {
		return named.Repo().CloneURL
	}
	return ""
}

// gitRunner is how the forge runs git: through the workspace's chokepoint,
// in the run's checkout, with the credential the forge minted added to the
// child's environment for that one command.
//
// Every value the forge passes is registered with the redactor first. A
// credential travels in the environment rather than in the remote URL or in
// argv, and the redactor is the second guarantee that it does not reach a log
// by another route.
type gitRunner struct {
	w   *Workspace
	dir string
}

func (g gitRunner) Run(ctx context.Context, env map[string]string, args ...string) (string, error) {
	for _, value := range env {
		g.w.redactor.Add(value)
	}
	out, err := g.w.gitWith(ctx, g.dir, env, args...)
	if err != nil {
		return "", err
	}
	return out, nil
}

// Git is the interface gitRunner satisfies.
var _ forge.Git = gitRunner{}

// forgeVerbs implements the socket's forge half.
var _ workspace.ForgeVerbs = forgeVerbs{}
