package run_test

import (
	"context"
	"sync"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/harness"
	"github.com/ivy/mill/internal/run"
	"github.com/ivy/mill/internal/testsupport"
	"github.com/ivy/mill/internal/workspace"
	"github.com/ivy/mill/internal/workspace/machine"
)

// The ledger's record of the run's machine (docs/ledger.md §workspaces,
// docs/sandbox.md §Reconciliation): what the loop writes when the workspace
// opens and when the run finishes. The machine itself is the backend's
// business (internal/workspace/machine); what these specs use is a local
// workspace wearing a machine's handle, which is exactly what the loop sees.

// sandboxed is a workspace whose handle carries a Sandbox and whose Destroy
// promotes its lineages, as the machine backend's does.
type sandboxed struct {
	run.Workspace
	sandbox   workspace.Sandbox
	promote   map[string]int
	mu        sync.Mutex
	promoted  []workspace.Lineage
	destroyed bool
}

func (s *sandboxed) Handle() *workspace.Handle {
	handle := *s.Workspace.Handle()
	handle.Kind = workspace.KindMachine
	sandbox := s.sandbox
	sandbox.Lineages = append([]workspace.Lineage(nil), s.sandbox.Lineages...)
	handle.Sandbox = &sandbox
	return &handle
}

func (s *sandboxed) Destroy(ctx context.Context) error {
	if err := s.Workspace.Destroy(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.destroyed = true
	for _, lineage := range s.sandbox.Lineages {
		if generation, ok := s.promote[lineage.Scope+"/"+lineage.Name]; ok {
			lineage.Promoted = &generation
		}
		s.promoted = append(s.promoted, lineage)
	}
	return nil
}

func (s *sandboxed) Promoted() []workspace.Lineage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]workspace.Lineage(nil), s.promoted...)
}

var _ = Describe("The ledger's record of the machine", Label(testsupport.DBLabel), func() {
	var f *fixture
	var ctx context.Context

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
		f = start(ctx)
		f.node(ctx, "one line change", "Change the README's first line.")
		f.script(f.quickFixScripts()...)
	})

	// workspaceRow reads the run's workspace row as the ledger renders it.
	workspaceRow := func() *millv1.Workspace {
		GinkgoHelper()
		return f.inspect(ctx).GetSummary().GetWorkspace()
	}

	// Scenario: The ledger names the machine
	//   Given a run created with the machine backend
	//   When the workspace is recorded
	//   Then the row holds the machine, unit, digest and veth
	//
	// Scenario: Promotion is recorded at the end
	//   Given a run whose last evidence-closed step promoted a lineage
	//   When the run finishes
	//   Then the row's lineage entry names the promoted generation
	It("The ledger names the machine, and the promotion is recorded at the end", func() {
		By("given a run created with the machine backend, whose caches restored two lineages")
		var ws *sandboxed
		f.wrapWorkspace = func(inner run.Workspace) run.Workspace {
			ws = &sandboxed{
				Workspace: inner,
				sandbox: workspace.Sandbox{
					Machine: "mill-01jrun", Unit: "mill-machine@01jrun.service",
					Image: "sha256:3c9a43", Veth: "ve-mill-01jru",
					Lineages: []workspace.Lineage{
						{Scope: "global", Name: "mise", Restored: 3},
						{Scope: "repo/abc123", Name: "go-build", Restored: 0},
					},
				},
				// The last evidence-closed step's candidate promotes mise
				// to generation 4; go-build's copy is discarded.
				promote: map[string]int{"global/mise": 4},
			}
			return ws
		}

		By("when the workspace is recorded — as the run opens — and the run finishes")
		Expect(f.runner().Execute(ctx, f.nodeRef)).To(Succeed())
		Expect(ws.destroyed).To(BeTrue())

		By("then the row holds the machine, unit, digest and veth")
		row := workspaceRow()
		Expect(row).NotTo(BeNil())
		Expect(row.GetKind()).To(Equal(millv1.WorkspaceKind_WORKSPACE_KIND_MACHINE))
		Expect(row.GetWorkerRef()).To(Equal(f.worker))
		Expect(row.GetSandbox().GetMachine()).To(Equal("mill-01jrun"))
		Expect(row.GetSandbox().GetUnit()).To(Equal("mill-machine@01jrun.service"))
		Expect(row.GetSandbox().GetImageDigest()).To(Equal("sha256:3c9a43"))
		Expect(row.GetSandbox().GetVeth()).To(Equal("ve-mill-01jru"))

		By("and the row's lineage entry names the promoted generation, once the run has finished")
		Expect(row.GetState()).To(Equal(millv1.WorkspaceState_WORKSPACE_STATE_DESTROYED))
		lineages := row.GetSandbox().GetLineages()
		Expect(lineages).To(HaveLen(2))
		Expect(lineages[0].GetScope() + "/" + lineages[0].GetName()).To(Equal("global/mise"))
		Expect(lineages[0].GetRestored()).To(Equal(int64(3)))
		Expect(lineages[0].Promoted).NotTo(BeNil())
		Expect(lineages[0].GetPromoted()).To(Equal(int64(4)))
		Expect(lineages[1].GetName()).To(Equal("go-build"))
		Expect(lineages[1].Promoted).To(BeNil(), "a discarded copy promoted nothing")

		var machineColumn string
		Expect(f.db.QueryRow(ctx, `SELECT machine FROM workspaces WHERE run_id = $1`, f.runOf(ctx).GetId()).Scan(&machineColumn)).To(Succeed())
		Expect(machineColumn).To(Equal("mill-01jrun"), "the machine name is a column of its own, for reconciliation to match on")

		By("and the ledger's live runs would have carried the machine for reconciliation while the run was live")
		res, err := f.store.Status(ctx, connect.NewRequest(&millv1.StatusRequest{Statuses: []millv1.RunStatus{millv1.RunStatus_RUN_STATUS_DONE}}))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Msg.GetRuns()).To(HaveLen(1))
		Expect(res.Msg.GetRuns()[0].GetWorkspace().GetSandbox().GetMachine()).To(Equal("mill-01jrun"))
	})

	// Scenario: A local run is unchanged
	//   Given a run with the local backend
	//   When the workspace is recorded
	//   Then the sandbox fields are empty and every existing field is as before
	It("A local run is unchanged", func() {
		By("given a run with the local backend")
		f.keepWorkspace = true

		By("when the workspace is recorded")
		Expect(f.runner().Execute(ctx, f.nodeRef)).To(Succeed())

		By("then the sandbox fields are empty and every existing field is as before")
		row := workspaceRow()
		Expect(row).NotTo(BeNil())
		Expect(row.GetSandbox()).To(BeNil())
		Expect(row.GetKind()).To(Equal(millv1.WorkspaceKind_WORKSPACE_KIND_LOCAL))
		Expect(row.GetRunId()).To(Equal(f.runOf(ctx).GetId()))
		Expect(row.GetWorkerRef()).To(Equal(f.worker))
		Expect(row.GetRef()).To(Equal("main"))
		Expect(row.GetState()).To(Equal(millv1.WorkspaceState_WORKSPACE_STATE_DESTROYED), "the run finished and destroyed its workspace")
		Expect(row.GetCreatedAt()).NotTo(BeNil())
		Expect(row.GetDestroyedAt()).NotTo(BeNil())
		var machineColumn, unit, digest, veth string
		var lineages []byte
		Expect(f.db.QueryRow(ctx, `SELECT machine, unit, image_digest, veth, lineages FROM workspaces WHERE run_id = $1`, row.GetRunId()).
			Scan(&machineColumn, &unit, &digest, &veth, &lineages)).To(Succeed())
		Expect([]string{machineColumn, unit, digest, veth}).To(Equal([]string{"", "", "", ""}))
		Expect(lineages).To(BeNil())
	})

	It("hands reconciliation the machine the ledger names for each live run", func() {
		f.wrapWorkspace = func(inner run.Workspace) run.Workspace {
			return &sandboxed{Workspace: inner, sandbox: workspace.Sandbox{Machine: "mill-01jrun", Unit: "mill-machine@01jrun.service"}}
		}
		// The ledger is read from inside the first session: the workspace
		// is recorded before any step runs, so the run is live with its
		// machine on record by then.
		var mu sync.Mutex
		var seen []machine.LiveRun
		scripts := f.quickFixScripts()
		first := scripts[0].Do
		scripts[0].Do = func(session harness.Session) {
			defer GinkgoRecover()
			first(session)
			live, err := run.NewLedger(f.store, f.worker, nil).LiveRuns(ctx)
			Expect(err).NotTo(HaveOccurred())
			mu.Lock()
			seen = live
			mu.Unlock()
		}
		f.script(scripts...)

		Expect(f.runner().Execute(ctx, f.nodeRef)).To(Succeed())

		mu.Lock()
		defer mu.Unlock()
		Expect(seen).To(HaveLen(1))
		Expect(seen[0].Machine).To(Equal("mill-01jrun"))
		Expect(seen[0].RunID).To(Equal(f.runOf(ctx).GetId()))
	})
})
