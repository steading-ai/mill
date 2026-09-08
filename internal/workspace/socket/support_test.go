package socket_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"

	"github.com/ivy/mill/internal/forge"
	"github.com/ivy/mill/internal/forge/gitlab"
	"github.com/ivy/mill/internal/workspace"
)

// tree is a workspace whose verbs record what reached them, so a spec can
// assert that a refused verb never touched the checkout. It is not the local
// workspace: what is under test here is the socket's gate, not what is
// behind it.
type tree struct {
	mu      sync.Mutex
	files   map[string]string
	execs   []workspace.Command
	commits []workspace.CommitRequest
	redacts []string
	forge   workspace.ForgeVerbs
}

func newTree() *tree { return &tree{files: map[string]string{"README.md": "as cloned\n"}} }

func (t *tree) snapshot() map[string]string {
	t.mu.Lock()
	defer t.mu.Unlock()
	files := make(map[string]string, len(t.files))
	for path, content := range t.files {
		files[path] = content
	}
	return files
}

func (t *tree) Exec(_ context.Context, command workspace.Command) (*workspace.Process, error) {
	t.mu.Lock()
	t.execs = append(t.execs, command)
	t.mu.Unlock()
	return workspace.Spawner{Floor: []string{}}.Exec(context.Background(), command)
}

func (t *tree) Read(_ context.Context, path string) ([]byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return []byte(t.files[path]), nil
}

func (t *tree) Write(_ context.Context, path string, content []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.files[path] = string(content)
	return nil
}

func (t *tree) List(context.Context, string) ([]workspace.Entry, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	entries := make([]workspace.Entry, 0, len(t.files))
	for name := range t.files {
		entries = append(entries, workspace.Entry{Name: name})
	}
	return entries, nil
}

func (t *tree) Git() workspace.Git { return t }

func (t *tree) Redact(values ...string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.redacts = append(t.redacts, values...)
}

func (t *tree) Status(context.Context) (workspace.Status, error) {
	return workspace.Status{Branch: "mill/mill-12", Clean: true}, nil
}

func (t *tree) Diff(context.Context, workspace.DiffRequest) (string, error) {
	return "diff --git a/README.md b/README.md\n", nil
}

func (t *tree) Log(context.Context, workspace.LogRequest) ([]workspace.CommitInfo, error) {
	return []workspace.CommitInfo{{SHA: "0d15ea5e", Subject: "as cloned"}}, nil
}

func (t *tree) Commit(_ context.Context, request workspace.CommitRequest) (workspace.CommitInfo, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.commits = append(t.commits, request)
	return workspace.CommitInfo{SHA: "c0ffee", Subject: request.Message}, nil
}

// unixDialer dials the workspace socket whatever host a request names.
func unixDialer(path string) func(ctx context.Context, network, address string) (net.Conn, error) {
	return func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}
}

// Forge is the forge half of the verbs. A tree with no forge answers
// [workspace.ErrNoForge], which is what a run on a project with no repository
// gets.
func (t *tree) Forge() workspace.ForgeVerbs {
	if t.forge == nil {
		return noForge{}
	}
	return t.forge
}

// noForge is a run whose project has no repository.
type noForge struct{}

func (noForge) Push(context.Context, workspace.PushRequest) (workspace.PushResult, error) {
	return workspace.PushResult{}, workspace.ErrNoForge
}

func (noForge) PR(context.Context, workspace.PRRequest) (forge.PullRequest, error) {
	return forge.PullRequest{}, workspace.ErrNoForge
}

func (noForge) Checks(context.Context, workspace.ChecksRequest) (workspace.ChecksResult, error) {
	return workspace.ChecksResult{}, workspace.ErrNoForge
}

// gitlabForge is the forge verbs over a real GitLab adapter pointed at a
// recorded API, so a spec can assert not only what the socket answered but
// whether the forge was reached at all.
type gitlabForge struct {
	branch string
	client forge.Forge
	api    *recordedGitLab
}

func (g *gitlabForge) Push(ctx context.Context, request workspace.PushRequest) (workspace.PushResult, error) {
	sha, err := g.client.Push(ctx, forge.PushRequest{Git: nullGit{}, Branch: g.branch, Force: request.Force})
	if err != nil {
		return workspace.PushResult{}, err
	}
	return workspace.PushResult{Branch: g.branch, SHA: sha}, nil
}

func (g *gitlabForge) PR(ctx context.Context, request workspace.PRRequest) (forge.PullRequest, error) {
	return g.client.OpenPR(ctx, forge.OpenPRRequest{Branch: g.branch, Title: request.Title, Target: request.Target})
}

func (g *gitlabForge) Checks(ctx context.Context, request workspace.ChecksRequest) (workspace.ChecksResult, error) {
	sha := request.SHA
	if sha == "" {
		sha = "9a1f0c3f2b6d4e5a7c8b9d0e1f2a3b4c5d6e7f80"
	}
	status, err := g.client.Checks(ctx, sha)
	if err != nil {
		return workspace.ChecksResult{}, err
	}
	return workspace.ChecksResult{SHA: sha, Status: status}, nil
}

// nullGit stands in for the workspace's git: the socket specs are about the
// gate, not about what a push does to a checkout.
type nullGit struct{}

func (nullGit) Run(context.Context, map[string]string, ...string) (string, error) {
	return "9a1f0c3f2b6d4e5a7c8b9d0e1f2a3b4c5d6e7f80\n", nil
}

// recordedGitLab is a GitLab that counts every request it receives. Zero is
// the assertion a refusal at the socket needs: the forge was never called.
type recordedGitLab struct {
	server *httptest.Server

	mu       sync.Mutex
	requests int
}

func newRecordedGitLab() *recordedGitLab {
	api := &recordedGitLab{}
	api.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.mu.Lock()
		api.requests++
		api.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.EscapedPath(), "/merge_requests") && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[{"iid":7,"state":"opened","web_url":"https://gitlab.example.test/group/thing/-/merge_requests/7",` +
				`"source_branch":"mill/mill-12","target_branch":"main","sha":"9a1f0c3f2b6d4e5a7c8b9d0e1f2a3b4c5d6e7f80","squash":true}]`))
		case strings.HasSuffix(r.URL.EscapedPath(), "/pipelines"):
			_, _ = w.Write([]byte(`[{"id":1,"sha":"9a1f0c3f2b6d4e5a7c8b9d0e1f2a3b4c5d6e7f80","status":"success"}]`))
		default:
			_, _ = w.Write([]byte(`{"id":42,"path_with_namespace":"group/thing","default_branch":"main","squash_option":"default_on"}`))
		}
	}))
	return api
}

func (a *recordedGitLab) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.requests
}

// forgeVerbs is the tree's forge, over the recorded GitLab.
func (a *recordedGitLab) forgeVerbs(branch string) *gitlabForge {
	repo := forge.Repo{
		Kind:     forge.KindGitLab,
		BaseURL:  a.server.URL,
		Path:     "group/thing",
		CloneURL: a.server.URL + "/group/thing.git",
	}
	minter := forge.MinterFunc(func(context.Context, forge.Step) (forge.Credential, error) {
		return forge.Credential{Username: "mill", Token: "glpat-only-this-step-holds-it"}, nil // gitleaks:allow
	})
	return &gitlabForge{
		branch: branch,
		api:    a,
		client: gitlab.New(repo, minter, gitlab.WithHTTPClient(a.server.Client())),
	}
}
