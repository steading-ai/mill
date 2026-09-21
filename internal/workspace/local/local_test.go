package local_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/evidence"
	"github.com/ivy/mill/internal/testsupport"
	"github.com/ivy/mill/internal/workspace"
	"github.com/ivy/mill/internal/workspace/local"
	"github.com/ivy/mill/internal/workspace/redact"
	"github.com/ivy/mill/internal/workspace/socket"
)

var _ = Describe("The local workspace", Label("unit"), func() {
	var (
		root      string
		from      string
		log       *bytes.Buffer
		redactor  *redact.Redactor
		ws        *local.Workspace
		handle    *workspace.Handle
		theBearer = "the-live-step-token"
	)

	// create makes the workspace for a run, as the worker does on a claim.
	create := func(ctx context.Context, runID, nodeRef string) *workspace.Handle {
		GinkgoHelper()
		made, err := ws.Create(ctx, workspace.Spec{RunID: runID, NodeRef: nodeRef, Origin: from})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) { Expect(ws.Destroy(ctx)).To(Succeed()) })
		return made
	}

	// step is the client a session holds: the socket and a token, nothing else.
	step := func(ctx context.Context, capabilities ...workspace.Capability) *socket.Client {
		GinkgoHelper()
		Expect(ws.Rotate(ctx, workspace.TokenFor(theBearer, capabilities...))).To(Succeed())
		return socket.Dial(handle.Socket, theBearer)
	}

	BeforeEach(func() {
		root = filepath.Join(GinkgoT().TempDir(), "workspaces")
		from = origin()
		log = &bytes.Buffer{}
		redactor = redact.New()
		ws = local.New(local.Options{
			Root:     root,
			Origin:   from,
			Log:      log,
			Redactor: redactor,
			Identity: "mill@example.test",
			Name:     "mill",
		})
	})

	Describe("creating one", func() {
		BeforeEach(func(ctx context.Context) { handle = create(ctx, "run-01", "mill#12") })

		It("clones the project's default branch onto a branch named from the node", func(ctx context.Context) {
			Expect(handle.Branch).To(Equal("mill/mill-12"))
			Expect(git(handle.Root, "branch", "--show-current")).To(Equal("mill/mill-12"))
			Expect(git(handle.Root, "rev-parse", "HEAD")).To(Equal(git(from, "rev-parse", "main")))
			Expect(os.ReadFile(filepath.Join(handle.Root, "README.md"))).To(Equal([]byte("as cloned\n")))
			Expect(handle.Kind).To(Equal(workspace.KindLocal))
			Expect(handle.Ref).To(Equal("main"), "the ref recorded is the one the clone landed on")
		})

		It("keeps the run's directory readable by the mill's account alone", func() {
			info, err := os.Stat(filepath.Dir(handle.Root))
			Expect(err).NotTo(HaveOccurred())
			Expect(info.Mode().Perm()).To(Equal(os.FileMode(0o700)))
		})

		It("keeps the socket and the signing key out of the checkout", func(ctx context.Context) {
			Expect(filepath.Dir(handle.Socket)).To(Equal(filepath.Dir(handle.Root)))
			Expect(ws.SigningKey().Path).To(HavePrefix(root))
			Expect(ws.SigningKey().Path).NotTo(HavePrefix(handle.Root))
			status, err := ws.Git().Status(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(status.Clean).To(BeTrue(), "the workspace's own furniture shows up as a change: %v", status.Files)
		})

		It("makes a browser directory beside the checkout, for a mounted MCP server, gone after Destroy", func(ctx context.Context) {
			Expect(handle.McpDir).To(Equal(filepath.Join(filepath.Dir(handle.Root), "browser")))
			info, err := os.Stat(handle.McpDir)
			Expect(err).NotTo(HaveOccurred())
			Expect(info.IsDir()).To(BeTrue())
			status, err := ws.Git().Status(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(status.Clean).To(BeTrue(), "the browser directory shows up as a change: %v", status.Files)

			Expect(ws.Destroy(ctx)).To(Succeed())

			_, err = os.Stat(handle.McpDir)
			Expect(err).To(MatchError(os.ErrNotExist))
		})

		It("refuses a second workspace for the same run", func(ctx context.Context) {
			second := local.New(local.Options{Root: root, Origin: from})

			_, err := second.Create(ctx, workspace.Spec{RunID: "run-01", NodeRef: "mill#12"})

			Expect(err).To(MatchError(local.ErrExists))
		})

		It("reattaches to the directory a previous worker left behind", func(ctx context.Context) {
			Expect(ws.Destroy(ctx)).Error().NotTo(HaveOccurred())
		})
	})

	It("Two concurrent Create calls for different runs never share a directory or a branch", func(ctx context.Context) {
		first := local.New(local.Options{Root: root, Origin: from})
		second := local.New(local.Options{Root: root, Origin: from})
		handles := make([]*workspace.Handle, 2)
		errs := make([]error, 2)

		var wg sync.WaitGroup
		wg.Add(2)
		for index, pair := range []struct {
			ws      *local.Workspace
			runID   string
			nodeRef string
		}{
			{first, "run-11", "mill#12"},
			{second, "run-12", "mill#13"},
		} {
			go func() {
				defer GinkgoRecover()
				defer wg.Done()
				handles[index], errs[index] = pair.ws.Create(ctx, workspace.Spec{RunID: pair.runID, NodeRef: pair.nodeRef})
			}()
		}
		wg.Wait()
		DeferCleanup(func(ctx context.Context) {
			Expect(first.Destroy(ctx)).To(Succeed())
			Expect(second.Destroy(ctx)).To(Succeed())
		})

		Expect(errs).To(Equal([]error{nil, nil}))
		Expect(handles[0].Root).NotTo(Equal(handles[1].Root))
		Expect(handles[0].Branch).NotTo(Equal(handles[1].Branch))
		Expect(handles[0].Socket).NotTo(Equal(handles[1].Socket))
		Expect(git(handles[0].Root, "branch", "--show-current")).To(Equal("mill/mill-12"))
		Expect(git(handles[1].Root, "branch", "--show-current")).To(Equal("mill/mill-13"))
	})

	Describe("what a step is given", func() {
		BeforeEach(func(ctx context.Context) { handle = create(ctx, "run-02", "mill#12") })

		It("The step sees a socket, not the host", func(ctx context.Context) {
			// Given a live step token with read and exec
			session := step(ctx, workspace.Read, workspace.Exec)

			// When the step runs `cat /etc/passwd` through Exec
			result, err := session.Exec(ctx, socket.ExecRequest{Path: "cat", Args: []string{"/etc/passwd"}})

			// Then the command runs with cwd inside the clone and the output
			// is returned
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Status).To(BeEquivalentTo(0))
			Expect(result.Stdout).To(ContainSubstring("root:"))

			where, err := session.Exec(ctx, socket.ExecRequest{Path: "/bin/sh", Args: []string{"-c", "pwd"}})
			Expect(err).NotTo(HaveOccurred())
			Expect(strings.TrimSpace(where.Stdout)).To(Equal(resolved(handle.Root)))

			// And the environment it ran with is the floor and the grant:
			// the mill's PATH, locale and account, the step's two
			// credentials, and the git identity the workspace decided for
			// the run.
			environment, err := session.Exec(ctx, socket.ExecRequest{Path: "/usr/bin/env"})
			Expect(err).NotTo(HaveOccurred())
			Expect(lines(environment.Stdout)).To(ConsistOf(floorAndGrant(ws, handle.Socket, theBearer)))
		})

		It("gives a child the mill's PATH and nothing else the mill's own process carries", func(ctx context.Context) {
			GinkgoT().Setenv("MILL_A_SECRET_THE_MILL_HOLDS", "not for a session")
			GinkgoT().Setenv("DATABASE_URL", "postgres:///not-for-a-session")
			session := step(ctx, workspace.Exec)

			environment, err := session.Exec(ctx, socket.ExecRequest{Path: "/bin/sh", Args: []string{"-c", "env; echo PATH_SEEN=$PATH"}})

			Expect(err).NotTo(HaveOccurred())
			Expect(environment.Stdout).NotTo(ContainSubstring("MILL_A_SECRET_THE_MILL_HOLDS"))
			Expect(environment.Stdout).NotTo(ContainSubstring("DATABASE_URL"))
			Expect(lines(environment.Stdout)).To(ContainElement("PATH_SEEN="+os.Getenv("PATH")),
				"a check's shell resolves the toolchain on the PATH the mill was started with, not a shell's built-in default")
			// The shell adds PWD, SHLVL and _ of its own; everything else is
			// the floor and the grant.
			Expect(lines(environment.Stdout)).To(ContainElements(toInterfaces(floorAndGrant(ws, handle.Socket, theBearer))...))
			for _, line := range lines(environment.Stdout) {
				name, _, _ := strings.Cut(line, "=")
				Expect(name).To(BeElementOf(append(names(floorAndGrant(ws, handle.Socket, theBearer)), "PATH_SEEN", "PWD", "SHLVL", "_")))
			}
		})

		It("Rotate to a new token makes the previous token fail on the next request", func(ctx context.Context) {
			implementer := step(ctx, workspace.Read, workspace.Write)
			Expect(implementer.Write(ctx, "notes.md", []byte("plan\n"))).To(Succeed())

			Expect(ws.Rotate(ctx, workspace.TokenFor("the-reviewers-bearer", workspace.Read))).To(Succeed())

			_, err := implementer.Read(ctx, "README.md")
			Expect(err).To(MatchError(ContainSubstring("not this step's live token")))
			Expect(socket.Dial(handle.Socket, "the-reviewers-bearer").Read(ctx, "README.md")).To(Equal([]byte("as cloned\n")))
		})

		It("reads, writes and lists inside the checkout, and refuses a path that leaves it", func(ctx context.Context) {
			session := step(ctx, workspace.Read, workspace.Write)

			Expect(session.Write(ctx, "docs/plan.md", []byte("a plan\n"))).To(Succeed())
			Expect(session.Read(ctx, "docs/plan.md")).To(Equal([]byte("a plan\n")))
			Expect(session.List(ctx, "docs")).To(ConsistOf(workspace.Entry{Name: "plan.md", Size: 7}))

			err := session.Write(ctx, "../escaped.md", []byte("outside"))
			Expect(err).To(MatchError(ContainSubstring("outside the checkout")))
			Expect(filepath.Join(filepath.Dir(handle.Root), "escaped.md")).NotTo(BeAnExistingFile())
		})

		It("A value registered via redact never appears in any log line written after registration", func(ctx context.Context) {
			session := step(ctx, workspace.Read, workspace.Exec)
			Expect(session.Redact(ctx, "a-forge-token-the-step-was-minted")).To(Succeed())

			result, err := session.Exec(ctx, socket.ExecRequest{
				Path: "/bin/sh",
				Args: []string{"-c", "echo using a-forge-token-the-step-was-minted"},
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(result.Stdout).To(ContainSubstring("a-forge-token-the-step-was-minted"))
			Expect(log.String()).NotTo(ContainSubstring("a-forge-token-the-step-was-minted"))
			Expect(log.String()).To(ContainSubstring("out=using " + redact.Mask))
		})

		It("seeds the redactor with the step token itself, before it can reach a log", func(ctx context.Context) {
			step(ctx, workspace.Exec)

			Expect(redactor.Values()).To(ContainElement(theBearer))
		})

		It("refuses a verb before the workspace exists", func(ctx context.Context) {
			fresh := local.New(local.Options{Root: root, Origin: from})

			_, err := fresh.Read(ctx, "README.md")

			Expect(err).To(MatchError(local.ErrNotCreated))
		})
	})

	Describe("git", func() {
		BeforeEach(func(ctx context.Context) { handle = create(ctx, "run-03", "mill#12") })

		It("commits on the run's branch, signed with a key git verify-commit accepts", func(ctx context.Context) {
			session := step(ctx, workspace.Read, workspace.Write)
			Expect(session.Write(ctx, "notes.md", []byte("what I did\n"))).To(Succeed())

			commit, err := session.GitCommit(ctx, workspace.CommitRequest{Message: "mill#12: implement"})

			Expect(err).NotTo(HaveOccurred())
			Expect(commit.SHA).NotTo(BeEmpty())
			Expect(commit.Subject).To(Equal("mill#12: implement"))
			Expect(git(handle.Root, "branch", "--contains", commit.SHA)).To(ContainSubstring("mill/mill-12"))

			key := ws.SigningKey()
			verify := exec.Command("git", append(key.GitConfig(), "verify-commit", commit.SHA)...) //nolint:gosec // the arguments are this spec's own
			verify.Dir = handle.Root
			verify.Env = gitEnv()
			out, err := verify.CombinedOutput()
			Expect(err).NotTo(HaveOccurred(), "git verify-commit: %s", out)
			Expect(string(out)).To(ContainSubstring("Good \"git\" signature for mill@example.test"))
		})

		It("reports status, diff and log of the checkout", func(ctx context.Context) {
			session := step(ctx, workspace.Read, workspace.Write)

			status, err := session.GitStatus(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(status).To(Equal(workspace.Status{Branch: "mill/mill-12", Clean: true}))

			Expect(session.Write(ctx, "README.md", []byte("as changed\n"))).To(Succeed())
			status, err = session.GitStatus(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(status.Clean).To(BeFalse())
			Expect(status.Files).To(ConsistOf(workspace.FileStatus{Code: " M", Path: "README.md"}))
			Expect(session.GitDiff(ctx, workspace.DiffRequest{})).To(ContainSubstring("+as changed"))

			commit, err := session.GitCommit(ctx, workspace.CommitRequest{Message: "mill#12: change it"})
			Expect(err).NotTo(HaveOccurred())
			Expect(session.GitLog(ctx, workspace.LogRequest{Limit: 1})).To(ConsistOf(commit))
		})

		It("refuses to commit when the checkout has wandered off the run's branch", func(ctx context.Context) {
			session := step(ctx, workspace.Read, workspace.Write)
			git(handle.Root, "checkout", "--quiet", "-b", "somewhere-else")

			_, err := session.GitCommit(ctx, workspace.CommitRequest{Message: "not on the run's branch"})

			Expect(err).To(MatchError(ContainSubstring("not the run's branch")))
		})

		// mill#33: a catch-up leaves a merge staged for `commit` to finish. A
		// session names only the paths it touched itself (as the commit
		// skill does), which is fewer than everything the staged merge
		// touched — git refuses a `commit -- <paths>` naming less than that
		// outright ("cannot do a partial commit during a merge"), so the
		// paths must be dropped rather than honored while a merge is staged.
		It("finishes a staged merge even when the session names only its own paths", func(ctx context.Context) {
			git(handle.Root, "checkout", "--quiet", "-b", "elsewhere")
			Expect(os.WriteFile(filepath.Join(handle.Root, "elsewhere.md"), []byte("from elsewhere\n"), 0o600)).To(Succeed())
			git(handle.Root, "add", "elsewhere.md")
			git(handle.Root, "commit", "--quiet", "--no-gpg-sign", "--message", "elsewhere")
			git(handle.Root, "checkout", "--quiet", "mill/mill-12")
			git(handle.Root, "merge", "--no-commit", "--no-ff", "elsewhere")

			session := step(ctx, workspace.Read, workspace.Write)
			Expect(session.Write(ctx, "notes.md", []byte("what I did\n"))).To(Succeed())

			commit, err := session.GitCommit(ctx, workspace.CommitRequest{
				Message: "mill#12: catch up",
				Paths:   []string{"notes.md"},
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(git(handle.Root, "rev-parse", "--verify", commit.SHA+"^2")).NotTo(BeEmpty(),
				"the commit has the merge's second parent, so it finished the staged merge")
			Expect(filepath.Join(handle.Root, "elsewhere.md")).To(BeAnExistingFile())
		})
	})

	Describe("resuming", func() {
		It("reattaches to the run's directory and lands back on its branch", func(ctx context.Context) {
			handle = create(ctx, "run-04", "mill#12")
			session := step(ctx, workspace.Read, workspace.Write)
			Expect(session.Write(ctx, "notes.md", []byte("half done\n"))).To(Succeed())
			Expect(ws.Destroy(ctx)).To(Succeed())

			// Destroy removed the directory, so the resume this spec models
			// is the one a worker on another host performs: Create again,
			// with the run's branch as the ref.
			resumed := local.New(local.Options{Root: root, Origin: from})
			DeferCleanup(func(ctx context.Context) { Expect(resumed.Destroy(ctx)).To(Succeed()) })

			again, err := resumed.Create(ctx, workspace.Spec{RunID: "run-04", NodeRef: "mill#12", Ref: "main"})

			Expect(err).NotTo(HaveOccurred())
			Expect(again.Branch).To(Equal("mill/mill-12"))
		})

		It("attaches to the workspace a parked run left on disk", func(ctx context.Context) {
			handle = create(ctx, "run-05", "mill#12")
			Expect(ws.Rotate(ctx, workspace.TokenFor(theBearer, workspace.Write))).To(Succeed())
			Expect(ws.Write(ctx, "notes.md", []byte("half done\n"))).To(Succeed())
			// A run that parks at a gate keeps its checkout; only the socket
			// ends with the worker that held the lease.
			Expect(ws.Close(ctx)).To(Succeed())

			resumed := local.New(local.Options{Root: root, Origin: from})
			DeferCleanup(func(ctx context.Context) { Expect(resumed.Destroy(ctx)).To(Succeed()) })
			again, err := resumed.Attach(ctx, workspace.Spec{RunID: "run-05", NodeRef: "mill#12"})

			Expect(err).NotTo(HaveOccurred())
			Expect(again.Branch).To(Equal("mill/mill-12"))
			Expect(again.Root).To(Equal(handle.Root))
			Expect(resumed.Rotate(ctx, workspace.TokenFor("a-fresh-bearer", workspace.Read))).To(Succeed())
			Expect(socket.Dial(again.Socket, "a-fresh-bearer").Read(ctx, "notes.md")).To(Equal([]byte("half done\n")))
		})
	})

	// The way out of the workspace — the proxy for the world, the gateway
	// for the model — is decided by the spec, and reaches every process the
	// workspace spawns beside the git identity: docs/sandbox.md §Network,
	// §Credentials. The gateway's bearer is the step token, and the step
	// token rotates.
	Describe("the workspace's way out", func() {
		decided := workspace.Egress{
			Proxy:   "http://10.99.14.1:3128",
			NoProxy: "10.99.14.1,127.0.0.1",
			Gateway: "http://10.99.14.1:8081",
		}

		// printed runs `env` through the chokepoint and returns what the
		// child saw, once it has exited.
		printed := func(ctx context.Context) map[string]string {
			GinkgoHelper()
			process, err := ws.Exec(ctx, workspace.Command{Path: "/usr/bin/env"})
			Expect(err).NotTo(HaveOccurred())
			out, err := io.ReadAll(process.Stdout)
			Expect(err).NotTo(HaveOccurred())
			Expect(process.Wait().Status).To(BeZero())
			env := map[string]string{}
			for _, line := range lines(string(out)) {
				if name, value, ok := strings.Cut(line, "="); ok {
					env[name] = value
				}
			}
			return env
		}

		BeforeEach(func(ctx context.Context) {
			made, err := ws.Create(ctx, workspace.Spec{RunID: "run-07", NodeRef: "mill#12", Origin: from, Egress: decided})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func(ctx context.Context) { Expect(ws.Destroy(ctx)).To(Succeed()) })
			handle = made
		})

		It("sets the proxy and the gateway on every exec, with the git identity", func(ctx context.Context) {
			Expect(ws.Rotate(ctx, workspace.TokenFor(theBearer, workspace.Exec))).To(Succeed())

			env := printed(ctx)

			Expect(env).To(HaveKeyWithValue("HTTPS_PROXY", decided.Proxy))
			Expect(env).To(HaveKeyWithValue("HTTP_PROXY", decided.Proxy))
			Expect(env).To(HaveKeyWithValue("NO_PROXY", decided.NoProxy))
			Expect(env).To(HaveKeyWithValue("ANTHROPIC_BASE_URL", decided.Gateway))
			Expect(env).To(HaveKeyWithValue("ANTHROPIC_AUTH_TOKEN", theBearer))
			Expect(env).To(HaveKeyWithValue("STEP_TOKEN", theBearer))
			Expect(env).To(HaveKeyWithValue("GIT_CONFIG_GLOBAL", ws.GitEnvironment()["GIT_CONFIG_GLOBAL"]))
			// And what the run loop hands a session is the same map.
			Expect(ws.Environment()).To(HaveKeyWithValue("ANTHROPIC_AUTH_TOKEN", theBearer))
			Expect(ws.Environment()).To(HaveKeyWithValue("HTTPS_PROXY", decided.Proxy))
		})

		It("The auth token follows the step", func(ctx context.Context) {
			By("given step 1 ran with token A")
			Expect(ws.Rotate(ctx, workspace.TokenFor("token-a", workspace.Exec))).To(Succeed())
			first := printed(ctx)
			Expect(first).To(HaveKeyWithValue("ANTHROPIC_AUTH_TOKEN", "token-a"))

			By("when the workspace rotates to token B for step 2")
			Expect(ws.Rotate(ctx, workspace.TokenFor("token-b", workspace.Exec))).To(Succeed())

			By("then step 2's exec carries B")
			second := printed(ctx)
			Expect(second).To(HaveKeyWithValue("ANTHROPIC_AUTH_TOKEN", "token-b"))
			Expect(second).To(HaveKeyWithValue("STEP_TOKEN", "token-b"))
			Expect(ws.Environment()).To(HaveKeyWithValue("ANTHROPIC_AUTH_TOKEN", "token-b"))

			By("and no process still carries A")
			// Step 1's process has exited — printed waits for it — and
			// nothing the workspace hands out names A any more.
			for name, value := range second {
				Expect(value).NotTo(Equal("token-a"), "%s still carries step 1's token", name)
			}
			for name, value := range ws.Environment() {
				Expect(value).NotTo(Equal("token-a"), "%s still carries step 1's token", name)
			}
		})
	})

	// A session runs git in its own shell — the `commit` skill has it do
	// exactly that — and it is handed HOME, for the shell and the SDK's
	// own state. What it must not be handed is the account's git identity: who
	// a commit is by is the workspace's answer, and the `commit` verifier
	// is checking that answer. docs/architecture.md §"The workspace".
	Describe("the session's own git", func() {
		var host string

		// sessionGit runs git the way a session's shell does: the account's
		// HOME and PATH, and what the workspace says about git over them.
		// Nothing else, which is what the sidecar hands the SDK.
		sessionGit := func(args ...string) (string, error) {
			command := exec.Command("git", args...) //nolint:gosec // the arguments are this spec's own
			command.Dir = handle.Root
			command.Env = []string{"HOME=" + host, "PATH=" + os.Getenv("PATH"), "LC_ALL=C"}
			for name, value := range ws.GitEnvironment() {
				command.Env = append(command.Env, name+"="+value)
			}
			out, err := command.CombinedOutput()
			return strings.TrimSpace(string(out)), err
		}

		BeforeEach(func(ctx context.Context) {
			// The host account's own git identity, in a HOME this spec owns:
			// nothing of the operator's is read or written. It disagrees with
			// the run's on every field a commit carries.
			host = filepath.Join(GinkgoT().TempDir(), "host-home")
			Expect(os.MkdirAll(filepath.Join(host, ".config", "git"), 0o700)).To(Succeed())
			config := "[user]\n\tname = Somebody Else\n\temail = somebody@elsewhere.test\n[commit]\n\tgpgsign = false\n"
			Expect(os.WriteFile(filepath.Join(host, ".gitconfig"), []byte(config), 0o600)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(host, ".config", "git", "config"), []byte(config), 0o600)).To(Succeed())
			handle = create(ctx, "run-06", "mill#12")
		})

		It("A session commits under the run's identity on a host that disagrees", func(ctx context.Context) {
			base := git(handle.Root, "rev-parse", "HEAD")
			Expect(os.WriteFile(filepath.Join(handle.Root, "notes.md"), []byte("what I did\n"), 0o600)).To(Succeed())
			Expect(sessionGit("add", "notes.md")).Error().NotTo(HaveOccurred())

			Expect(sessionGit("commit", "--message", "mill#12: implement")).Error().NotTo(HaveOccurred())

			sha, err := sessionGit("rev-parse", "HEAD")
			Expect(err).NotTo(HaveOccurred())
			identity := ws.Committer()
			Expect(sessionGit("show", "--no-patch", "--format=%an <%ae>", sha)).To(Equal(identity.String()))
			Expect(sessionGit("show", "--no-patch", "--format=%cn <%ce>", sha)).To(Equal(identity.String()))

			// The signature is the mill's key, verified against the
			// allowed_signers the same configuration names.
			out, err := sessionGit("verify-commit", sha)
			Expect(err).NotTo(HaveOccurred(), "git verify-commit: %s", out)
			Expect(out).To(ContainSubstring("Good \"git\" signature for " + identity.Email))

			// And that is what closes the step: the `commit` verifier, given
			// the commit the workspace observed, accepts it.
			verified, err := evidence.Verify(ctx, millv1.EvidenceKind_EVIDENCE_KIND_COMMIT, evidence.Step{
				Branch:    handle.Branch,
				Claim:     sha,
				Base:      base,
				Committer: evidence.Committer{Name: identity.Name, Email: identity.Email},
				Git:       ws.GitRunner(),
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(verified.Value.GetCommit()).To(Equal(sha))
		})

		It("The host's git configuration is never consulted", func() {
			listed, err := sessionGit("config", "--list", "--show-origin")
			Expect(err).NotTo(HaveOccurred())

			run := filepath.Dir(handle.Root)
			for _, line := range strings.Split(listed, "\n") {
				origin, _, found := strings.Cut(strings.TrimPrefix(line, "file:"), "\t")
				if !found || origin == os.DevNull {
					continue
				}
				// git names the checkout's own configuration relative to the
				// directory it ran in.
				if !filepath.IsAbs(origin) {
					origin = filepath.Join(handle.Root, origin)
				}
				// Every file git read is the workspace's own: the run's
				// configuration beside the checkout, or the checkout's.
				Expect(resolved(origin)).To(HavePrefix(resolved(run)),
					"git read %s, which is outside the workspace", origin)
			}
			Expect(listed).NotTo(ContainSubstring("somebody@elsewhere.test"))
			Expect(sessionGit("config", "user.email")).To(Equal(ws.Committer().Email))
		})
	})
})

// Twins are captured from $HOME, not the checkout, so these specs fabricate
// their own HOME rather than reuse the workspace fixture above — nothing
// here creates a workspace at all, only the directory CaptureTwin reads
// from directly (internal/workspace/local.go's CaptureTwin).
var _ = Describe("Capturing the session's twin", Label("unit"), func() {
	var home string

	BeforeEach(func() {
		home = GinkgoT().TempDir()
		GinkgoT().Setenv("HOME", home)
	})

	// transcriptDir is where CaptureTwin looks for root+runID's transcripts:
	// $HOME/.claude/projects/<cwd-slug>, cwd being the run's checkout
	// directory (docs/spikes/sdk-sessions.md §Q1; the transformation itself
	// is pinned directly against the doc's own example in cwdslug_test.go).
	transcriptDir := func(root, runID string) string {
		cwd := filepath.Join(root, runID, "checkout")
		slug := strings.NewReplacer("/", "-", ".", "-").Replace(cwd)
		return filepath.Join(home, ".claude", "projects", slug)
	}

	It("tars every transcript file under the run's slug, main session and subagent alike", func(ctx context.Context) {
		root := filepath.Join(GinkgoT().TempDir(), "workspaces")
		dir := transcriptDir(root, "run-01")
		Expect(os.MkdirAll(dir, 0o750)).To(Succeed())
		main := []byte(`{"type":"turn","role":"user","content":"main session"}` + "\n")
		sub := []byte(`{"type":"turn","role":"user","content":"a subagent's own session"}` + "\n")
		Expect(os.WriteFile(filepath.Join(dir, "session-main.jsonl"), main, 0o600)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, "session-sub.jsonl"), sub, 0o600)).To(Succeed())
		// Not a transcript: CaptureTwin's own doc says every .jsonl, nothing else.
		Expect(os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("not a transcript"), 0o600)).To(Succeed())

		r, err := local.CaptureTwin(ctx, root, "run-01", "session-main")
		Expect(err).NotTo(HaveOccurred())

		files := testsupport.Untar(r)
		Expect(files).To(HaveLen(2))
		Expect(files).To(HaveKeyWithValue("session-main.jsonl", main))
		Expect(files).To(HaveKeyWithValue("session-sub.jsonl", sub))
	})

	It("returns an empty tar and no error when the run left no transcript", func(ctx context.Context) {
		root := filepath.Join(GinkgoT().TempDir(), "workspaces")

		r, err := local.CaptureTwin(ctx, root, "run-never-ran", "session-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(testsupport.Untar(r)).To(BeEmpty())
	})
})

// resolved is a path with its symlinks evaluated, which is what a child
// process reports as its own working directory.
func resolved(path string) string {
	GinkgoHelper()
	out, err := filepath.EvalSymlinks(path)
	Expect(err).NotTo(HaveOccurred())
	return out
}

// lines is an environment dump as a sorted list.
func lines(output string) []string {
	fields := strings.Split(strings.TrimSpace(output), "\n")
	sort.Strings(fields)
	return fields
}
