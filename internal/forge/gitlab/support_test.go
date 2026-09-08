package gitlab_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/forge"
)

// The repository every spec runs against, on a GitLab the stub below stands
// in for.
const (
	repoPath = "group/thing"
	// escaped is how the client addresses the project: its path, URL-encoded,
	// which is what GitLab's API takes in place of a numeric id.
	escaped  = "/api/v4/projects/group%2Fthing"
	theToken = "glpat-only-this-step-holds-it" // gitleaks:allow
)

// call is one request the stub received, kept so a spec can assert what
// reached GitLab — and, for a refused request, that nothing did.
type call struct {
	Method string
	Path   string
	Query  string
	Body   string
	Token  string
}

// stub is a recorded GitLab: routes answered from testdata, every request
// remembered. The fixtures are the API's own shapes, so the mapping this
// package does is exercised against what GitLab actually returns rather than
// against a convenient invention.
type stub struct {
	server *httptest.Server

	mu      sync.Mutex
	routes  map[string]answer
	seen    []call
	minted  int
	forStep []forge.Step
}

type answer struct {
	status int
	body   []byte
}

func newStub() *stub {
	s := &stub{routes: map[string]answer{}}
	s.server = httptest.NewServer(http.HandlerFunc(s.serve))
	DeferCleanup(s.server.Close)
	return s
}

// serve answers a recorded route, or fails the spec with the route it was
// not given — an unrouted call is a call the adapter should not have made.
func (s *stub) serve(w http.ResponseWriter, r *http.Request) {
	body := make([]byte, 0)
	if r.Body != nil {
		buffer := make([]byte, 1<<20)
		n, _ := r.Body.Read(buffer)
		body = buffer[:n]
	}
	key := r.Method + " " + r.URL.EscapedPath()

	s.mu.Lock()
	s.seen = append(s.seen, call{
		Method: r.Method,
		Path:   r.URL.EscapedPath(),
		Query:  r.URL.RawQuery,
		Body:   string(body),
		Token:  r.Header.Get("PRIVATE-TOKEN"),
	})
	route, known := s.routes[key]
	s.mu.Unlock()

	if !known {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"the spec recorded no route for ` + key + `"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(route.status)
	_, _ = w.Write(route.body)
}

// on records the answer to a route, read from a fixture file.
func (s *stub) on(method, path string, status int, fixture string) {
	GinkgoHelper()
	body, err := os.ReadFile(filepath.Join("testdata", fixture))
	Expect(err).NotTo(HaveOccurred())
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes[method+" "+path] = answer{status: status, body: body}
}

// calls is every request the stub received.
func (s *stub) calls() []call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]call(nil), s.seen...)
}

// count is how many requests matched a method and path prefix.
func (s *stub) count(method, path string) int {
	n := 0
	for _, seen := range s.calls() {
		if seen.Method == method && strings.HasPrefix(seen.Path, path) {
			n++
		}
	}
	return n
}

// repo is the repository the client serves: this stub's GitLab.
func (s *stub) repo() forge.Repo {
	return forge.Repo{
		Kind:     forge.KindGitLab,
		BaseURL:  s.server.URL,
		Path:     repoPath,
		CloneURL: s.server.URL + "/" + repoPath + ".git",
	}
}

// minter mints the step's credential, counting how often it was asked. A
// credential is minted per request and never held, so the count is the number
// of calls made.
func (s *stub) minter() forge.Minter {
	return forge.MinterFunc(func(_ context.Context, step forge.Step) (forge.Credential, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.minted++
		s.forStep = append(s.forStep, step)
		return forge.Credential{Username: "mill", Token: theToken}, nil
	})
}

// recordingGit is the workspace's git, recorded: what the forge asked for and
// what environment it asked for it with.
type recordingGit struct {
	runs []gitRun
	head string
}

type gitRun struct {
	Args []string
	Env  map[string]string
}

func (g *recordingGit) Run(_ context.Context, env map[string]string, args ...string) (string, error) {
	g.runs = append(g.runs, gitRun{Args: append([]string(nil), args...), Env: env})
	if len(args) > 0 && args[0] == "rev-parse" {
		return g.head + "\n", nil
	}
	return "", nil
}

// argv is every argument of every command, joined, so a spec can assert that
// a credential never appears in one.
func (g *recordingGit) argv() string {
	var all []string
	for _, run := range g.runs {
		all = append(all, run.Args...)
	}
	return strings.Join(all, " ")
}
