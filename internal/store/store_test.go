package store_test

import (
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	harnessv1 "github.com/steading-ai/mill/gen/harness/v1"
	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/store"
	"github.com/steading-ai/mill/internal/testsupport"
)

var _ = Describe("SetLease", Label(testsupport.DBLabel), func() {
	// The statuses a new claim may take over, from Invariant 9. Whether a
	// running run's lease has expired is skift's judgement: it hands out a
	// second lease only once the first is gone.
	DescribeTable("takes a run over only from a status a new claim may take",
		func(ctx SpecContext, reach func(*ledger, SpecContext) *millv1.Run, accepted bool) {
			l := newLedger(ctx)
			run := reach(l, ctx)
			claimed, err := l.store.SetLease(ctx, run.GetId(), "L2", "worker-2")
			if !accepted {
				Expect(err).To(MatchError(store.ErrNotClaimable))
				Expect(err).To(MatchError(ContainSubstring("is "+statusText(run.GetStatus()))),
					"the refusal names the status")
				return
			}
			Expect(err).NotTo(HaveOccurred())
			Expect(claimed.GetLeaseId()).To(Equal("L2"))
			Expect(claimed.GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_RUNNING))
			Expect(claimed.GetWorkerRef()).To(Equal("worker-2"))
		},
		Entry("running, whose lease skift has already replaced", func(l *ledger, ctx SpecContext) *millv1.Run {
			run, _ := l.runWithWorkflow(ctx, "mill#8")
			return run
		}, true),
		Entry("waiting on a gate", func(l *ledger, ctx SpecContext) *millv1.Run {
			run, _ := l.runWithWorkflow(ctx, "mill#8")
			open := l.startKind(ctx, run.GetId(), "review", millv1.StepKind_STEP_KIND_GATE)
			waiting, err := l.store.MarkWaiting(ctx, l.lease, run.GetId(), "gate-1", open.GetId())
			Expect(err).NotTo(HaveOccurred())
			return waiting
		}, true),
		Entry("parked by the circuit breaker", func(l *ledger, ctx SpecContext) *millv1.Run {
			run, _ := l.runWithWorkflow(ctx, "mill#8")
			parked, err := l.store.Park(ctx, l.lease, run.GetId(), "gate-1",
				millv1.ParkedReason_PARKED_REASON_RETRIES)
			Expect(err).NotTo(HaveOccurred())
			return parked
		}, true),
		Entry("done, with the node still open", func(l *ledger, ctx SpecContext) *millv1.Run {
			run, _ := l.runWithWorkflow(ctx, "mill#8")
			done, err := l.store.Done(ctx, l.lease, run.GetId())
			Expect(err).NotTo(HaveOccurred())
			return done
		}, true),
		Entry("selecting, still holding the lease of the claim that died before it chose a workflow", func(l *ledger, ctx SpecContext) *millv1.Run {
			return l.run(ctx, "mill#8")
		}, true),
		Entry("selecting, opened by a restart and holding no lease", func(l *ledger, ctx SpecContext) *millv1.Run {
			run, err := l.store.CreateRun(ctx, "", store.CreateRunParams{
				NodeID: "node-restarted", NodeRef: "mill#9", Generation: 2, Branch: "mill/9",
			})
			Expect(err).NotTo(HaveOccurred())
			return run
		}, true),
		Entry("failed", func(l *ledger, ctx SpecContext) *millv1.Run {
			run, _ := l.runWithWorkflow(ctx, "mill#8")
			failed, err := l.store.Failed(ctx, l.lease, run.GetId(), "denied")
			Expect(err).NotTo(HaveOccurred())
			return failed
		}, false),
		Entry("abandoned by a restart", func(l *ledger, ctx SpecContext) *millv1.Run {
			run, _ := l.runWithWorkflow(ctx, "mill#8")
			abandoned, err := l.store.Abandon(ctx, l.lease, run.GetId(), "restart")
			Expect(err).NotTo(HaveOccurred())
			return abandoned
		}, false),
	)

	// Scenario: The next generation counts the abandoned one
	It("The next generation counts the abandoned one", func(ctx SpecContext) {
		l := newLedger(ctx)
		By("given a node that has never run")
		Expect(l.store.NextGeneration(ctx, "node-never")).To(Equal(int64(1)))

		By("and a node whose only run a restart abandoned")
		run, _ := l.runWithWorkflow(ctx, "mill#8")
		_, err := l.store.Abandon(ctx, l.lease, run.GetId(), "restart")
		Expect(err).NotTo(HaveOccurred())

		By("when the next generation is asked for")
		next, err := l.store.NextGeneration(ctx, run.GetNodeId())
		Expect(err).NotTo(HaveOccurred())

		By("then it is one past the abandoned generation, which GetRunByNode no longer returns")
		Expect(next).To(Equal(int64(2)))
		live, err := l.store.GetRunByNode(ctx, run.GetNodeId())
		Expect(err).NotTo(HaveOccurred())
		Expect(live).To(BeNil())
	}, SpecTimeout(time.Minute))

	It("makes the new lease what later writes are compared against", func(ctx SpecContext) {
		l := newLedger(ctx)
		run, _ := l.runWithWorkflow(ctx, "mill#8")
		_, err := l.store.SetLease(ctx, run.GetId(), "L2", "worker-2")
		Expect(err).NotTo(HaveOccurred())

		_, err = l.store.StartStepRun(ctx, "L2", store.StartStepRunParams{
			RunID: run.GetId(), StepName: "implement", Kind: millv1.StepKind_STEP_KIND_SKILL,
		})
		Expect(err).NotTo(HaveOccurred())
	}, SpecTimeout(time.Minute))

	It("clears what the previous attempt left behind, and keeps the gate", func(ctx SpecContext) {
		l := newLedger(ctx)
		run, _ := l.runWithWorkflow(ctx, "mill#8")
		parked, err := l.store.Park(ctx, l.lease, run.GetId(), "gate-1",
			millv1.ParkedReason_PARKED_REASON_BUDGET)
		Expect(err).NotTo(HaveOccurred())
		Expect(parked.GetEndedAt()).To(BeNil())

		claimed, err := l.store.SetLease(ctx, run.GetId(), "L2", "worker-2")
		Expect(err).NotTo(HaveOccurred())
		Expect(claimed.GetParkedReason()).To(Equal(millv1.ParkedReason_PARKED_REASON_UNSPECIFIED))
		Expect(claimed.GetGateNodeId()).To(Equal("gate-1"), "a resume through a gate reads it")
	}, SpecTimeout(time.Minute))
})

// ReleaseLease is a graceful drain's release (internal/run/shutdown.go's
// drainStep): the run stays running, holding no lease, so a reclaim or
// another worker's ClaimNext can pick it up at its next step boundary
// (docs/ledger.md §Resume). Unlike SetLease it never takes a run over —
// it only gives one back — so it is fenced like an ordinary write rather
// than checking [claimableStatuses].
var _ = Describe("ReleaseLease", Label(testsupport.DBLabel), func() {
	It("gives back a running run's lease and leaves it running", func(ctx SpecContext) {
		l := newLedger(ctx)
		run, _ := l.runWithWorkflow(ctx, "mill#8")

		released, err := l.store.ReleaseLease(ctx, l.lease, run.GetId())
		Expect(err).NotTo(HaveOccurred())
		Expect(released.GetLeaseId()).To(BeEmpty())
		Expect(released.GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_RUNNING))
	}, SpecTimeout(time.Minute))

	It("refuses a lease that is not the run's current one", func(ctx SpecContext) {
		l := newLedger(ctx)
		run, _ := l.runWithWorkflow(ctx, "mill#8")

		_, err := l.store.ReleaseLease(ctx, "not-the-lease", run.GetId())
		Expect(err).To(MatchError(store.ErrFenced))

		unchanged, err := l.store.GetRun(ctx, run.GetId())
		Expect(err).NotTo(HaveOccurred())
		Expect(unchanged.GetLeaseId()).To(Equal(l.lease), "a fenced-out write writes nothing")
	}, SpecTimeout(time.Minute))

	It("refuses a run that is not running, rather than silently succeeding", func(ctx SpecContext) {
		l := newLedger(ctx)
		// l.run leaves a fresh run selecting, still holding l.lease — the
		// one status a matching lease can reach that is not running,
		// since every later status (waiting, parked, done...) clears
		// lease_id on its way out.
		run := l.run(ctx, "mill#9")
		Expect(run.GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_SELECTING))

		_, err := l.store.ReleaseLease(ctx, l.lease, run.GetId())
		Expect(err).To(HaveOccurred())
		Expect(errors.Is(err, pgx.ErrNoRows)).To(BeTrue(),
			"the query's own status = 'running' guard is what refuses it")

		unchanged, err := l.store.GetRun(ctx, run.GetId())
		Expect(err).NotTo(HaveOccurred())
		Expect(unchanged.GetLeaseId()).To(Equal(l.lease), "refused, not silently released")
	}, SpecTimeout(time.Minute))
})

var _ = Describe("Usage", Label(testsupport.DBLabel), func() {
	It("RecordUsage accepts two rows for one turn with different models", func(ctx SpecContext) {
		l := newLedger(ctx)
		run, _ := l.runWithWorkflow(ctx, "mill#8")
		open := l.start(ctx, run.GetId(), "implement")

		recorded, err := l.store.RecordUsage(ctx, l.lease, open.GetId(), []*millv1.Usage{
			{Model: "claude-opus-5", InputTokens: 100, OutputTokens: 20, CacheReadTokens: 5, CacheWriteTokens: 1},
			{Model: "claude-haiku-4", InputTokens: 7, OutputTokens: 3},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(recorded).To(HaveLen(2))
		Expect(recorded[0].GetModel()).To(Equal("claude-opus-5"))
		Expect(recorded[0].GetInputTokens()).To(Equal(int64(100)))
		Expect(recorded[1].GetModel()).To(Equal("claude-haiku-4"))
		Expect(recorded[0].GetPostedAt()).To(BeNil())
	}, SpecTimeout(time.Minute))

	It("MarkUsagePosted marks exactly the rows for one step run", func(ctx SpecContext) {
		l := newLedger(ctx)
		run, _ := l.runWithWorkflow(ctx, "mill#8")
		first := l.start(ctx, run.GetId(), "implement")
		_, err := l.store.RecordUsage(ctx, l.lease, first.GetId(), []*millv1.Usage{
			{Model: "opus", InputTokens: 1}, {Model: "haiku", InputTokens: 2},
		})
		Expect(err).NotTo(HaveOccurred())
		_, _, err = l.store.CloseStepRun(ctx, l.lease, store.CloseStepRunParams{
			StepRunID: first.GetId(), Evidence: commitEvidence("sha")})
		Expect(err).NotTo(HaveOccurred())

		second := l.start(ctx, run.GetId(), "simplify")
		_, err = l.store.RecordUsage(ctx, l.lease, second.GetId(),
			[]*millv1.Usage{{Model: "opus", InputTokens: 9}})
		Expect(err).NotTo(HaveOccurred())

		marked, err := l.store.MarkUsagePosted(ctx, l.lease, first.GetId())
		Expect(err).NotTo(HaveOccurred())
		Expect(marked).To(Equal(int64(2)))

		var unposted int
		Expect(l.db.QueryRow(ctx,
			`SELECT count(*) FROM usage WHERE step_run_id = $1 AND posted_at IS NULL`, second.GetId()).
			Scan(&unposted)).To(Succeed())
		Expect(unposted).To(Equal(1), "the other attempt's rows are untouched")

		// A second call marks nothing: the rows are posted already.
		marked, err = l.store.MarkUsagePosted(ctx, l.lease, first.GetId())
		Expect(err).NotTo(HaveOccurred())
		Expect(marked).To(BeZero())
	}, SpecTimeout(time.Minute))
})

var _ = Describe("Resume", Label(testsupport.DBLabel), func() {
	It("returns the run a claim on the node resumes, and nothing for a node with none", func(ctx SpecContext) {
		l := newLedger(ctx)
		run := l.run(ctx, "mill#8")

		found, err := l.store.GetRunByNode(ctx, run.GetNodeId())
		Expect(err).NotTo(HaveOccurred())
		Expect(found.GetId()).To(Equal(run.GetId()))

		missing, err := l.store.GetRunByNode(ctx, "node-nothing")
		Expect(err).NotTo(HaveOccurred())
		Expect(missing).To(BeNil())

		// A restart retires the run, and the node reads as free until the
		// next generation is created.
		_, err = l.store.Abandon(ctx, l.lease, run.GetId(), "restart")
		Expect(err).NotTo(HaveOccurred())
		retired, err := l.store.GetRunByNode(ctx, run.GetNodeId())
		Expect(err).NotTo(HaveOccurred())
		Expect(retired).To(BeNil())
	}, SpecTimeout(time.Minute))

	It("aborts the attempts of a dead worker and revokes their tokens", func(ctx SpecContext) {
		l := newLedger(ctx)
		run, workflow := l.runWithWorkflow(ctx, "mill#8")
		open := l.start(ctx, run.GetId(), "implement")
		token, err := l.store.IssueStepToken(ctx, l.lease, open.GetId(), nil)
		Expect(err).NotTo(HaveOccurred())

		// The next worker holds a lease the store has never seen, which is
		// why this call is admitted by status rather than by the fence.
		aborted, err := l.store.AbortOpenStepRuns(ctx, "L2", run.GetId(), "crashed")
		Expect(err).NotTo(HaveOccurred())
		Expect(namesOf(aborted)).To(Equal([]string{"implement"}))
		Expect(l.statusOf(ctx, open.GetId())).To(Equal("aborted"))

		var revoked *time.Time
		Expect(l.db.QueryRow(ctx, `SELECT revoked_at FROM step_tokens WHERE id = $1`, token.GetId()).
			Scan(&revoked)).To(Succeed())
		Expect(revoked).NotTo(BeNil())

		// The aborted attempt does not close the step: the next worker
		// starts a new one from the top.
		Expect(l.currentStep(ctx, run.GetId(), workflow)).To(Equal("implement"))
		_, err = l.store.SetLease(ctx, run.GetId(), "L2", "worker-2")
		Expect(err).NotTo(HaveOccurred())
		next, err := l.store.StartStepRun(ctx, "L2", store.StartStepRunParams{
			RunID: run.GetId(), StepName: "implement", Kind: millv1.StepKind_STEP_KIND_SKILL,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(next.GetAttempt()).To(Equal(int64(2)))
	}, SpecTimeout(time.Minute))

	It("records how a failed attempt's process ended", func(ctx SpecContext) {
		l := newLedger(ctx)
		run, _ := l.runWithWorkflow(ctx, "mill#8")
		open := l.start(ctx, run.GetId(), "implement")
		failed, err := l.store.FailStepRun(ctx, l.lease, open.GetId(), &harnessv1.ProcessExit{
			Status: -1, Reason: harnessv1.ExitReason_EXIT_REASON_TIMEOUT,
		}, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(failed.GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_FAILED))
		Expect(failed.GetExit().GetStatus()).To(Equal(int32(-1)))
		Expect(failed.GetExit().GetReason()).To(Equal(harnessv1.ExitReason_EXIT_REASON_TIMEOUT))

		_, err = l.store.FailStepRun(ctx, l.lease, open.GetId(), nil, nil)
		Expect(err).To(MatchError(store.ErrNotRunning))
	}, SpecTimeout(time.Minute))

	It("records a revise verdict as the evidence of the attempt it failed", func(ctx SpecContext) {
		l := newLedger(ctx)
		run, _ := l.runWithWorkflow(ctx, "mill#8")
		open := l.start(ctx, run.GetId(), "review")
		failed, err := l.store.FailStepRun(ctx, l.lease, open.GetId(), nil,
			reviseEvidence("AC-1", "the plan names no proof"))
		Expect(err).NotTo(HaveOccurred())
		Expect(failed.GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_FAILED))

		detail, err := l.store.Inspect(ctx, store.InspectParams{ID: run.GetId()})
		Expect(err).NotTo(HaveOccurred())
		var found *millv1.Evidence
		for _, step := range detail.Steps {
			if step.GetStepRun().GetId() == open.GetId() {
				found = step.GetEvidence()
			}
		}
		Expect(found.GetKind()).To(Equal(millv1.EvidenceKind_EVIDENCE_KIND_VERDICT))
		Expect(found.GetVerdict().GetDecision()).To(Equal(millv1.VerdictDecision_VERDICT_DECISION_REVISE))
		Expect(found.GetVerdict().GetFindings()[0].GetSummary()).To(Equal("the plan names no proof"))

		// Failing again with no evidence is unaffected: a plain failure still
		// leaves no row.
		_, err = l.store.FailStepRun(ctx, l.lease, open.GetId(), nil, nil)
		Expect(err).To(MatchError(store.ErrNotRunning))
	}, SpecTimeout(time.Minute))
})

var _ = Describe("The catalog", Label(testsupport.DBLabel), func() {
	It("stores a workflow version once, by content hash", func(ctx SpecContext) {
		l := newLedger(ctx)
		definition := &millv1.Workflow{
			Name: "feature", Scope: millv1.Scope_SCOPE_ORG,
			Steps: []*millv1.Step{{Name: "implement", Kind: millv1.StepKind_STEP_KIND_SKILL}},
		}
		first, created, err := l.store.PutWorkflow(ctx, definition)
		Expect(err).NotTo(HaveOccurred())
		Expect(created).To(BeTrue())
		Expect(first.GetVersion()).NotTo(BeEmpty())

		again, created, err := l.store.PutWorkflow(ctx, definition)
		Expect(err).NotTo(HaveOccurred())
		Expect(created).To(BeFalse())
		Expect(again.GetId()).To(Equal(first.GetId()))

		changed := &millv1.Workflow{
			Name: "feature", Scope: millv1.Scope_SCOPE_ORG,
			Steps: []*millv1.Step{
				{Name: "implement", Kind: millv1.StepKind_STEP_KIND_SKILL},
				{Name: "review", Kind: millv1.StepKind_STEP_KIND_GATE},
			},
		}
		next, created, err := l.store.PutWorkflow(ctx, changed)
		Expect(err).NotTo(HaveOccurred())
		Expect(created).To(BeTrue())
		Expect(next.GetVersion()).NotTo(Equal(first.GetVersion()))

		newest, err := l.store.GetWorkflow(ctx, store.GetWorkflowParams{
			Name: "feature", Scope: millv1.Scope_SCOPE_ORG})
		Expect(err).NotTo(HaveOccurred())
		Expect(newest.GetId()).To(Equal(next.GetId()))
		Expect(newest.GetSteps()).To(HaveLen(2))

		pinned, err := l.store.GetWorkflow(ctx, store.GetWorkflowParams{
			Name: "feature", Scope: millv1.Scope_SCOPE_ORG, Version: first.GetVersion()})
		Expect(err).NotTo(HaveOccurred())
		Expect(pinned.GetId()).To(Equal(first.GetId()))

		listed, _, err := l.store.ListWorkflows(ctx, store.WorkflowFilter{Name: "feature"})
		Expect(err).NotTo(HaveOccurred())
		Expect(listed).To(HaveLen(1), "the newest of each name unless every version is asked for")
		listed, _, err = l.store.ListWorkflows(ctx, store.WorkflowFilter{Name: "feature", AllVersions: true})
		Expect(err).NotTo(HaveOccurred())
		Expect(listed).To(HaveLen(2))
	}, SpecTimeout(time.Minute))

	It("re-pushing a version wins selection back from one created after it", func(ctx SpecContext) {
		l := newLedger(ctx)
		a := &millv1.Workflow{
			Name: "feature", Scope: millv1.Scope_SCOPE_ORG,
			Steps: []*millv1.Step{{Name: "implement", Kind: millv1.StepKind_STEP_KIND_SKILL}},
		}
		b := &millv1.Workflow{
			Name: "feature", Scope: millv1.Scope_SCOPE_ORG,
			Steps: []*millv1.Step{
				{Name: "implement", Kind: millv1.StepKind_STEP_KIND_SKILL},
				{Name: "review", Kind: millv1.StepKind_STEP_KIND_GATE},
			},
		}
		first, created, err := l.store.PutWorkflow(ctx, a)
		Expect(err).NotTo(HaveOccurred())
		Expect(created).To(BeTrue())

		second, created, err := l.store.PutWorkflow(ctx, b)
		Expect(err).NotTo(HaveOccurred())
		Expect(created).To(BeTrue())

		newest, err := l.store.GetWorkflow(ctx, store.GetWorkflowParams{
			Name: "feature", Scope: millv1.Scope_SCOPE_ORG})
		Expect(err).NotTo(HaveOccurred())
		Expect(newest.GetId()).To(Equal(second.GetId()), "B was pushed after A, so B is newest so far")

		again, created, err := l.store.PutWorkflow(ctx, a)
		Expect(err).NotTo(HaveOccurred())
		Expect(created).To(BeFalse(), "A's content already exists as a version; re-pushing it stores nothing new")
		Expect(again.GetId()).To(Equal(first.GetId()), "still the same row, not a duplicate")

		newest, err = l.store.GetWorkflow(ctx, store.GetWorkflowParams{
			Name: "feature", Scope: millv1.Scope_SCOPE_ORG})
		Expect(err).NotTo(HaveOccurred())
		Expect(newest.GetId()).To(Equal(first.GetId()),
			"a new run of this name pins A: the re-push moved it back ahead of B")

		listed, _, err := l.store.ListWorkflows(ctx, store.WorkflowFilter{Name: "feature", Scope: millv1.Scope_SCOPE_ORG})
		Expect(err).NotTo(HaveOccurred())
		Expect(listed).To(HaveLen(1))
		Expect(listed[0].GetId()).To(Equal(first.GetId()), "`workflow list` shows the re-pushed version first")
	}, SpecTimeout(time.Minute))

	It("touching pushed_at on a re-push is permitted to the application role, updating a workflow's content is not", func(ctx SpecContext) {
		l := newLedger(ctx)
		stored, _, err := l.store.PutWorkflow(ctx, &millv1.Workflow{
			Name: "feature", Scope: millv1.Scope_SCOPE_ORG,
			Steps: []*millv1.Step{{Name: "implement"}},
		})
		Expect(err).NotTo(HaveOccurred())

		app := l.asAppRole(ctx)
		_, err = app.Exec(ctx, `UPDATE workflows SET pushed_at = now() WHERE id = $1`, stored.GetId())
		Expect(err).NotTo(HaveOccurred(), "mill serve's own re-push, run as mill_app, must be able to touch pushed_at")

		_, err = app.Exec(ctx, `UPDATE workflows SET version = 'x' WHERE id = $1`, stored.GetId())
		Expect(err).To(MatchError(ContainSubstring("permission denied for table workflows")),
			"the grant is scoped to pushed_at alone; a workflow's content stays unupdatable")
	}, SpecTimeout(time.Minute))

	It("resolves a project workflow before the org's, and falls back to it", func(ctx SpecContext) {
		l := newLedger(ctx)
		org, _, err := l.store.PutWorkflow(ctx, &millv1.Workflow{
			Name: "feature", Scope: millv1.Scope_SCOPE_ORG,
			Steps: []*millv1.Step{{Name: "implement"}}})
		Expect(err).NotTo(HaveOccurred())
		project, _, err := l.store.PutWorkflow(ctx, &millv1.Workflow{
			Name: "feature", Scope: millv1.Scope_SCOPE_PROJECT, Project: "mill",
			Steps: []*millv1.Step{{Name: "implement"}, {Name: "review"}}})
		Expect(err).NotTo(HaveOccurred())

		resolved, err := l.store.GetWorkflow(ctx, store.GetWorkflowParams{Name: "feature", Project: "mill"})
		Expect(err).NotTo(HaveOccurred())
		Expect(resolved.GetId()).To(Equal(project.GetId()))

		resolved, err = l.store.GetWorkflow(ctx, store.GetWorkflowParams{Name: "feature", Project: "skift"})
		Expect(err).NotTo(HaveOccurred())
		Expect(resolved.GetId()).To(Equal(org.GetId()))

		_, err = l.store.GetWorkflow(ctx, store.GetWorkflowParams{Name: "nothing"})
		Expect(err).To(MatchError(store.ErrNotFound))
	}, SpecTimeout(time.Minute))

	It("stores a skill once, by digest", func(ctx SpecContext) {
		l := newLedger(ctx)
		skill := &millv1.Skill{
			Name: "implement", Digest: "sha256:abc", Body: "do the work",
			Files:    map[string]string{"scripts/check.sh": "sha256:def"},
			Metadata: map[string]string{"description": "implement a node"},
		}
		stored, created, err := l.store.PutSkill(ctx, skill)
		Expect(err).NotTo(HaveOccurred())
		Expect(created).To(BeTrue())
		Expect(stored.GetFiles()).To(HaveKeyWithValue("scripts/check.sh", "sha256:def"))
		Expect(stored.GetMetadata()).To(HaveKeyWithValue("description", "implement a node"))

		again, created, err := l.store.PutSkill(ctx, skill)
		Expect(err).NotTo(HaveOccurred())
		Expect(created).To(BeFalse())
		Expect(again.GetId()).To(Equal(stored.GetId()))
	}, SpecTimeout(time.Minute))
})

var _ = Describe("The reads the CLI needs", Label(testsupport.DBLabel), func() {
	It("Status derives the current step, the attempts, the cost and the leases", func(ctx SpecContext) {
		l := newLedger(ctx)
		run, _ := l.runWithWorkflow(ctx, "mill#8")
		l.close(ctx, run.GetId(), "implement")
		failing := l.start(ctx, run.GetId(), "simplify")
		_, err := l.store.RecordUsage(ctx, l.lease, failing.GetId(), []*millv1.Usage{
			{Model: "opus", InputTokens: 10, OutputTokens: 2},
			{Model: "opus", InputTokens: 5},
			{Model: "haiku", InputTokens: 1},
		})
		Expect(err).NotTo(HaveOccurred())

		summaries, _, err := l.store.Status(ctx, nil, "mill")
		Expect(err).NotTo(HaveOccurred())
		Expect(summaries).To(HaveLen(1))
		summary := summaries[0]
		Expect(summary.GetCurrentStep()).To(Equal("simplify"))
		Expect(summary.GetCurrentAttempt()).To(Equal(int64(1)))
		Expect(summary.GetTotals()).To(HaveLen(2))
		Expect(summary.GetTotals()[1].GetModel()).To(Equal("opus"))
		Expect(summary.GetTotals()[1].GetInputTokens()).To(Equal(int64(15)))
		Expect(summary.GetLeases()).To(BeZero(), "the run has been claimed once, at creation")

		_, err = l.store.SetLease(ctx, run.GetId(), "L2", "worker-2")
		Expect(err).NotTo(HaveOccurred())
		summaries, _, err = l.store.Status(ctx, []millv1.RunStatus{millv1.RunStatus_RUN_STATUS_RUNNING}, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(summaries[0].GetLeases()).To(Equal(int64(1)))

		// A project the node does not belong to sees nothing.
		summaries, _, err = l.store.Status(ctx, nil, "skift")
		Expect(err).NotTo(HaveOccurred())
		Expect(summaries).To(BeEmpty())
	}, SpecTimeout(time.Minute))

	It("lists the workflows that decode and names the row that does not", func(ctx SpecContext) {
		l := newLedger(ctx)
		good := l.workflow(ctx)
		stale, _, err := l.store.PutWorkflow(ctx, &millv1.Workflow{
			Name: "legacy", Scope: millv1.Scope_SCOPE_ORG,
			Steps: []*millv1.Step{{Name: "implement"}}})
		Expect(err).NotTo(HaveOccurred())
		// A field an older mill's schema had and this one's does not, in
		// the bytes of the stored definition.
		l.corrupt(ctx, stale.GetId(), `{"model": "opus"}`)

		listed, findings, err := l.store.ListWorkflows(ctx, store.WorkflowFilter{})
		Expect(err).NotTo(HaveOccurred())
		Expect(listed).To(HaveLen(1))
		Expect(listed[0].GetId()).To(Equal(good.GetId()))
		Expect(findings).To(HaveLen(1))
		Expect(findings[0].WorkflowID).To(Equal(stale.GetId()))
		Expect(findings[0].Field).To(Equal("model"))
		Expect(findings[0].Detail).NotTo(BeEmpty())

		// The row is reported and nothing else: still there, still as it was.
		var definition []byte
		Expect(l.db.QueryRow(ctx, `SELECT definition FROM workflows WHERE id = $1`, stale.GetId()).
			Scan(&definition)).To(Succeed())
		Expect(string(definition)).To(ContainSubstring(`"model"`))
	}, SpecTimeout(time.Minute))

	It("names a field nested in a step, by its path", func(ctx SpecContext) {
		l := newLedger(ctx)
		stale, _, err := l.store.PutWorkflow(ctx, &millv1.Workflow{
			Name: "legacy", Scope: millv1.Scope_SCOPE_ORG,
			Steps: []*millv1.Step{{Name: "implement"}, {Name: "review"}}})
		Expect(err).NotTo(HaveOccurred())
		l.corrupt(ctx, stale.GetId(), `{"steps": [{"name": "implement"}, {"name": "review", "budget": 3}]}`)

		_, findings, err := l.store.ListWorkflows(ctx, store.WorkflowFilter{})
		Expect(err).NotTo(HaveOccurred())
		Expect(findings).To(HaveLen(1))
		Expect(findings[0].Field).To(Equal("steps[1].budget"))
	}, SpecTimeout(time.Minute))

	It("Status lists a run whose workflow will not decode, with the row named once", func(ctx SpecContext) {
		l := newLedger(ctx)
		good, _ := l.runWithWorkflow(ctx, "mill#8")
		legacy, _, err := l.store.PutWorkflow(ctx, &millv1.Workflow{
			Name: "legacy", Scope: millv1.Scope_SCOPE_ORG,
			Steps: []*millv1.Step{{Name: "assess"}, {Name: "implement"}}})
		Expect(err).NotTo(HaveOccurred())
		stale, workflow := l.runFollowing(ctx, "mill#9", legacy)
		l.corrupt(ctx, workflow.GetId(), `{"model": "opus"}`)

		summaries, findings, err := l.store.Status(ctx, nil, "mill")
		Expect(err).NotTo(HaveOccurred())
		ids := make([]string, 0, len(summaries))
		for _, summary := range summaries {
			ids = append(ids, summary.GetRun().GetId())
		}
		Expect(ids).To(ConsistOf(good.GetId(), stale.GetId()))
		for _, summary := range summaries {
			if summary.GetRun().GetId() == good.GetId() {
				Expect(summary.GetCurrentStep()).To(Equal("implement"), "the run whose workflow decodes is derived as ever")
			} else {
				Expect(summary.GetCurrentStep()).To(BeEmpty(), "no definition, no derived step")
			}
		}
		Expect(findings).To(HaveLen(1))
		Expect(findings[0].WorkflowID).To(Equal(workflow.GetId()))
		Expect(findings[0].Field).To(Equal("model"))
	}, SpecTimeout(time.Minute))

	It("Inspect returns every attempt in order with its evidence and cost", func(ctx SpecContext) {
		l := newLedger(ctx)
		run, _ := l.runWithWorkflow(ctx, "mill#8")
		l.close(ctx, run.GetId(), "implement")
		open := l.start(ctx, run.GetId(), "simplify")
		_, err := l.store.RecordUsage(ctx, l.lease, open.GetId(),
			[]*millv1.Usage{{Model: "opus", InputTokens: 4}})
		Expect(err).NotTo(HaveOccurred())

		detail, err := l.store.Inspect(ctx, store.InspectParams{NodeRef: "mill#8"})
		Expect(err).NotTo(HaveOccurred())
		Expect(detail.Workflow.GetName()).To(Equal("feature"))
		Expect(detail.Steps).To(HaveLen(3))
		Expect(detail.Steps[0].GetStepRun().GetStepName()).To(Equal("assess"))
		Expect(detail.Steps[1].GetEvidence().GetCommit()).To(Equal("implement-sha"))
		Expect(detail.Steps[2].GetEvidence()).To(BeNil())
		Expect(detail.Steps[2].GetTotals()[0].GetInputTokens()).To(Equal(int64(4)))
		Expect(detail.Summary.GetCurrentStep()).To(Equal("simplify"))
	}, SpecTimeout(time.Minute))

	It("writes the audit trail as it writes the change", func(ctx SpecContext) {
		l := newLedger(ctx)
		run, _ := l.runWithWorkflow(ctx, "mill#8")
		l.close(ctx, run.GetId(), "implement")

		events, err := l.store.Events(ctx, run.GetId())
		Expect(err).NotTo(HaveOccurred())
		ops := make([]string, 0, len(events))
		for _, e := range events {
			ops = append(ops, e.Op)
		}
		Expect(ops).To(Equal([]string{
			"create_run", "start_step_run", "close_step_run", "start_step_run", "close_step_run",
		}))

		_, err = l.db.Exec(ctx, `DELETE FROM events WHERE run_id = $1`, run.GetId())
		Expect(err).To(MatchError(ContainSubstring("events is append-only")))
	}, SpecTimeout(time.Minute))
})

var _ = Describe("ListRuns", Label(testsupport.DBLabel), func() {
	It("and Inspect agree on last_step, last_step_status and rounds after one revise round", func(ctx SpecContext) {
		l := newLedger(ctx)
		run, _ := l.runWithWorkflow(ctx, "mill#8")
		for _, name := range []string{"implement", "simplify", "commit", "pr"} {
			l.close(ctx, run.GetId(), name)
		}
		firstReview := l.fail(ctx, run.GetId(), "review")
		secondReview := l.revise(ctx, run.GetId(), "review", firstReview.GetId())
		_, err := l.store.FailStepRun(ctx, l.lease, secondReview.GetId(), nil, nil)
		Expect(err).NotTo(HaveOccurred())
		_, err = l.store.Failed(ctx, l.lease, run.GetId(), "denied")
		Expect(err).NotTo(HaveOccurred())

		runs, _, _, _, err := l.store.ListRuns(ctx, store.ListRunsPageParams{})
		Expect(err).NotTo(HaveOccurred())
		var listed *millv1.RunSummary
		for _, summary := range runs {
			if summary.GetRun().GetId() == run.GetId() {
				listed = summary
			}
		}
		Expect(listed).NotTo(BeNil(), "the failed run must be on the default (terminal) listing")
		Expect(listed.GetLastStep()).To(Equal("review"))
		Expect(listed.GetLastStepStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_FAILED))
		Expect(listed.GetRounds()).To(Equal(int64(2)))

		detail, err := l.store.Inspect(ctx, store.InspectParams{ID: run.GetId()})
		Expect(err).NotTo(HaveOccurred())
		Expect(detail.Summary.GetLastStep()).To(Equal(listed.GetLastStep()))
		Expect(detail.Summary.GetLastStepStatus()).To(Equal(listed.GetLastStepStatus()))
		Expect(detail.Summary.GetRounds()).To(Equal(listed.GetRounds()))
	}, SpecTimeout(time.Minute))

	It("excludes a run that ended a second before the window", func(ctx SpecContext) {
		l := newLedger(ctx)
		window := time.Now().UTC().Truncate(time.Second)

		before, _ := l.runWithWorkflow(ctx, "mill#8")
		_, err := l.store.Done(ctx, l.lease, before.GetId())
		Expect(err).NotTo(HaveOccurred())
		l.endAt(ctx, before.GetId(), window.Add(-time.Second))

		within, _ := l.runWithWorkflow(ctx, "mill#9")
		_, err = l.store.Done(ctx, l.lease, within.GetId())
		Expect(err).NotTo(HaveOccurred())
		l.endAt(ctx, within.GetId(), window)

		runs, _, _, _, err := l.store.ListRuns(ctx, store.ListRunsPageParams{EndedAfter: &window})
		Expect(err).NotTo(HaveOccurred())
		ids := make([]string, 0, len(runs))
		for _, summary := range runs {
			ids = append(ids, summary.GetRun().GetId())
		}
		Expect(ids).To(ConsistOf(within.GetId()))
	}, SpecTimeout(time.Minute))

	It("pages two runs with an equal ended_at once each, with no overlap", func(ctx SpecContext) {
		l := newLedger(ctx)
		tie := time.Now().UTC().Truncate(time.Second)

		first, _ := l.runWithWorkflow(ctx, "mill#8")
		_, err := l.store.Done(ctx, l.lease, first.GetId())
		Expect(err).NotTo(HaveOccurred())
		l.endAt(ctx, first.GetId(), tie)

		second, _ := l.runWithWorkflow(ctx, "mill#9")
		_, err = l.store.Done(ctx, l.lease, second.GetId())
		Expect(err).NotTo(HaveOccurred())
		l.endAt(ctx, second.GetId(), tie)

		page1, next1, _, _, err := l.store.ListRuns(ctx, store.ListRunsPageParams{Limit: 1})
		Expect(err).NotTo(HaveOccurred())
		Expect(page1).To(HaveLen(1))
		Expect(next1).NotTo(BeEmpty(), "a second run is still waiting")

		page2, next2, _, _, err := l.store.ListRuns(ctx, store.ListRunsPageParams{Limit: 1, Cursor: next1})
		Expect(err).NotTo(HaveOccurred())
		Expect(page2).To(HaveLen(1))
		Expect(next2).To(BeEmpty(), "nothing is left to page")

		Expect([]string{page1[0].GetRun().GetId(), page2[0].GetRun().GetId()}).To(
			ConsistOf(first.GetId(), second.GetId()), "each run exactly once, no overlap or gap")
	}, SpecTimeout(time.Minute))

	It("returns every generation for a node_ref", func(ctx SpecContext) {
		l := newLedger(ctx)
		first, _ := l.runWithWorkflow(ctx, "mill#20")
		_, err := l.store.Abandon(ctx, l.lease, first.GetId(), "restart")
		Expect(err).NotTo(HaveOccurred())

		second, err := l.store.CreateRun(ctx, "", store.CreateRunParams{
			NodeID: first.GetNodeId(), NodeRef: "mill#20", Generation: 2, Branch: "mill/20",
		})
		Expect(err).NotTo(HaveOccurred())
		second, err = l.store.SetLease(ctx, second.GetId(), l.lease, "worker-1")
		Expect(err).NotTo(HaveOccurred())
		_, err = l.store.Failed(ctx, l.lease, second.GetId(), "denied")
		Expect(err).NotTo(HaveOccurred())

		runs, _, _, _, err := l.store.ListRuns(ctx, store.ListRunsPageParams{NodeRef: "mill#20"})
		Expect(err).NotTo(HaveOccurred())
		ids := make([]string, 0, len(runs))
		for _, summary := range runs {
			ids = append(ids, summary.GetRun().GetId())
		}
		Expect(ids).To(ConsistOf(first.GetId(), second.GetId()))
	}, SpecTimeout(time.Minute))

	It("clamps a limit above 200 and says so", func(ctx SpecContext) {
		l := newLedger(ctx)
		run, _ := l.runWithWorkflow(ctx, "mill#8")
		_, err := l.store.Done(ctx, l.lease, run.GetId())
		Expect(err).NotTo(HaveOccurred())

		_, _, clamped, _, err := l.store.ListRuns(ctx, store.ListRunsPageParams{Limit: 500})
		Expect(err).NotTo(HaveOccurred())
		Expect(clamped).To(BeTrue())
	}, SpecTimeout(time.Minute))
})

// statusText renders a run status the way the ledger stores it, for the
// specs that assert a refusal names the status.
func statusText(status millv1.RunStatus) string {
	switch status {
	case millv1.RunStatus_RUN_STATUS_SELECTING:
		return "selecting"
	case millv1.RunStatus_RUN_STATUS_FAILED:
		return "failed"
	case millv1.RunStatus_RUN_STATUS_ABANDONED:
		return "abandoned"
	default:
		return status.String()
	}
}

var _ = Describe("Reconciliations", Label(testsupport.DBLabel), func() {
	It("records a pass with its findings, under no lease, and lists the newest first", func(ctx SpecContext) {
		l := newLedger(ctx)
		started := time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)

		Expect(l.store.RecordReconciliation(ctx, "mill@core", started, []store.ReconciliationFinding{
			{Kind: "orphan", Name: "mill-x", Action: "stopped; run directory removed", Detail: "unit mill-machine@x.service"},
			{Kind: "swept", Name: "mill-acceptance-x-0badcafe", Action: "archived (skift offers no project delete)"},
		})).To(Succeed())
		Expect(l.store.RecordReconciliation(ctx, "mill@core", started.Add(time.Hour), nil)).To(Succeed(),
			"a pass that found nothing is still a record")

		passes, err := l.store.Reconciliations(ctx, 10)
		Expect(err).NotTo(HaveOccurred())
		Expect(passes).To(HaveLen(2))
		Expect(passes[0].StartedAt).To(BeTemporally("==", started.Add(time.Hour)))
		Expect(passes[0].Findings).To(BeEmpty())
		Expect(passes[1].WorkerRef).To(Equal("mill@core"))
		Expect(passes[1].Findings).To(HaveLen(2))
		Expect(passes[1].Findings[0].Name).To(Equal("mill-x"))
		Expect(passes[1].Findings[1].Action).To(ContainSubstring("archived"))
	}, SpecTimeout(time.Minute))
})
