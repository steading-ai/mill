package cli_test

import (
	"context"
	"strings"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/testsupport"
)

// `mill rerun` is the operator saying "the work up to here is fine, redo
// from this step". It is specified against a real service over a real
// socket, with the ledger composed through the same Connect API a worker
// writes it with.

// stoppedAtCleanTree is the ledger both endings share: gather-context and
// implement closed, a commit closed on a commit that turned out to be the
// wrong one, and clean-tree failed against it. The run still holds the lease
// of the claim that opened it; what ends it is the caller's business.
func stoppedAtCleanTree(ctx context.Context, nodeRef string, id int) *runs {
	GinkgoHelper()
	r := openRun(ctx, nodeRef, nodeID(id))
	r.closeGatherContext(ctx, "sha256:"+strings.Repeat("a", 64))
	r.close(ctx, r.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL), &millv1.CloseStepRunRequest{
		Evidence: &millv1.Evidence{
			Kind:       millv1.EvidenceKind_EVIDENCE_KIND_ARTIFACT,
			Value:      &millv1.Evidence_Artifact{Artifact: "sha256:" + strings.Repeat("b", 64)},
			VerifiedBy: "mill: skift accepted the artifact",
		},
	})
	r.close(ctx, r.start(ctx, "commit", millv1.StepKind_STEP_KIND_SKILL), &millv1.CloseStepRunRequest{
		Evidence: &millv1.Evidence{
			Kind:       millv1.EvidenceKind_EVIDENCE_KIND_COMMIT,
			Value:      &millv1.Evidence_Commit{Commit: strings.Repeat("c", 40)},
			VerifiedBy: "mill: the commit is on the run's branch",
		},
	})
	r.fail(ctx, "clean-tree", 1)
	return r
}

// parkedAtCleanTree is the run scenario one is given: stopped at clean-tree,
// and the breaker parked it. The gate the park filed is what nulls the
// lease, which is what leaves the run open to an operator's write.
func parkedAtCleanTree(ctx context.Context, nodeRef string, id int) *runs {
	GinkgoHelper()
	r := stoppedAtCleanTree(ctx, nodeRef, id)
	r.park(ctx, nodeID(id+50))
	return r
}

// failedAtCleanTree is the same ledger ended the other way: the run was
// failed outright, which is terminal to the run loop and nulls the lease.
func failedAtCleanTree(ctx context.Context, nodeRef string, id int) *runs {
	GinkgoHelper()
	r := stoppedAtCleanTree(ctx, nodeRef, id)
	_, err := r.store.Fail(ctx, connect.NewRequest(&millv1.FailRequest{
		LeaseId: r.lease, RunId: r.run.GetId(), Reason: "the gate was denied",
	}))
	Expect(err).NotTo(HaveOccurred())
	return r
}

// statusesOf is the ledger's status for the latest attempt at each step.
func statusesOf(detail *millv1.InspectResponse) map[string]millv1.StepRunStatus {
	out := map[string]millv1.StepRunStatus{}
	for _, step := range detail.GetSteps() {
		out[step.GetStepRun().GetStepName()] = step.GetStepRun().GetStatus()
	}
	return out
}

var _ = Describe("mill rerun", Label(testsupport.DBLabel), func() {
	var ctx context.Context

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
	})

	It("Redo from a step without a new generation", func() {
		// Given a parked run whose commit and clean-tree attempts are bad.
		r := parkedAtCleanTree(ctx, "millcli#60", 60)
		Expect(r.reload(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_PARKED))
		Expect(r.reload(ctx).GetGeneration()).To(Equal(int64(1)))

		// When the operator reruns from implement.
		out := r.cli(ctx, "rerun", "millcli#60", "--from", "implement")

		// Then implement through clean-tree are superseded with reason
		// rerun. The span is what the command names and what the ledger
		// holds; the reason is on the event each superseded attempt got.
		Expect(out).To(ContainSubstring("from implement through clean-tree"))
		for _, step := range []string{"implement", "commit", "clean-tree"} {
			Expect(rowFor(out, step)).To(ContainSubstring("rerun"), "%s is named as superseded", step)
		}
		detail, err := r.store.Inspect(ctx, connect.NewRequest(&millv1.InspectRequest{Id: r.run.GetId()}))
		Expect(err).NotTo(HaveOccurred())
		statuses := statusesOf(detail.Msg)
		for _, step := range []string{"implement", "commit", "clean-tree"} {
			Expect(statuses[step]).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_SUPERSEDED), "step %s", step)
		}
		Expect(statuses["gather-context"]).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED),
			"the work before the named step stands")

		history, err := r.store.History(ctx, connect.NewRequest(&millv1.HistoryRequest{Id: r.run.GetId()}))
		Expect(err).NotTo(HaveOccurred())
		var reruns int
		for _, event := range history.Msg.GetEvents() {
			if event.GetOp() == "supersede_step_run" {
				Expect(event.GetAfter().AsMap()).To(HaveKeyWithValue("reason", "rerun"))
				reruns++
			}
		}
		Expect(reruns).To(Equal(3), "one superseded attempt per step of the span")

		// And the next run resumes at implement in generation 1. The step a
		// resume starts at is derived from the ledger and nowhere else
		// (docs/ledger.md Invariant 10), so what the run is now on is what
		// the next claim will begin with.
		Expect(r.cli(ctx, "explain", "millcli#60")).To(ContainSubstring("implement"))
		Expect(detail.Msg.GetSummary().GetCurrentStep()).To(Equal("implement"))
		Expect(r.reload(ctx).GetGeneration()).To(Equal(int64(1)),
			"a rerun keeps the generation; restart is what opens a new one")
	})

	It("refuses a run a worker is still holding, naming the holder", func() {
		// A run that has not parked still holds the lease of the claim that
		// opened it, and the worker holding it is in the middle of a step.
		r := openRun(ctx, "millcli#61", nodeID(61))
		r.closeGatherContext(ctx, "sha256:"+strings.Repeat("a", 64))

		out, err := runCLI(ctx, r.mill.socket, "rerun", "millcli#61", "--from", "gather-context")
		Expect(err).To(HaveOccurred())
		Expect(out).To(ContainSubstring("spec@host"), "the refusal names the holder")
		Expect(out).To(ContainSubstring(r.lease))

		detail, err2 := r.store.Inspect(ctx, connect.NewRequest(&millv1.InspectRequest{Id: r.run.GetId()}))
		Expect(err2).NotTo(HaveOccurred())
		Expect(statusesOf(detail.Msg)["gather-context"]).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED),
			"the refused rerun changed nothing")
	})

	It("refuses a --from naming a step the run never attempted, naming the ones it did", func() {
		r := parkedAtCleanTree(ctx, "millcli#62", 62)

		out, err := runCLI(ctx, r.mill.socket, "rerun", "millcli#62", "--from", "pr")
		Expect(err).To(HaveOccurred())
		Expect(out).To(ContainSubstring("pr"))
		for _, step := range []string{"gather-context", "implement", "commit", "clean-tree"} {
			Expect(out).To(ContainSubstring(step), "the refusal names %s, which was attempted", step)
		}
	})

	It("reopens a failed run and leaves it resumable at the named step", func() {
		// The criterion is "a parked or failed run". Failure is terminal to
		// the run loop, so a rerun on one is a person overruling the
		// ending: the span is superseded and the run is made live again,
		// holding no lease, which is the state a claim resumes.
		r := failedAtCleanTree(ctx, "millcli#64", 64)
		Expect(r.reload(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_FAILED))

		out := r.cli(ctx, "rerun", "millcli#64", "--from", "implement")
		Expect(out).To(ContainSubstring("from implement through clean-tree"))
		Expect(out).To(ContainSubstring("is live again"))

		detail, err := r.store.Inspect(ctx, connect.NewRequest(&millv1.InspectRequest{Id: r.run.GetId()}))
		Expect(err).NotTo(HaveOccurred())
		statuses := statusesOf(detail.Msg)
		for _, step := range []string{"implement", "commit", "clean-tree"} {
			Expect(statuses[step]).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_SUPERSEDED), "step %s", step)
		}
		Expect(detail.Msg.GetSummary().GetCurrentStep()).To(Equal("implement"))

		// Resumable: a claim is admitted, and it resumes this run rather
		// than opening a generation over it.
		row := r.reload(ctx)
		Expect(row.GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_STOPPED))
		Expect(row.GetGeneration()).To(Equal(int64(1)))
		claimed, err := r.store.SetLease(ctx, connect.NewRequest(&millv1.SetLeaseRequest{
			RunId: r.run.GetId(), LeaseId: "01HZZZZZZZZZZZZZZZZZZZZZZA", WorkerRef: "spec@host",
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(claimed.Msg.GetRun().GetGeneration()).To(Equal(int64(1)))
	})

	It("refuses a run a later generation has replaced, and says which verb opens one", func() {
		// An `abandoned` run has been superseded by a restart. Reviving it
		// would put two live runs on one node, so the rerun is refused.
		r := parkedAtCleanTree(ctx, "millcli#65", 65)
		_, err := r.store.Abandon(ctx, connect.NewRequest(&millv1.AbandonRequest{
			RunId: r.run.GetId(), Reason: "restarted",
		}))
		Expect(err).NotTo(HaveOccurred())

		out, err := runCLI(ctx, r.mill.socket, "rerun", "millcli#65", "--from", "implement")
		Expect(err).To(HaveOccurred())
		Expect(out).To(ContainSubstring("later generation"))
		Expect(out).To(ContainSubstring("mill restart"))
	})

	It("is wired into the tree and needs a step to redo from", func() {
		r := parkedAtCleanTree(ctx, "millcli#63", 63)
		_, err := runCLI(ctx, r.mill.socket, "rerun", "millcli#63")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("--from"))
	})
})
