package local_test

import (
	"context"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/workspace"
	"github.com/ivy/mill/internal/workspace/local"
)

// The inspector answers `mill workspace show` and `mill workspace clean`. It
// opens nothing: a spec here asserts that reading a workspace leaves it
// exactly as it found it, because a worker may still be holding the run.

var _ = Describe("Inspecting the workspaces a host holds", func() {
	// Run ids are canonical ULIDs, which is how a directory under the root
	// is told from the signing key that lives beside it.
	const runID = "01JAAAAAAAAAAAAAAAAAAAAAAA"

	var root, from string
	var ws *local.Workspace
	var i *local.Inspector

	BeforeEach(func() {
		root = filepath.Join(GinkgoT().TempDir(), "workspaces")
		from = origin()
		ws = local.New(local.Options{Root: root, Origin: from})
		i = local.NewInspector(local.Options{Root: root})
	})

	It("reports the checkout, the branch, the head and a clean tree", func(ctx context.Context) {
		handle, err := ws.Create(ctx, workspace.Spec{RunID: runID, NodeRef: "mill#12"})
		Expect(err).NotTo(HaveOccurred())

		info, err := i.Inspect(ctx, runID)
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Checkout).To(Equal(handle.Root))
		Expect(info.Branch).To(Equal("mill/mill-12"))
		Expect(info.Head).To(Equal(git(handle.Root, "rev-parse", "HEAD")))
		Expect(info.Clean).To(BeTrue())
		Expect(info.Changed).To(BeZero())
		Expect(info.Detail).To(BeEmpty())

		// The socket is bound while the workspace is open, and gone once
		// the worker has closed it.
		Expect(info.SocketPresent).To(BeTrue())
		Expect(info.Socket).To(Equal(handle.Socket))
	})

	It("reports a tree a session left uncommitted as dirty", func(ctx context.Context) {
		handle, err := ws.Create(ctx, workspace.Spec{RunID: runID, NodeRef: "mill#12"})
		Expect(err).NotTo(HaveOccurred())
		Expect(os.WriteFile(filepath.Join(handle.Root, "scratch.txt"), []byte("left behind\n"), 0o600)).To(Succeed())

		info, err := i.Inspect(ctx, runID)
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Clean).To(BeFalse())
		Expect(info.Changed).To(Equal(1))
	})

	It("refuses a run this host holds no workspace for", func(ctx context.Context) {
		_, err := i.Inspect(ctx, "01JBBBBBBBBBBBBBBBBBBBBBBB")
		Expect(err).To(MatchError(local.ErrNoWorkspace))
	})

	It("lists the run directories and nothing else, and a missing root holds none", func(ctx context.Context) {
		Expect(i.RunIDs()).To(BeEmpty(), "a host that has never run anything has nothing to clean")

		_, err := ws.Create(ctx, workspace.Spec{RunID: runID, NodeRef: "mill#12"})
		Expect(err).NotTo(HaveOccurred())
		// The mill's signing key lives beside the runs and is not one of
		// them, and neither is any other name that is not a run id.
		Expect(os.MkdirAll(filepath.Join(root, "signing"), 0o700)).To(Succeed())

		Expect(i.RunIDs()).To(ConsistOf(runID))
	})

	It("removes a run's directory and leaves the signing key above it", func(ctx context.Context) {
		_, err := ws.Create(ctx, workspace.Spec{RunID: runID, NodeRef: "mill#12"})
		Expect(err).NotTo(HaveOccurred())
		Expect(ws.Close(ctx)).To(Succeed())
		key := filepath.Join(root, "signing")

		Expect(i.Remove(runID)).To(Succeed())
		Expect(filepath.Join(root, runID)).NotTo(BeADirectory())
		Expect(key).To(BeADirectory(), "the key is the account's, not the run's")
		Expect(i.Remove(runID)).To(MatchError(local.ErrNoWorkspace))
	})
})
