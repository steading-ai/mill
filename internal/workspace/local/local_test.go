package local_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

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

			// And the environment it ran with contains no variable but the
			// two named
			environment, err := session.Exec(ctx, socket.ExecRequest{Path: "/usr/bin/env"})
			Expect(err).NotTo(HaveOccurred())
			Expect(lines(environment.Stdout)).To(ConsistOf(
				"STEP_TOKEN="+theBearer,
				"WORKSPACE_SOCKET="+handle.Socket,
			))
		})

		It("gives a child nothing the mill's own process carries", func(ctx context.Context) {
			GinkgoT().Setenv("MILL_A_SECRET_THE_MILL_HOLDS", "not for a session")
			session := step(ctx, workspace.Exec)

			environment, err := session.Exec(ctx, socket.ExecRequest{Path: "/usr/bin/env"})

			Expect(err).NotTo(HaveOccurred())
			Expect(environment.Stdout).NotTo(ContainSubstring("MILL_A_SECRET_THE_MILL_HOLDS"))
			Expect(lines(environment.Stdout)).To(HaveLen(2))
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
