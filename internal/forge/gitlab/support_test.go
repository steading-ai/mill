package gitlab_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

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

	mu       sync.Mutex
	routes   map[string]answer
	handlers map[string]func(http.ResponseWriter, []byte)
	seen     []call
	minted   int
	forStep  []forge.Step
}

type answer struct {
	status int
	body   []byte
}

func newStub() *stub {
	s := &stub{routes: map[string]answer{}, handlers: map[string]func(http.ResponseWriter, []byte){}}
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
	handler, handled := s.handlers[key]
	s.mu.Unlock()

	if handled {
		w.Header().Set("Content-Type", "application/json")
		handler(w, body)
		return
	}
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

// onMerge answers a merge the way GitLab 19 does: the request must name the
// commit it merges, and it must be the branch's head. A merge with no `sha`
// is `400 SHA must be provided when merging`; one naming anything but head
// is a 409, which is the forge saying the branch moved under the mill.
func (s *stub) onMerge(path, head, fixture string) {
	GinkgoHelper()
	merged, err := os.ReadFile(filepath.Join("testdata", fixture))
	Expect(err).NotTo(HaveOccurred())
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[http.MethodPut+" "+path] = func(w http.ResponseWriter, body []byte) {
		var request struct {
			SHA string `json:"sha"`
		}
		_ = json.Unmarshal(body, &request)
		switch {
		case request.SHA == "":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message":"SHA must be provided when merging"}`))
		case request.SHA != head:
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"message":"SHA does not match HEAD of source branch"}`))
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(merged)
		}
	}
}

// onPushedBranch stands the stub in for a GitLab catching up with a push.
// The branch reads 404 for the first hidden reads and then exists at head; a
// negative hidden is a push that never landed, and the branch never appears.
//
// The merge requests endpoint refuses a merge request opened before the
// branch is visible the way GitLab does — `400 source_branch does not exist`
// — so a merge request created too early fails the spec where it is created
// rather than only at an assertion.
func (s *stub) onPushedBranch(name, head string, hidden int, fixture string) {
	GinkgoHelper()
	created, err := os.ReadFile(filepath.Join("testdata", fixture))
	Expect(err).NotTo(HaveOccurred())
	var reads atomic.Int64
	var visible atomic.Bool
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[http.MethodGet+" "+escaped+"/repository/branches/"+url.PathEscape(name)] =
		func(w http.ResponseWriter, _ []byte) {
			if hidden >= 0 && reads.Add(1) > int64(hidden) {
				visible.Store(true)
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"name":"` + name + `","commit":{"id":"` + head + `"}}`))
				return
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"404 Branch Not Found"}`))
		}
	s.handlers[http.MethodPost+" "+escaped+"/merge_requests"] = func(w http.ResponseWriter, _ []byte) {
		if !visible.Load() {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message":{"source_branch":["does not exist"]}}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(created)
	}
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
