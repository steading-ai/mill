package socket_test

import (
	"context"
	"net"
	"sync"

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
