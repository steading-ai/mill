package github_test

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
	"github.com/ivy/mill/internal/forge/github"
)

// The repository every spec runs against, on the GitHub the stub below
// stands in for.
const (
	repoPath = "octo-org/thing"
	// repoAPI is where the repository's endpoints live under the API root.
	repoAPI  = "/repos/octo-org/thing"
	theToken = "ghp_only_this_step_holds_it" // gitleaks:allow
)

// call is one request the stub received, kept so a spec can assert what
// reached GitHub — and, for a refused request, that nothing did.
type call struct {
	Method string
	Path   string
	Query  string
	Body   string
	Bearer string
}

// stub is a recorded GitHub: routes answered from testdata, every request
// remembered. The adapter talks to api.github.com and nothing else, so the
// stub is reached through a transport that sends that host here rather
// than through a setting the adapter does not have.
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
	status  int
	body    []byte
	headers map[string]string
}

func newStub() *stub {
	s := &stub{routes: map[string]answer{}, handlers: map[string]func(http.ResponseWriter, []byte){}}
	s.server = httptest.NewServer(http.HandlerFunc(s.serve))
	DeferCleanup(s.server.Close)
	return s
}

// serve answers a recorded route, or fails with the route it was not given
// — an unrouted call is a call the adapter should not have made.
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
		Bearer: strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "),
	})
	route, known := s.routes[key]
	handler, handled := s.handlers[key]
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if handled {
		handler(w, body)
		return
	}
	if !known {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"the spec recorded no route for ` + key + `"}`))
		return
	}
	for name, value := range route.headers {
		w.Header().Set(name, value)
	}
	w.WriteHeader(route.status)
	_, _ = w.Write(route.body)
}

// on records the answer to a route, read from a fixture file.
func (s *stub) on(method, path string, status int, fixture string) {
	GinkgoHelper()
	s.onWith(method, path, status, fixture, nil)
}

// onWith is on with response headers, for the permission header GitHub
// puts on a refusal.
func (s *stub) onWith(method, path string, status int, fixture string, headers map[string]string) {
	GinkgoHelper()
	body, err := os.ReadFile(filepath.Join("testdata", fixture))
	Expect(err).NotTo(HaveOccurred())
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes[method+" "+path] = answer{status: status, body: body, headers: headers}
}

// onMerge answers a merge the way GitHub does: the request names the commit
// it merges, and a request naming anything but head is a 409, which is the
// forge saying the branch moved under the mill.
func (s *stub) onMerge(head string) {
	GinkgoHelper()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[http.MethodPut+" "+repoAPI+"/pulls/7/merge"] = func(w http.ResponseWriter, body []byte) {
		var request struct {
			SHA    string `json:"sha"`
			Method string `json:"merge_method"`
		}
		_ = json.Unmarshal(body, &request)
		switch {
		case request.SHA != head:
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"message":"Head branch was modified. Review and try the merge again."}`))
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"sha":"c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00","merged":true,"message":"Pull Request successfully merged"}`))
		}
	}
}

// onPushedBranch stands the stub in for a GitHub catching up with a push.
// The branch reads 404 for the first hidden reads and then exists at head;
// a negative hidden is a push that never landed. A pull request opened
// before the branch is visible is refused the way GitHub refuses it.
func (s *stub) onPushedBranch(name, head string, hidden int, fixture string) {
	GinkgoHelper()
	created, err := os.ReadFile(filepath.Join("testdata", fixture))
	Expect(err).NotTo(HaveOccurred())
	var reads atomic.Int64
	var visible atomic.Bool
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[http.MethodGet+" "+repoAPI+"/branches/"+url.PathEscape(name)] =
		func(w http.ResponseWriter, _ []byte) {
			if hidden >= 0 && reads.Add(1) > int64(hidden) {
				visible.Store(true)
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"name":"` + name + `","commit":{"sha":"` + head + `"}}`))
				return
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Branch not found"}`))
		}
	s.handlers[http.MethodPost+" "+repoAPI+"/pulls"] = func(w http.ResponseWriter, _ []byte) {
		if !visible.Load() {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"message":"Validation Failed","errors":[{"message":"head sha can't be blank"}]}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(created)
	}
}

// onMergeableStates answers repeated reads of one pull request with a
// sequence of `mergeable_state` values, one per read, holding the last
// value once the sequence runs out — the way GitHub settles on an answer
// and keeps it. Everything else about the pull request comes from a
// fixture, decoded and re-encoded with the state substituted in.
func (s *stub) onMergeableStates(fixture string, states ...string) {
	GinkgoHelper()
	body, err := os.ReadFile(filepath.Join("testdata", fixture))
	Expect(err).NotTo(HaveOccurred())
	var decoded map[string]any
	Expect(json.Unmarshal(body, &decoded)).To(Succeed())
	var reads atomic.Int64
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[http.MethodGet+" "+repoAPI+"/pulls/7"] = func(w http.ResponseWriter, _ []byte) {
		i := int(reads.Add(1)) - 1
		if i >= len(states) {
			i = len(states) - 1
		}
		decoded["mergeable_state"] = states[i]
		encoded, err := json.Marshal(decoded)
		Expect(err).NotTo(HaveOccurred())
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(encoded)
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

// repo is the repository the client serves, as forge.ParseRepo reads a
// github.com URL.
func (s *stub) repo() forge.Repo {
	return forge.Repo{
		Kind:     forge.KindGitHub,
		BaseURL:  "https://github.com",
		Path:     repoPath,
		CloneURL: "https://github.com/" + repoPath + ".git",
	}
}

// httpClient reaches the stub for every request the adapter addresses to
// api.github.com.
func (s *stub) httpClient() *http.Client {
	target, err := url.Parse(s.server.URL)
	Expect(err).NotTo(HaveOccurred())
	return &http.Client{Transport: rewriting{target: target, inner: s.server.Client().Transport}}
}

// rewriting sends every request to the stub, whatever host it named.
type rewriting struct {
	target *url.URL
	inner  http.RoundTripper
}

func (t rewriting) RoundTrip(r *http.Request) (*http.Response, error) {
	Expect(r.URL.String()).To(HavePrefix(github.API), "the adapter reaches one host")
	clone := r.Clone(r.Context())
	clone.URL.Scheme = t.target.Scheme
	clone.URL.Host = t.target.Host
	clone.Host = t.target.Host
	return t.inner.RoundTrip(clone)
}

// minter mints the step's credential, counting how often it was asked.
func (s *stub) minter() forge.Minter {
	return forge.MinterFunc(func(_ context.Context, step forge.Step) (forge.Credential, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.minted++
		s.forStep = append(s.forStep, step)
		return forge.Credential{Token: theToken}, nil
	})
}

// recordingGit is the workspace's git, recorded: what the forge asked for
// and what environment it asked for it with.
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

// argv is every argument of every command, joined, so a spec can assert
// that a credential never appears in one.
func (g *recordingGit) argv() string {
	var all []string
	for _, run := range g.runs {
		all = append(all, run.Args...)
	}
	return strings.Join(all, " ")
}
