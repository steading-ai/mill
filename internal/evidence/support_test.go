package evidence_test

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/mill/internal/evidence"
	"github.com/steading-ai/mill/internal/forge"
	"github.com/steading-ai/mill/internal/harness"
	"github.com/steading-ai/mill/internal/harness/fake"
)

// repository is a real git repository the commit verifier is pointed at.
// Real git, because the whole of what the verifier does is ask git two
// questions, and a double that answers them is a spec about the double.
type repository struct {
	dir   string
	calls [][]string
	mu    sync.Mutex
}

// millIdentity is the identity the fixture commits under unless a spec asks
// for another. It is pinned in the environment rather than left to the
// repository's config, because git exports GIT_AUTHOR_* and GIT_COMMITTER_*
// to the hooks it runs, and a spec run from a commit hook would otherwise
// make its commits under whoever is committing.
var millIdentity = map[string]string{
	"GIT_AUTHOR_NAME":     "mill",
	"GIT_AUTHOR_EMAIL":    "mill@example.test",
	"GIT_COMMITTER_NAME":  "mill",
	"GIT_COMMITTER_EMAIL": "mill@example.test",
}

func newRepository(branch string) *repository {
	repo := &repository{dir: GinkgoT().TempDir()}
	repo.must("init", "--initial-branch", branch)
	repo.must("config", "user.name", "mill")
	repo.must("config", "user.email", "mill@example.test")
	return repo
}

// Run is the git the verifier is given: one command in the checkout, with
// every invocation recorded so a spec can assert what was asked.
func (r *repository) Run(ctx context.Context, env map[string]string, args ...string) (string, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string(nil), args...))
	r.mu.Unlock()
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // a spec's own arguments
	cmd.Dir = r.dir
	cmd.Env = scratchEnv("GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	for name, value := range millIdentity {
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	for name, value := range env {
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// scratchEnv is the environment for the fixture's own git, with the host's
// GIT_* variables cleared: under a git hook the process inherits GIT_DIR and
// GIT_INDEX_FILE, and this git would otherwise act on the hook's repository
// rather than the fixture, whatever cmd.Dir says.
func scratchEnv(extra ...string) []string {
	env := os.Environ()
	clean := make([]string, 0, len(env)+len(extra))
	for _, kv := range env {
		if !strings.HasPrefix(kv, "GIT_") {
			clean = append(clean, kv)
		}
	}
	return append(clean, extra...)
}

var _ evidence.Git = (*repository)(nil)

// asked reports whether git was ever run with an argument containing text.
func (r *repository) asked(text string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, call := range r.calls {
		if strings.Contains(strings.Join(call, " "), text) {
			return true
		}
	}
	return false
}

// must runs git and fails the spec if it does not succeed.
func (r *repository) must(args ...string) string {
	out, err := r.Run(context.Background(), nil, args...)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), out)
	return out
}

// commit writes a file and commits it, returning the new commit's SHA.
func (r *repository) commit(name, content string) string {
	Expect(os.WriteFile(filepath.Join(r.dir, name), []byte(content), 0o600)).To(Succeed())
	r.must("add", name)
	r.must("commit", "--message", "add "+name)
	return strings.TrimSpace(r.must("rev-parse", "HEAD"))
}

// write leaves a file in the working tree without staging or committing it,
// the way a session that edited something and never committed would.
func (r *repository) write(name, content string) {
	Expect(os.WriteFile(filepath.Join(r.dir, name), []byte(content), 0o600)).To(Succeed())
}

// commitAs writes a file and commits it under an identity of the spec's
// choosing, which is how a commit that arrived on the branch by some other
// hand than the mill's is staged.
func (r *repository) commitAs(name, email, file, content string) string {
	Expect(os.WriteFile(filepath.Join(r.dir, file), []byte(content), 0o600)).To(Succeed())
	r.must("add", file)
	out, err := r.Run(context.Background(), map[string]string{
		"GIT_AUTHOR_NAME":     name,
		"GIT_AUTHOR_EMAIL":    email,
		"GIT_COMMITTER_NAME":  name,
		"GIT_COMMITTER_EMAIL": email,
	}, "commit", "--message", "add "+file)
	Expect(err).NotTo(HaveOccurred(), out)
	return strings.TrimSpace(r.must("rev-parse", "HEAD"))
}

// fixtureForge answers the two questions the `url` and `status` verifiers
// ask, from a fixture rather than a network.
type fixtureForge struct {
	pr        forge.PullRequest
	prErr     error
	status    forge.Status
	statusErr error
	asked     []string
}

func (f *fixtureForge) PR(_ context.Context, branch string) (forge.PullRequest, error) {
	f.asked = append(f.asked, "pr "+branch)
	return f.pr, f.prErr
}

func (f *fixtureForge) Checks(_ context.Context, sha string) (forge.Status, error) {
	f.asked = append(f.asked, "checks "+sha)
	return f.status, f.statusErr
}

var _ evidence.Forge = (*fixtureForge)(nil)

// fixtureArtifacts is dagger's artifact store, holding one digest.
type fixtureArtifacts struct {
	held string
	err  error
}

func (a fixtureArtifacts) Accepted(_ context.Context, digest string) (bool, error) {
	return digest == a.held, a.err
}

// fixtureGates is dagger, holding one gate node.
type fixtureGates struct {
	node evidence.GateNode
	err  error
}

func (g fixtureGates) Node(_ context.Context, id string) (evidence.GateNode, error) {
	if g.err != nil {
		return evidence.GateNode{}, g.err
	}
	node := g.node
	node.ID = id
	return node, nil
}

// transcript is a session transcript on disk that records every attempt to
// open it. A verifier that reads a transcript would show up here, which is
// the point: "nothing in a transcript is evidence" is a rule worth being
// able to check.
type transcript struct {
	path  string
	opens int
}

func newTranscript(content string) *transcript {
	path := filepath.Join(GinkgoT().TempDir(), "transcript.jsonl")
	Expect(os.WriteFile(path, []byte(content), 0o600)).To(Succeed())
	return &transcript{path: path}
}

func (t *transcript) evidence() evidence.Transcript {
	return evidence.Transcript{
		Path: t.path,
		Open: func(context.Context) (io.ReadCloser, error) {
			t.opens++
			return os.Open(t.path)
		},
	}
}

// session runs the fake harness and returns what it produced, so that every
// spec about a session's output takes that output the way the worker does —
// off a terminal record — rather than by writing the map by hand.
func session(final string, output map[string]any) evidence.Session {
	GinkgoHelper()
	agent := fake.New(fake.Script{Terminal: fake.Terminal(final, output, nil)})
	turn, err := agent.Start(context.Background(), harness.Session{ID: "s"}, harness.Prompt{}, nil)
	Expect(err).NotTo(HaveOccurred())
	terminal, err := turn.Wait(context.Background())
	Expect(err).NotTo(HaveOccurred())
	return evidence.Session{FinalMessage: terminal.FinalMessage, Output: terminal.StructuredOutput}
}
