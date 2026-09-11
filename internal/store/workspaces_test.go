package store_test

import (
	"time"

	"github.com/jackc/pgx/v5"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/store"
	"github.com/ivy/mill/internal/testsupport"
)

// sandboxRow is the workspaces row's sandbox columns, read straight from
// the table: what the ledger holds, not what the store renders.
type sandboxRow struct {
	kind, state, machine, unit, digest, veth string
	lineages                                 []byte
	destroyed                                bool
}

func (l *ledger) sandboxRow(ctx SpecContext, runID string) sandboxRow {
	GinkgoHelper()
	var row sandboxRow
	err := l.db.QueryRow(ctx,
		`SELECT kind, state, machine, unit, image_digest, veth, lineages, destroyed_at IS NOT NULL
		 FROM workspaces WHERE run_id = $1 ORDER BY id DESC LIMIT 1`, runID).
		Scan(&row.kind, &row.state, &row.machine, &row.unit, &row.digest, &row.veth, &row.lineages, &row.destroyed)
	Expect(err).NotTo(HaveOccurred())
	return row
}

func machineSandbox() *millv1.Sandbox {
	return &millv1.Sandbox{
		Machine:     "mill-01jrun",
		Unit:        "mill-machine@01jrun.service",
		ImageDigest: "sha256:" + "3c9a43" + "0000000000000000000000000000000000000000000000000000000000",
		Veth:        "ve-mill-01jru",
		Lineages: []*millv1.SandboxLineage{
			{Scope: "global", Name: "mise", Restored: 3},
			{Scope: "repo/abc123", Name: "go-build", Restored: 0},
		},
	}
}

var _ = Describe("Workspaces", Label(testsupport.DBLabel), func() {
	// Scenario: The ledger names the machine
	//   Given a run created with the machine backend
	//   When the workspace is recorded
	//   Then the row holds the machine, unit, digest and veth
	It("The ledger names the machine", func(ctx SpecContext) {
		By("given a run created with the machine backend")
		l := newLedger(ctx)
		run, _ := l.runWithWorkflow(ctx, "mill#80")
		sandbox := machineSandbox()

		By("when the workspace is recorded")
		recorded, err := l.store.RecordWorkspace(ctx, l.lease, store.WorkspaceParams{
			RunID: run.GetId(), Kind: millv1.WorkspaceKind_WORKSPACE_KIND_MACHINE,
			WorkerRef: "worker-1", Ref: "mill/mill-80", Sandbox: sandbox,
		})
		Expect(err).NotTo(HaveOccurred())

		By("then the row holds the machine, unit, digest and veth")
		row := l.sandboxRow(ctx, run.GetId())
		Expect(row.kind).To(Equal("machine"))
		Expect(row.state).To(Equal("live"))
		Expect(row.machine).To(Equal(sandbox.GetMachine()))
		Expect(row.unit).To(Equal(sandbox.GetUnit()))
		Expect(row.digest).To(Equal(sandbox.GetImageDigest()))
		Expect(row.veth).To(Equal(sandbox.GetVeth()))
		Expect(row.lineages).To(MatchJSON(`[{"scope":"global","name":"mise","restored":3},{"scope":"repo/abc123","name":"go-build","restored":0}]`),
			"each lineage's restored generation, and no promoted one yet")
		Expect(recorded.GetSandbox().GetMachine()).To(Equal(sandbox.GetMachine()))
		Expect(recorded.GetSandbox().GetLineages()).To(HaveLen(2))
		Expect(recorded.GetSandbox().GetLineages()[0].Promoted).To(BeNil())

		By("and a resume that reattaches the workspace brings the row up to date rather than adding one")
		sandbox.Veth = "ve-mill-01jrv"
		again, err := l.store.RecordWorkspace(ctx, l.lease, store.WorkspaceParams{
			RunID: run.GetId(), Kind: millv1.WorkspaceKind_WORKSPACE_KIND_MACHINE,
			WorkerRef: "worker-2", Ref: "mill/mill-80", Sandbox: sandbox,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(again.GetId()).To(Equal(recorded.GetId()))
		Expect(again.GetWorkerRef()).To(Equal("worker-2"))
		var count int
		Expect(l.db.QueryRow(ctx, `SELECT count(*) FROM workspaces WHERE run_id = $1`, run.GetId()).Scan(&count)).To(Succeed())
		Expect(count).To(Equal(1))
		Expect(l.sandboxRow(ctx, run.GetId()).veth).To(Equal("ve-mill-01jrv"))
	}, SpecTimeout(time.Minute))

	// Scenario: Promotion is recorded at the end
	//   Given a run whose last evidence-closed step promoted a lineage
	//   When the run finishes
	//   Then the row's lineage entry names the promoted generation
	It("Promotion is recorded at the end", func(ctx SpecContext) {
		By("given a run with a machine whose caches were restored")
		l := newLedger(ctx)
		run, _ := l.runWithWorkflow(ctx, "mill#80")
		_, err := l.store.RecordWorkspace(ctx, l.lease, store.WorkspaceParams{
			RunID: run.GetId(), Kind: millv1.WorkspaceKind_WORKSPACE_KIND_MACHINE,
			WorkerRef: "worker-1", Ref: "mill/mill-80", Sandbox: machineSandbox(),
		})
		Expect(err).NotTo(HaveOccurred())

		By("when the run finishes: marked done, its lease released, and then its workspace destroyed and the promotion recorded")
		_, err = l.store.Done(ctx, l.lease, run.GetId())
		Expect(err).NotTo(HaveOccurred())
		four := int64(4)
		finished, err := l.store.FinishWorkspace(ctx, l.lease, run.GetId(), []*millv1.SandboxLineage{
			{Scope: "global", Name: "mise", Restored: 3, Promoted: &four},
		})
		Expect(err).NotTo(HaveOccurred())

		By("then the row's lineage entry names the promoted generation, and a lineage not promoted stays as restored")
		row := l.sandboxRow(ctx, run.GetId())
		Expect(row.state).To(Equal("destroyed"))
		Expect(row.destroyed).To(BeTrue())
		Expect(row.lineages).To(MatchJSON(`[{"scope":"global","name":"mise","restored":3,"promoted":4},{"scope":"repo/abc123","name":"go-build","restored":0}]`))
		Expect(finished.GetState()).To(Equal(millv1.WorkspaceState_WORKSPACE_STATE_DESTROYED))
		Expect(finished.GetSandbox().GetLineages()[0].GetPromoted()).To(Equal(four))
		Expect(finished.GetSandbox().GetLineages()[1].Promoted).To(BeNil())

		By("and the summary the CLI reads carries the workspace")
		detail, err := l.store.Inspect(ctx, store.InspectParams{ID: run.GetId()})
		Expect(err).NotTo(HaveOccurred())
		Expect(detail.Summary.GetWorkspace().GetSandbox().GetMachine()).To(Equal("mill-01jrun"))
		Expect(detail.Summary.GetWorkspace().GetSandbox().GetLineages()[0].GetPromoted()).To(Equal(four))

		By("and the end is on the audit trail")
		events, err := l.store.Events(ctx, run.GetId())
		Expect(err).NotTo(HaveOccurred())
		var ops []string
		for _, event := range events {
			ops = append(ops, event.Op)
		}
		Expect(ops).To(ContainElements("record_workspace", "finish_workspace"))
	}, SpecTimeout(time.Minute))

	It("refuses a promotion of a lineage the run never restored", func(ctx SpecContext) {
		l := newLedger(ctx)
		run, _ := l.runWithWorkflow(ctx, "mill#80")
		_, err := l.store.RecordWorkspace(ctx, l.lease, store.WorkspaceParams{
			RunID: run.GetId(), Kind: millv1.WorkspaceKind_WORKSPACE_KIND_MACHINE, Sandbox: machineSandbox(),
		})
		Expect(err).NotTo(HaveOccurred())
		one := int64(1)
		_, err = l.store.FinishWorkspace(ctx, l.lease, run.GetId(), []*millv1.SandboxLineage{
			{Scope: "global", Name: "cargo", Promoted: &one},
		})
		Expect(err).To(MatchError(ContainSubstring("global/cargo was never restored")))
		Expect(l.sandboxRow(ctx, run.GetId()).state).To(Equal("live"), "nothing was written")
	}, SpecTimeout(time.Minute))

	It("fences the finish by the lease while the run is live, and admits it once the run has ended", func(ctx SpecContext) {
		l := newLedger(ctx)
		run, _ := l.runWithWorkflow(ctx, "mill#80")
		_, err := l.store.RecordWorkspace(ctx, l.lease, store.WorkspaceParams{
			RunID: run.GetId(), Kind: millv1.WorkspaceKind_WORKSPACE_KIND_LOCAL,
		})
		Expect(err).NotTo(HaveOccurred())

		_, err = l.store.FinishWorkspace(ctx, "somebody-elses-lease", run.GetId(), nil)
		Expect(err).To(MatchError(store.ErrFenced), "a live run's workspace is finished under its lease alone")
		_, err = l.store.FinishWorkspace(ctx, l.lease, run.GetId(), nil)
		Expect(err).NotTo(HaveOccurred(), "the holder may finish it: a run that failed keeps or destroys its workspace under its lease")

		By("and a run that has ended holds no lease, so its ended state admits the write")
		other, _ := l.runWithWorkflow(ctx, "mill#81")
		_, err = l.store.RecordWorkspace(ctx, l.lease, store.WorkspaceParams{
			RunID: other.GetId(), Kind: millv1.WorkspaceKind_WORKSPACE_KIND_LOCAL,
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = l.store.Done(ctx, l.lease, other.GetId())
		Expect(err).NotTo(HaveOccurred())
		_, err = l.store.FinishWorkspace(ctx, l.lease, other.GetId(), nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(l.sandboxRow(ctx, other.GetId()).state).To(Equal("destroyed"))
		_, err = l.store.FinishWorkspace(ctx, l.lease, other.GetId(), nil)
		Expect(err).To(MatchError(store.ErrNotFound), "a workspace is finished once")
	}, SpecTimeout(time.Minute))

	// Scenario: A local run is unchanged
	//   Given a run with the local backend
	//   When the workspace is recorded
	//   Then the sandbox fields are empty and every existing field is as before
	It("A local run is unchanged", func(ctx SpecContext) {
		By("given a run with the local backend")
		l := newLedger(ctx)
		run, _ := l.runWithWorkflow(ctx, "mill#80")

		By("when the workspace is recorded")
		recorded, err := l.store.RecordWorkspace(ctx, l.lease, store.WorkspaceParams{
			RunID: run.GetId(), Kind: millv1.WorkspaceKind_WORKSPACE_KIND_LOCAL,
			WorkerRef: "worker-1", Ref: "main",
		})
		Expect(err).NotTo(HaveOccurred())

		By("then the sandbox fields are empty and every existing field is as before")
		row := l.sandboxRow(ctx, run.GetId())
		Expect(row.kind).To(Equal("local"))
		Expect(row.state).To(Equal("live"))
		Expect(row.machine).To(BeEmpty())
		Expect(row.unit).To(BeEmpty())
		Expect(row.digest).To(BeEmpty())
		Expect(row.veth).To(BeEmpty())
		Expect(row.lineages).To(BeNil(), "no lineages: the column is null, not an empty list")
		Expect(recorded.GetSandbox()).To(BeNil())
		Expect(recorded.GetKind()).To(Equal(millv1.WorkspaceKind_WORKSPACE_KIND_LOCAL))
		Expect(recorded.GetRef()).To(Equal("main"))
		Expect(recorded.GetWorkerRef()).To(Equal("worker-1"))
		Expect(recorded.GetState()).To(Equal(millv1.WorkspaceState_WORKSPACE_STATE_LIVE))
		Expect(recorded.GetCreatedAt()).NotTo(BeNil())
		Expect(recorded.GetDestroyedAt()).To(BeNil())

		var existing struct {
			id, runID, kind, workerRef, ref, state string
			destroyedAt                            *time.Time
		}
		Expect(l.db.QueryRow(ctx, `SELECT id, run_id, kind, worker_ref, ref, state, destroyed_at FROM workspaces WHERE run_id = $1`, run.GetId()).
			Scan(&existing.id, &existing.runID, &existing.kind, &existing.workerRef, &existing.ref, &existing.state, &existing.destroyedAt)).To(Succeed())
		Expect(existing.id).To(Equal(recorded.GetId()))
		Expect(existing.runID).To(Equal(run.GetId()))
		Expect(existing.ref).To(Equal("main"))
		Expect(existing.destroyedAt).To(BeNil())

		By("and a run that recorded no workspace shows none")
		other, _ := l.runWithWorkflow(ctx, "mill#81")
		detail, err := l.store.Inspect(ctx, store.InspectParams{ID: other.GetId()})
		Expect(err).NotTo(HaveOccurred())
		Expect(detail.Summary.GetWorkspace()).To(BeNil())
		Expect(l.db.QueryRow(ctx, `SELECT id FROM workspaces WHERE run_id = $1`, other.GetId()).Scan(new(string))).To(MatchError(pgx.ErrNoRows))
	}, SpecTimeout(time.Minute))
})
