package cli_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/testsupport"
	"github.com/steading-ai/mill/internal/workspace"
)

// `mill workspace` reads and removes the checkouts a host is holding. The
// directories here are made the way the local workspace makes them —
// `<root>/<run id>/checkout`, a clone on the run's branch — because what the
// tool answers about is a real checkout and a fixture git would not have a
// branch or a head to report.

// checkout puts a run's workspace on disk under root and returns the run's
// directory. The repository has one commit, on the branch the node ref
// derives, which is what a workspace opened for the run would be on.
func checkout(root, runID, nodeRef string) string {
	GinkgoHelper()
	dir := filepath.Join(root, runID)
	tree := filepath.Join(dir, "checkout")
	Expect(os.MkdirAll(tree, 0o700)).To(Succeed())
	Expect(os.WriteFile(filepath.Join(tree, "README.md"), []byte("as cloned\n"), 0o600)).To(Succeed())
	git(tree, "init", "--initial-branch="+workspace.BranchFor(nodeRef))
	git(tree, "config", "user.email", "spec@example.test")
	git(tree, "config", "user.name", "Spec")
	git(tree, "config", "commit.gpgsign", "false")
	git(tree, "add", "README.md")
	git(tree, "commit", "-m", "first")
	return dir
}

// ended marks a run done, which is the state that makes its workspace
// removable. The lease it was opened under is the one the completing patch
// released, which is what Done is fenced by.
func ended(ctx context.Context, r *runs) {
	GinkgoHelper()
	_, err := r.store.Done(ctx, connect.NewRequest(&millv1.DoneRequest{
		LeaseId: r.lease, RunId: r.run.GetId(),
	}))
	Expect(err).NotTo(HaveOccurred())
}

var _ = Describe("mill workspace", Label(testsupport.DBLabel), func() {
	var ctx context.Context
	var root string

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
		root = filepath.Join(GinkgoT().TempDir(), "workspaces")
	})

	It("Clean removes only what is finished", func() {
		// Given workspaces for a done run and a running run.
		m := serving(ctx)
		done := openRunOn(ctx, m, "millcli#70", nodeID(70))
		ended(ctx, done)
		running := openRunOn(ctx, m, "millcli#71", nodeID(71))

		doneDir := checkout(root, done.run.GetId(), "millcli#70")
		runningDir := checkout(root, running.run.GetId(), "millcli#71")

		// When workspace clean runs.
		out := m.run(ctx, "workspace", "clean", "--workspace-dir", root)

		// Then the done run's workspace is removed.
		Expect(out).To(ContainSubstring(done.run.GetId()))
		Expect(rowFor(out, done.run.GetId())).To(ContainSubstring("removed"))
		Expect(doneDir).NotTo(BeADirectory())

		// And the running run's workspace remains.
		Expect(out).NotTo(ContainSubstring(running.run.GetId()))
		Expect(runningDir).To(BeADirectory())
	})

	It("show prints the checkout path, branch, head, and whether the tree is clean", func() {
		m := serving(ctx)
		r := openRunOn(ctx, m, "millcli#72", nodeID(72))
		dir := checkout(root, r.run.GetId(), "millcli#72")

		out := m.run(ctx, "workspace", "show", "millcli#72", "--workspace-dir", root)
		Expect(rowFor(out, "path")).To(ContainSubstring(filepath.Join(dir, "checkout")))
		Expect(rowFor(out, "branch")).To(ContainSubstring(workspace.BranchFor("millcli#72")))
		Expect(rowFor(out, "head")).To(MatchRegexp(`head\s+[0-9a-f]{40}`))
		Expect(rowFor(out, "tree")).To(ContainSubstring("clean"))
		Expect(rowFor(out, "socket")).To(ContainSubstring("absent"))
		Expect(rowFor(out, "retained")).To(ContainSubstring("being worked"))

		// A tree a session has written to and not committed reads dirty,
		// which is what the `clean-tree` check asserts against.
		Expect(os.WriteFile(filepath.Join(dir, "checkout", "scratch.txt"), []byte("left behind\n"), 0o600)).To(Succeed())
		Expect(rowFor(m.run(ctx, "workspace", "show", "millcli#72", "--workspace-dir", root), "tree")).
			To(ContainSubstring("dirty"))
	})

	It("clean --dry-run lists what it would remove and removes nothing", func() {
		m := serving(ctx)
		r := openRunOn(ctx, m, "millcli#73", nodeID(73))
		ended(ctx, r)
		dir := checkout(root, r.run.GetId(), "millcli#73")

		out := m.run(ctx, "workspace", "clean", "--workspace-dir", root, "--dry-run")
		Expect(rowFor(out, r.run.GetId())).To(ContainSubstring("would remove"))
		Expect(dir).To(BeADirectory())
	})

	It("leaves a directory the ledger holds no ended run for alone", func() {
		m := serving(ctx)
		// A run id the ledger has never heard of: deciding a directory is
		// rubbish needs a record that says so.
		stray := checkout(root, nodeID(74), "millcli#74")

		out := m.run(ctx, "workspace", "clean", "--workspace-dir", root)
		Expect(out).To(ContainSubstring("nothing to remove"))
		Expect(stray).To(BeADirectory())
	})
})

// git runs one git command for the fixture, through the process chokepoint:
// os/exec is internal/workspace's alone, here as everywhere else.
func git(dir string, args ...string) {
	GinkgoHelper()
	process, err := workspace.Spawner{Floor: scratchEnv()}.Exec(context.Background(), workspace.Command{
		Path: "git", Args: args, Dir: dir,
		Env: map[string]string{
			"PATH":              os.Getenv("PATH"),
			"HOME":              dir,
			"GIT_CONFIG_GLOBAL": os.DevNull,
			"GIT_CONFIG_SYSTEM": os.DevNull,
		},
	})
	Expect(err).NotTo(HaveOccurred())
	_ = process.Stdin.Close()
	stdout, _ := io.ReadAll(process.Stdout)
	stderr, _ := io.ReadAll(process.Stderr)
	Expect(process.Wait().Status).To(BeZero(), "git %s: %s %s", strings.Join(args, " "), stdout, stderr)
}

// scratchEnv is the environment for the fixture's own git, with the host's
// GIT_* variables cleared: under a git hook the process inherits GIT_DIR and
// GIT_INDEX_FILE, and this git would otherwise act on the hook's repository
// rather than the fixture, whatever Dir says.
func scratchEnv() []string {
	env := os.Environ()
	clean := make([]string, 0, len(env))
	for _, kv := range env {
		if !strings.HasPrefix(kv, "GIT_") {
			clean = append(clean, kv)
		}
	}
	return clean
}
