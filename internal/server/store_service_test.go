package server_test

import (
	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/timestamppb"

	harnessv1 "github.com/ivy/mill/gen/harness/v1"
	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/testsupport"
)

var _ = Describe("The store's handlers", Label(testsupport.DBLabel), func() {
	It("GetRunByNode returns the live run, and nothing for a node with none", func(ctx SpecContext) {
		f := newFixture(ctx, "mill#1")

		got, err := f.client().GetRunByNode(ctx, connect.NewRequest(&millv1.GetRunByNodeRequest{NodeId: f.run.GetNodeId()}))
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Msg.GetRun().GetId()).To(Equal(f.run.GetId()))

		none, err := f.client().GetRunByNode(ctx, connect.NewRequest(&millv1.GetRunByNodeRequest{NodeId: "node-unclaimed"}))
		Expect(err).NotTo(HaveOccurred())
		Expect(none.Msg.GetRun()).To(BeNil())
	})

	It("CreateRun opens a run in status selecting", func(ctx SpecContext) {
		f := newService(ctx)
		created, err := f.client().CreateRun(ctx, connect.NewRequest(&millv1.CreateRunRequest{
			LeaseId:   lease,
			NodeId:    "node-mill#2",
			NodeRef:   "mill#2",
			ContextId: "ctx-mill#2",
			WorkerRef: "worker-1",
			Branch:    "mill/mill#2",
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(created.Msg.GetRun().GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_SELECTING))
		Expect(created.Msg.GetRun().GetLeaseId()).To(Equal(lease))
	})

	It("SetLease takes a run over under a new lease", func(ctx SpecContext) {
		f := newFixture(ctx, "mill#3")
		taken, err := f.client().SetLease(ctx, connect.NewRequest(&millv1.SetLeaseRequest{
			RunId:     f.run.GetId(),
			LeaseId:   "lease-2",
			WorkerRef: "worker-2",
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(taken.Msg.GetRun().GetLeaseId()).To(Equal("lease-2"))
		Expect(taken.Msg.GetRun().GetWorkerRef()).To(Equal("worker-2"))
	})

	It("StartStepRun opens the next attempt at a step", func(ctx SpecContext) {
		f := newFixture(ctx, "mill#4")
		stepRun := f.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)
		Expect(stepRun.GetAttempt()).To(BeEquivalentTo(1))
		Expect(stepRun.GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_RUNNING))
		Expect(stepRun.GetLeaseId()).To(Equal(lease))
	})

	It("CloseStepRun records the evidence that closes an attempt", func(ctx SpecContext) {
		f := newFixture(ctx, "mill#5")
		stepRun := f.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)
		closed, err := f.client().CloseStepRun(ctx, connect.NewRequest(&millv1.CloseStepRunRequest{
			LeaseId:   lease,
			StepRunId: stepRun.GetId(),
			Evidence:  commitEvidence(),
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(closed.Msg.GetStepRun().GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED))
		Expect(closed.Msg.GetEvidence().GetCommit()).To(Equal(sha))
	})

	It("FailStepRun marks an attempt failed with how its process ended", func(ctx SpecContext) {
		f := newFixture(ctx, "mill#6")
		stepRun := f.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)
		failed, err := f.client().FailStepRun(ctx, connect.NewRequest(&millv1.FailStepRunRequest{
			LeaseId:   lease,
			StepRunId: stepRun.GetId(),
			Exit:      &harnessv1.ProcessExit{Status: 1},
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(failed.Msg.GetStepRun().GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_FAILED))
		Expect(failed.Msg.GetStepRun().GetExit().GetStatus()).To(BeEquivalentTo(1))
	})

	It("AbortOpenStepRuns marks a dead worker's attempt aborted", func(ctx SpecContext) {
		f := newFixture(ctx, "mill#7")
		stepRun := f.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)
		aborted, err := f.client().AbortOpenStepRuns(ctx, connect.NewRequest(&millv1.AbortOpenStepRunsRequest{
			LeaseId: lease,
			RunId:   f.run.GetId(),
			Reason:  "crashed",
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(aborted.Msg.GetStepRuns()).To(HaveLen(1))
		Expect(aborted.Msg.GetStepRuns()[0].GetId()).To(Equal(stepRun.GetId()))
		Expect(aborted.Msg.GetStepRuns()[0].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_ABORTED))
	})

	It("SupersedeSteps reopens a span of steps for a revise", func(ctx SpecContext) {
		f := newFixture(ctx, "mill#8")
		implement := f.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)
		_, err := f.client().CloseStepRun(ctx, connect.NewRequest(&millv1.CloseStepRunRequest{
			LeaseId: lease, StepRunId: implement.GetId(), Evidence: commitEvidence(),
		}))
		Expect(err).NotTo(HaveOccurred())
		review := f.start(ctx, "review", millv1.StepKind_STEP_KIND_SKILL)
		_, err = f.client().FailStepRun(ctx, connect.NewRequest(&millv1.FailStepRunRequest{
			LeaseId: lease, StepRunId: review.GetId(), Exit: &harnessv1.ProcessExit{Status: 0},
		}))
		Expect(err).NotTo(HaveOccurred())

		superseded, err := f.client().SupersedeSteps(ctx, connect.NewRequest(&millv1.SupersedeStepsRequest{
			LeaseId:     lease,
			RunId:       f.run.GetId(),
			FromStep:    "implement",
			ThroughStep: "review",
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(superseded.Msg.GetStepRuns()).To(HaveLen(2))
		for _, stepRun := range superseded.Msg.GetStepRuns() {
			Expect(stepRun.GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_SUPERSEDED))
		}
	})

	It("MarkWaiting records the gate a run is blocked on", func(ctx SpecContext) {
		f := newFixture(ctx, "mill#9")
		waiting := f.wait(ctx)
		Expect(waiting.GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_WAITING))
		Expect(waiting.GetGateNodeId()).To(Equal("gate-node"))
		Expect(waiting.GetLeaseId()).To(BeEmpty(), "a waiting run holds no lease")
	})

	It("Park trips the circuit breaker with the reason that stopped the run", func(ctx SpecContext) {
		f := newFixture(ctx, "mill#10")
		parked, err := f.client().Park(ctx, connect.NewRequest(&millv1.ParkRequest{
			LeaseId:    lease,
			RunId:      f.run.GetId(),
			GateNodeId: "gate-node",
			Reason:     millv1.ParkedReason_PARKED_REASON_RETRIES,
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(parked.Msg.GetRun().GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_PARKED))
		Expect(parked.Msg.GetRun().GetParkedReason()).To(Equal(millv1.ParkedReason_PARKED_REASON_RETRIES))
	})

	It("Done marks a run done", func(ctx SpecContext) {
		f := newFixture(ctx, "mill#11")
		done, err := f.client().Done(ctx, connect.NewRequest(&millv1.DoneRequest{
			LeaseId: lease, RunId: f.run.GetId(),
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(done.Msg.GetRun().GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_DONE))
		Expect(done.Msg.GetRun().GetEndedAt()).NotTo(BeNil())
	})

	It("Abandon marks a run abandoned so a new generation can be created", func(ctx SpecContext) {
		f := newFixture(ctx, "mill#12")
		abandoned, err := f.client().Abandon(ctx, connect.NewRequest(&millv1.AbandonRequest{
			LeaseId: lease, RunId: f.run.GetId(), Reason: "restart",
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(abandoned.Msg.GetRun().GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_ABANDONED))
	})

	It("Cancel records the request on the run without moving it", func(ctx SpecContext) {
		f := newFixture(ctx, "mill#13")
		cancelled, err := f.client().Cancel(ctx, connect.NewRequest(&millv1.CancelRequest{
			RunId: f.run.GetId(), Reason: "operator asked",
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(cancelled.Msg.GetRun().GetCancelRequested()).To(BeTrue())
		Expect(cancelled.Msg.GetRun().GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_RUNNING),
			"the worker is what stops the run; the flag only asks it to")
	})

	It("Cancel is refused on a run that has ended", func(ctx SpecContext) {
		f := newFixture(ctx, "mill#14")
		_, err := f.client().Done(ctx, connect.NewRequest(&millv1.DoneRequest{LeaseId: lease, RunId: f.run.GetId()}))
		Expect(err).NotTo(HaveOccurred())

		_, err = f.client().Cancel(ctx, connect.NewRequest(&millv1.CancelRequest{RunId: f.run.GetId()}))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
		Expect(err.Error()).To(ContainSubstring("done"))
	})

	It("IssueStepToken mints a bearer and RevokeStepToken revokes it", func(ctx SpecContext) {
		f := newFixture(ctx, "mill#15")
		stepRun := f.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)
		issued, err := f.client().IssueStepToken(ctx, connect.NewRequest(&millv1.IssueStepTokenRequest{
			LeaseId:      lease,
			StepRunId:    stepRun.GetId(),
			Capabilities: []millv1.Capability{millv1.Capability_CAPABILITY_READ, millv1.Capability_CAPABILITY_WRITE},
		}))
		Expect(err).NotTo(HaveOccurred())
		token := issued.Msg.GetStepToken()
		Expect(token.GetToken()).NotTo(BeEmpty(), "the bearer is returned once, here")
		Expect(token.GetCapabilities()).To(ConsistOf(
			millv1.Capability_CAPABILITY_READ, millv1.Capability_CAPABILITY_WRITE))

		revoked, err := f.client().RevokeStepToken(ctx, connect.NewRequest(&millv1.RevokeStepTokenRequest{
			LeaseId: lease, StepTokenId: token.GetId(),
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(revoked.Msg.GetStepToken().GetRevokedAt()).NotTo(BeNil())
		Expect(revoked.Msg.GetStepToken().GetToken()).To(BeEmpty(), "the bearer is never returned again")
	})

	It("RecordUsage appends what a turn cost and MarkUsagePosted marks it reported", func(ctx SpecContext) {
		f := newFixture(ctx, "mill#16")
		stepRun := f.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)
		recorded, err := f.client().RecordUsage(ctx, connect.NewRequest(&millv1.RecordUsageRequest{
			LeaseId:   lease,
			StepRunId: stepRun.GetId(),
			Usage: []*millv1.Usage{
				{Model: "opus", InputTokens: 10, OutputTokens: 2, CacheReadTokens: 1, CacheWriteTokens: 3},
			},
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(recorded.Msg.GetUsage()).To(HaveLen(1))
		Expect(recorded.Msg.GetUsage()[0].GetModel()).To(Equal("opus"))

		posted, err := f.client().MarkUsagePosted(ctx, connect.NewRequest(&millv1.MarkUsagePostedRequest{
			LeaseId: lease, StepRunId: stepRun.GetId(),
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(posted.Msg.GetMarked()).To(BeEquivalentTo(1))
	})

	It("SealAttempt inserts index rows once and never double-counts a retry", func(ctx SpecContext) {
		f := newFixture(ctx, "mill#40")
		stepRun := f.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)
		rows := []*millv1.ActivityIndexRow{
			{Seq: 1, At: timestamppb.Now(), Kind: "text", Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS, Bytes: 12},
			{Seq: 2, At: timestamppb.Now(), Kind: "tool_call", Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS, CallId: "call-1", Tool: "bash", Bytes: 40},
		}

		sealed, err := f.client().SealAttempt(ctx, connect.NewRequest(&millv1.SealAttemptRequest{
			StepRunId: stepRun.GetId(), Records: rows,
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(sealed.Msg.GetRecords()).To(HaveLen(2))
		Expect(sealed.Msg.GetRecords()[1].GetTool()).To(Equal("bash"))

		// A retry (the worker resending after a lost response) reports a
		// mix of the same rows and one genuinely new one: the already-
		// recorded seqs insert nothing new, and only the new one comes
		// back.
		retried, err := f.client().SealAttempt(ctx, connect.NewRequest(&millv1.SealAttemptRequest{
			StepRunId: stepRun.GetId(), Records: append(rows, &millv1.ActivityIndexRow{
				Seq: 3, At: timestamppb.Now(), Kind: "text", Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS,
			}),
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(retried.Msg.GetRecords()).To(HaveLen(1), "only the seq the first call never recorded comes back")
		Expect(retried.Msg.GetRecords()[0].GetSeq()).To(BeEquivalentTo(3))
	})

	It("PutWorkflow is idempotent by content hash and GetWorkflow returns the version", func(ctx SpecContext) {
		f := newFixture(ctx, "mill#17")
		again, err := f.client().PutWorkflow(ctx, connect.NewRequest(&millv1.PutWorkflowRequest{
			Workflow: &millv1.Workflow{Name: "feature", Scope: millv1.Scope_SCOPE_ORG, Steps: steps},
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(again.Msg.GetCreated()).To(BeFalse())
		Expect(again.Msg.GetWorkflow().GetId()).To(Equal(f.workflow.GetId()))

		got, err := f.client().GetWorkflow(ctx, connect.NewRequest(&millv1.GetWorkflowRequest{Id: f.workflow.GetId()}))
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Msg.GetWorkflow().GetVersion()).To(Equal(f.workflow.GetVersion()))
	})

	It("ListWorkflows lists the versions in a scope", func(ctx SpecContext) {
		f := newFixture(ctx, "mill#18")
		listed, err := f.client().ListWorkflows(ctx, connect.NewRequest(&millv1.ListWorkflowsRequest{
			Scope: millv1.Scope_SCOPE_ORG,
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(listed.Msg.GetWorkflows()).To(HaveLen(1))
		Expect(listed.Msg.GetWorkflows()[0].GetName()).To(Equal("feature"))
	})

	It("PutSkill stores a skill, idempotent by digest", func(ctx SpecContext) {
		f := newService(ctx)
		skill := &millv1.Skill{Name: "implement", Digest: "sha256:abc", Body: "do the work"}
		stored, err := f.client().PutSkill(ctx, connect.NewRequest(&millv1.PutSkillRequest{Skill: skill}))
		Expect(err).NotTo(HaveOccurred())
		Expect(stored.Msg.GetCreated()).To(BeTrue())

		again, err := f.client().PutSkill(ctx, connect.NewRequest(&millv1.PutSkillRequest{Skill: skill}))
		Expect(err).NotTo(HaveOccurred())
		Expect(again.Msg.GetCreated()).To(BeFalse())
		Expect(again.Msg.GetSkill().GetId()).To(Equal(stored.Msg.GetSkill().GetId()))
	})

	It("Status lists runs by state with the step each is on", func(ctx SpecContext) {
		f := newFixture(ctx, "mill#19")
		f.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)

		listed, err := f.client().Status(ctx, connect.NewRequest(&millv1.StatusRequest{}))
		Expect(err).NotTo(HaveOccurred())
		Expect(listed.Msg.GetRuns()).To(HaveLen(1))
		Expect(listed.Msg.GetRuns()[0].GetRun().GetId()).To(Equal(f.run.GetId()))
		Expect(listed.Msg.GetRuns()[0].GetCurrentStep()).To(Equal("implement"))
	})

	It("Inspect returns a run's every attempt with its evidence", func(ctx SpecContext) {
		f := newFixture(ctx, "mill#20")
		stepRun := f.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)
		_, err := f.client().CloseStepRun(ctx, connect.NewRequest(&millv1.CloseStepRunRequest{
			LeaseId: lease, StepRunId: stepRun.GetId(), Evidence: commitEvidence(),
		}))
		Expect(err).NotTo(HaveOccurred())

		detail, err := f.client().Inspect(ctx, connect.NewRequest(&millv1.InspectRequest{Id: f.run.GetId()}))
		Expect(err).NotTo(HaveOccurred())
		Expect(detail.Msg.GetWorkflow().GetId()).To(Equal(f.workflow.GetId()))
		Expect(detail.Msg.GetSteps()).To(HaveLen(2), "the selection step and the implement step")
		Expect(detail.Msg.GetSteps()[1].GetEvidence().GetCommit()).To(Equal(sha))
	})

	It("Inspect with neither a run id nor a node ref is refused as invalid", func(ctx SpecContext) {
		f := newService(ctx)
		_, err := f.client().Inspect(ctx, connect.NewRequest(&millv1.InspectRequest{}))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
	})

	It("a run that does not exist is not found", func(ctx SpecContext) {
		f := newService(ctx)
		_, err := f.client().Inspect(ctx, connect.NewRequest(&millv1.InspectRequest{Id: "01ARZ3NDEKTSV4RRFFQ69G5FAV"}))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))
	})
})

var _ = Describe("A write from a worker that is no longer the lease holder", Label(testsupport.DBLabel), func() {
	It("is refused as failed_precondition naming the presented and the current lease", func(ctx SpecContext) {
		f := newFixture(ctx, "mill#21")
		_, err := f.client().SetLease(ctx, connect.NewRequest(&millv1.SetLeaseRequest{
			RunId: f.run.GetId(), LeaseId: "lease-2", WorkerRef: "worker-2",
		}))
		Expect(err).NotTo(HaveOccurred())

		_, err = f.client().StartStepRun(ctx, connect.NewRequest(&millv1.StartStepRunRequest{
			LeaseId:   lease,
			RunId:     f.run.GetId(),
			StepName:  "implement",
			Kind:      millv1.StepKind_STEP_KIND_SKILL,
			WorkerRef: "worker-1",
		}))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
		Expect(err.Error()).To(ContainSubstring("lease-2"), "the refusal names the run's current lease")
		Expect(err.Error()).To(ContainSubstring(lease), "the refusal names the lease the write presented")
	})
})
