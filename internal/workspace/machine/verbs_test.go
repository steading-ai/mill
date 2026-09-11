package machine_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/forge"
	"github.com/ivy/mill/internal/forge/gitlab"
	"github.com/ivy/mill/internal/workspace"
	"github.com/ivy/mill/internal/workspace/machine"
	"github.com/ivy/mill/internal/workspace/machine/driver"
	"github.com/ivy/mill/internal/workspace/verbs"
)

// recorder is the execer the backend spawns host processes through, with
// every command it was handed on record: what git ran, with what argv, in
// which directory. The processes are real.
type recorder struct {
	mu       sync.Mutex
	commands []workspace.Command
}

func (r *recorder) Exec(ctx context.Context, command workspace.Command) (*workspace.Process, error) {
	r.mu.Lock()
	r.commands = append(r.commands, command)
	r.mu.Unlock()
	return workspace.Spawner{Floor: []string{}}.Exec(ctx, command)
}

// git is every git the recorder saw since it was last cleared.
func (r *recorder) git() []workspace.Command {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []workspace.Command
	for _, command := range r.commands {
		if filepath.Base(command.Path) == "git" {
			out = append(out, command)
		}
	}
	return out
}

func (r *recorder) clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commands = nil
}

// subcommand is the git verb an invocation ran, past `-C <dir>` and any
// `-c key=value` a credential helper or the signing configuration adds.
func subcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-C", "-c":
			i++
		default:
			return args[i]
		}
	}
	return ""
}

var _ = Describe("The machine workspace's git and forge verbs", Label("unit"), func() {
	const (
		branch    = "mill/mill-81"
		theBearer = "the-live-step-token"
		forgeKey  = "glpat-the-worker-alone-holds-this" // gitleaks:allow
	)

	var (
		f        *fixture
		bare     string
		execs    *recorder
		ws       *machine.Workspace
		handle   *workspace.Handle
		checkout string
	)

	BeforeEach(func(ctx context.Context) {
		// The forge's repository as a bare repository on disk, so the push
		// lands on a real ref, and GitLab's API as an httptest server that
		// answers about that repository's branch.
		bare = filepath.Join(GinkgoT().TempDir(), "thing.git")
		git(GinkgoT().TempDir(), "clone", "--quiet", "--bare", origin(nil), bare)
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case strings.HasSuffix(r.URL.EscapedPath(), "/merge_requests") && r.Method == http.MethodGet:
				_, _ = w.Write([]byte(`[]`))
			case strings.HasSuffix(r.URL.EscapedPath(), "/merge_requests") && r.Method == http.MethodPost:
				_, _ = w.Write([]byte(`{"iid":7,"state":"opened","web_url":"https://gitlab.example.test/group/thing/-/merge_requests/7",` +
					`"source_branch":"` + branch + `","target_branch":"main"}`))
			case strings.Contains(r.URL.EscapedPath(), "/repository/branches/"):
				head, err := exec.Command("git", "--git-dir="+bare, "rev-parse", "refs/heads/"+branch).Output()
				if err != nil {
					w.WriteHeader(http.StatusNotFound)
					_, _ = w.Write([]byte(`{"message":"404 Branch Not Found"}`))
					return
				}
				_, _ = w.Write([]byte(`{"name":"` + branch + `","commit":{"id":"` + strings.TrimSpace(string(head)) + `"}}`))
			case strings.HasSuffix(r.URL.EscapedPath(), "/pipelines"):
				_, _ = w.Write([]byte(`[{"id":1,"status":"success"}]`))
			default:
				_, _ = w.Write([]byte(`{"id":42,"path_with_namespace":"group/thing","default_branch":"main"}`))
			}
		}))
		DeferCleanup(api.Close)
		client := gitlab.New(forge.Repo{Kind: forge.KindGitLab, BaseURL: api.URL, Path: "group/thing", CloneURL: bare},
			forge.Static("mill", forgeKey), gitlab.WithHTTPClient(api.Client()))

		f = newFixture(bare)
		execs = &recorder{}
		f.config.Forge = client
		f.config.Execer = execs
		f.config.SigningDir = filepath.Join(GinkgoT().TempDir(), "signing")
		ws = f.workspace()
		made, err := ws.Create(ctx, workspace.Spec{RunID: runID, NodeRef: "mill#81", Origin: bare})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) { _ = ws.Destroy(ctx) })
		handle = made
		checkout = filepath.Join(f.runDir(), "workspace")
		Expect(handle.Root).To(Equal(checkout))
		Expect(ws.Rotate(ctx, workspace.TokenFor(theBearer, workspace.Read, workspace.Write, workspace.Exec, workspace.Forge))).To(Succeed())
		execs.clear()
	})

	// Scenario: The verbs never enter the machine
	//   Given the fake driver
	//   When every git and forge verb runs
	//   Then each is a host-side git invocation against the run's workspace directory
	//   And none is a helper exec
	It("The verbs never enter the machine", func(ctx context.Context) {
		By("given the fake driver, with the machine's execs counted")
		execsBefore := len(f.host.CallsTo(driver.Exec))

		By("when every git and forge verb runs")
		// A session's file, as the idmapped bind delivers it: uid 1000
		// inside is the worker outside, so a write to the workspace
		// directory on the host is what a write in /work is.
		Expect(os.WriteFile(filepath.Join(checkout, "NOTES.md"), []byte("worked\n"), 0o600)).To(Succeed())

		status, err := ws.Git().Status(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(status.Branch).To(Equal(branch))
		Expect(status.Clean).To(BeFalse())
		Expect(status.Files).To(ConsistOf(workspace.FileStatus{Code: "??", Path: "NOTES.md"}))

		diff, err := ws.Git().Diff(ctx, workspace.DiffRequest{})
		Expect(err).NotTo(HaveOccurred())
		Expect(diff).To(BeEmpty(), "nothing is staged yet")

		committed, err := ws.Git().Commit(ctx, workspace.CommitRequest{Message: "mill#81: worked"})
		Expect(err).NotTo(HaveOccurred())
		Expect(committed.SHA).To(HaveLen(40))
		Expect(committed.Author).To(Equal(ws.Committer().String()))

		log, err := ws.Git().Log(ctx, workspace.LogRequest{Limit: 1})
		Expect(err).NotTo(HaveOccurred())
		Expect(log).To(HaveLen(1))
		Expect(log[0].SHA).To(Equal(committed.SHA))

		pushed, err := ws.Forge().Push(ctx, workspace.PushRequest{})
		Expect(err).NotTo(HaveOccurred())
		Expect(pushed).To(Equal(workspace.PushResult{Branch: branch, SHA: committed.SHA, Remote: bare}))
		out, err := exec.Command("git", "--git-dir="+bare, "rev-parse", "refs/heads/"+branch).Output()
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(string(out))).To(Equal(committed.SHA), "the push landed on the forge's repository")

		pr, err := ws.Forge().PR(ctx, workspace.PRRequest{Title: "mill#81"})
		Expect(err).NotTo(HaveOccurred())
		Expect(pr.Number).To(Equal(7))

		checks, err := ws.Forge().Checks(ctx, workspace.ChecksRequest{})
		Expect(err).NotTo(HaveOccurred())
		Expect(checks).To(Equal(workspace.ChecksResult{SHA: committed.SHA, Status: forge.Success}))

		By("then each is a host-side git invocation against the run's workspace directory")
		seen := map[string]bool{}
		invocations := execs.git()
		Expect(invocations).NotTo(BeEmpty())
		for _, command := range invocations {
			Expect(command.Args[:2]).To(Equal([]string{"-C", checkout}), "git %s ran against %v", subcommand(command.Args), command.Args)
			Expect(command.Dir).To(Equal(checkout))
			seen[subcommand(command.Args)] = true
		}
		for _, verb := range []string{"status", "diff", "log", "add", "commit", "push", "rev-parse"} {
			Expect(seen).To(HaveKey(verb), "the seven verbs are git on the host")
		}

		By("and none is a helper exec")
		Expect(f.host.CallsTo(driver.Exec)).To(HaveLen(execsBefore), "no exec entered the machine")
		Expect(f.host.CallsTo(driver.StartUnit)).To(HaveLen(1))
	})

	It("signs the commit with the mill's key on the host, under the identity the machine's environment names", func(ctx context.Context) {
		Expect(os.WriteFile(filepath.Join(checkout, "NOTES.md"), []byte("worked\n"), 0o600)).To(Succeed())
		committed, err := ws.Git().Commit(ctx, workspace.CommitRequest{Message: "mill#81: signed"})
		Expect(err).NotTo(HaveOccurred())

		key := ws.SigningKey()
		Expect(key).NotTo(BeNil())
		Expect(key.Path).To(HavePrefix(f.config.SigningDir), "the key is the account's, beside no run")
		verify := exec.Command("git", "-c", "gpg.ssh.allowedSignersFile="+key.AllowedSigners, "verify-commit", committed.SHA)
		verify.Dir = checkout
		verify.Env = gitEnv()
		out, err := verify.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "git verify-commit: %s", out)
		Expect(string(out)).To(ContainSubstring("Good \"git\" signature for " + f.config.Identity))

		identity := ws.Committer()
		Expect(identity).To(Equal(workspace.Committer{Name: "mill", Email: "mill@core"}))
		Expect(committed.Author).To(Equal(identity.String()))
		env := ws.Environment()
		Expect(env).To(HaveKeyWithValue("GIT_AUTHOR_NAME", identity.Name))
		Expect(env).To(HaveKeyWithValue("GIT_AUTHOR_EMAIL", identity.Email))
		Expect(env).To(HaveKeyWithValue("GIT_COMMITTER_NAME", identity.Name))
		Expect(env).To(HaveKeyWithValue("GIT_COMMITTER_EMAIL", identity.Email))
		for name := range env {
			Expect(name).NotTo(HavePrefix("GIT_CONFIG"), "no host path reaches inside: the environment names no configuration file")
		}
		Expect(env).NotTo(HaveKey("GIT_SSH_COMMAND"))
	})

	It("refuses to push a commit a session made on its own inside, which the key never reached", func(ctx context.Context) {
		By("given a commit made inside the machine, under the identity the environment names and with no key")
		Expect(os.WriteFile(filepath.Join(checkout, "NOTES.md"), []byte("worked\n"), 0o600)).To(Succeed())
		inside := func(args ...string) string {
			GinkgoHelper()
			command := exec.Command("git", args...)
			command.Dir = checkout
			command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + checkout, "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_SYSTEM=" + os.DevNull}
			for name, value := range ws.Environment() {
				command.Env = append(command.Env, name+"="+value)
			}
			out, err := command.CombinedOutput()
			Expect(err).NotTo(HaveOccurred(), "git %s: %s", strings.Join(args, " "), out)
			return strings.TrimSpace(string(out))
		}
		inside("add", "NOTES.md")
		inside("commit", "--quiet", "--message", "mill#81: a stray commit")
		Expect(inside("show", "--no-patch", "--format=%an <%ae>", "HEAD")).To(Equal(ws.Committer().String()), "attributed to the mill")
		Expect(inside("show", "--no-patch", "--format=%G?", "HEAD")).To(Equal("N"), "and unsigned")

		By("when the workspace pushes")
		_, err := ws.Forge().Push(ctx, workspace.PushRequest{})

		By("then the push is refused, naming the commit")
		Expect(err).To(MatchError(verbs.ErrUnsigned))
		Expect(err).To(MatchError(ContainSubstring(inside("rev-parse", "HEAD"))))
		_, statErr := os.Stat(filepath.Join(bare, "refs", "heads", "mill"))
		Expect(statErr).To(MatchError(os.ErrNotExist), "nothing reached the forge")
		for _, command := range execs.git() {
			Expect(subcommand(command.Args)).NotTo(Equal("push"))
		}
	})

	It("answers every verb with ErrNotCreated before Create", func(ctx context.Context) {
		fresh := f.workspace()
		_, err := fresh.Git().Status(ctx)
		Expect(err).To(MatchError(machine.ErrNotCreated))
		_, err = fresh.Git().Commit(ctx, workspace.CommitRequest{Message: "x"})
		Expect(err).To(MatchError(machine.ErrNotCreated))
		_, err = fresh.Forge().Push(ctx, workspace.PushRequest{})
		Expect(err).To(MatchError(machine.ErrNotCreated))
		_, err = fresh.GitRunner().Run(ctx, nil, "status")
		Expect(err).To(MatchError(machine.ErrNotCreated))
	})
})
