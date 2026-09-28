package store_test

import (
	"time"

	"github.com/jackc/pgx/v5"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/store"
	"github.com/steading-ai/mill/internal/testsupport"
)

// The twelve invariants of docs/ledger.md, one spec each. Each is enforced
// by a constraint, an index or a transactional check, never by convention,
// so each spec here fails if the enforcement is taken out of the schema.
var _ = Describe("Invariants", Label(testsupport.DBLabel), func() {
	It(`Invariant 1: "One live run per node." Partial unique index on runs.node_id where status is not terminal; (node_id, generation) unique overall`, func(ctx SpecContext) {
		l := newLedger(ctx)
		first := l.run(ctx, "mill#8")

		_, err := l.store.CreateRun(ctx, l.lease, store.CreateRunParams{
			NodeID: first.GetNodeId(), NodeRef: first.GetNodeRef(), ContextID: first.GetContextId(),
			Generation: 2, WorkerRef: "worker-2",
		})
		Expect(err).To(MatchError(ContainSubstring("runs_live_node_id_idx")),
			"a second live run on the node must be rejected by the partial unique index")

		_, err = l.store.Abandon(ctx, l.lease, first.GetId(), "restart")
		Expect(err).NotTo(HaveOccurred())

		second, err := l.store.CreateRun(ctx, "L2", store.CreateRunParams{
			NodeID: first.GetNodeId(), NodeRef: first.GetNodeRef(), ContextID: first.GetContextId(),
			Generation: first.GetGeneration() + 1, WorkerRef: "worker-2",
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(second.GetGeneration()).To(Equal(int64(2)))

		_, err = l.db.Exec(ctx, `UPDATE runs SET generation = 2 WHERE id = $1`, first.GetId())
		Expect(err).To(MatchError(ContainSubstring("runs_node_id_generation_key")),
			"(node_id, generation) is unique whatever the status")
	}, SpecTimeout(time.Minute))

	It(`Invariant 2: "A run's workflow is set once." workflow_id transitions from null to a value exactly once`, func(ctx SpecContext) {
		l := newLedger(ctx)
		run, workflow := l.runWithWorkflow(ctx, "mill#8")
		Expect(run.GetWorkflowId()).To(Equal(workflow.GetId()))

		other, _, err := l.store.PutWorkflow(ctx, &millv1.Workflow{
			Name: "docs", Scope: millv1.Scope_SCOPE_ORG,
			Steps: []*millv1.Step{{Name: "implement"}},
		})
		Expect(err).NotTo(HaveOccurred())

		_, err = l.db.Exec(ctx, `UPDATE runs SET workflow_id = $2 WHERE id = $1`, run.GetId(), other.GetId())
		Expect(err).To(MatchError(ContainSubstring("workflow_id is set once")))

		_, err = l.db.Exec(ctx, `UPDATE runs SET workflow_id = NULL WHERE id = $1`, run.GetId())
		Expect(err).To(MatchError(ContainSubstring("workflow_id is set once")))
	}, SpecTimeout(time.Minute))

	It(`Invariant 3: "At most one running step run per run." Partial unique index on (run_id) WHERE status = 'running'`, func(ctx SpecContext) {
		l := newLedger(ctx)
		run, _ := l.runWithWorkflow(ctx, "mill#8")
		l.start(ctx, run.GetId(), "implement")

		_, err := l.store.StartStepRun(ctx, l.lease, store.StartStepRunParams{
			RunID: run.GetId(), StepName: "simplify", Kind: millv1.StepKind_STEP_KIND_SKILL,
		})
		Expect(err).To(MatchError(ContainSubstring("step_runs_one_running_idx")))
	}, SpecTimeout(time.Minute))

	It(`Invariant 4: "Evidence closes exactly one step run, and a step run has at most one evidence." evidence.step_run_id is unique and a foreign key`, func(ctx SpecContext) {
		l := newLedger(ctx)
		run, _ := l.runWithWorkflow(ctx, "mill#8")
		closed := l.close(ctx, run.GetId(), "implement")

		_, err := l.db.Exec(ctx,
			`INSERT INTO evidence (step_run_id, kind, value) VALUES ($1, 'commit', '{}'::jsonb)`, closed.GetId())
		Expect(err).To(MatchError(ContainSubstring("evidence_pkey")), "a step run has at most one evidence")

		_, err = l.db.Exec(ctx,
			`INSERT INTO evidence (step_run_id, kind, value) VALUES ($1, 'commit', '{}'::jsonb)`, newID())
		Expect(err).To(MatchError(ContainSubstring("evidence_step_run_fkey")), "evidence closes a step run that exists")
	}, SpecTimeout(time.Minute))

	It(`Invariant 5: "A closed step run has evidence; a step run with evidence is closed." Written in one transaction`, func(ctx SpecContext) {
		l := newLedger(ctx)
		run, _ := l.runWithWorkflow(ctx, "mill#8")
		open := l.start(ctx, run.GetId(), "implement")

		// Closed with no evidence: the deferred trigger fires at commit.
		err := inTx(ctx, l, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE step_runs SET status = 'closed', ended_at = now() WHERE id = $1`, open.GetId())
			return err
		})
		Expect(err).To(MatchError(ContainSubstring("is closed with no evidence")))
		Expect(l.statusOf(ctx, open.GetId())).To(Equal("running"))

		// Evidence for an attempt that is not being closed: likewise.
		err = inTx(ctx, l, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO evidence (step_run_id, kind, value) VALUES ($1, 'commit', '{}'::jsonb)`, open.GetId())
			return err
		})
		Expect(err).To(MatchError(ContainSubstring("not closed")))

		// Both halves in one transaction is the only way through, which is
		// what CloseStepRun does.
		closed, evidence, err := l.store.CloseStepRun(ctx, l.lease, store.CloseStepRunParams{
			StepRunID: open.GetId(), Evidence: commitEvidence("sha"),
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(closed.GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED))
		Expect(evidence.GetCommit()).To(Equal("sha"))

		// The one named exception (00026_verdict_evidence_on_failed_step.sql):
		// a review's revise verdict is recorded on the attempt it failed, not
		// only on one it closed. Narrow to `kind = 'verdict'` and
		// `status = 'failed'` — every other kind still requires closed, and a
		// verdict still requires the row to be closed or failed, not running.
		failedReview := l.fail(ctx, run.GetId(), "review")
		err = inTx(ctx, l, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO evidence (step_run_id, kind, value) VALUES ($1, 'verdict', '{"verdict":{"decision":"VERDICT_DECISION_REVISE"}}'::jsonb)`,
				failedReview.GetId())
			return err
		})
		Expect(err).NotTo(HaveOccurred(), "a verdict is admitted on a failed row, not only a closed one")

		failedImplement := l.fail(ctx, run.GetId(), "implement")
		err = inTx(ctx, l, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO evidence (step_run_id, kind, value) VALUES ($1, 'commit', '{}'::jsonb)`, failedImplement.GetId())
			return err
		})
		Expect(err).To(MatchError(ContainSubstring("not closed")), "the exception is verdict only, not every kind")

		runningSimplify := l.start(ctx, run.GetId(), "simplify")
		err = inTx(ctx, l, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO evidence (step_run_id, kind, value) VALUES ($1, 'verdict', '{"verdict":{"decision":"VERDICT_DECISION_REVISE"}}'::jsonb)`,
				runningSimplify.GetId())
			return err
		})
		Expect(err).To(MatchError(ContainSubstring("not closed")), "a verdict still requires closed or failed, not running")
	}, SpecTimeout(time.Minute))

	It(`Invariant 6: "Evidence is never updated or deleted." No UPDATE or DELETE grants on the table for the application role`, func(ctx SpecContext) {
		l := newLedger(ctx)
		run, _ := l.runWithWorkflow(ctx, "mill#8")
		closed := l.close(ctx, run.GetId(), "implement")
		app := l.asAppRole(ctx)

		var kind string
		Expect(app.QueryRow(ctx, `SELECT kind FROM evidence WHERE step_run_id = $1`, closed.GetId()).
			Scan(&kind)).To(Succeed(), "the role may read evidence")

		_, err := app.Exec(ctx, `UPDATE evidence SET kind = 'url' WHERE step_run_id = $1`, closed.GetId())
		Expect(err).To(MatchError(ContainSubstring("permission denied for table evidence")))

		_, err = app.Exec(ctx, `DELETE FROM evidence WHERE step_run_id = $1`, closed.GetId())
		Expect(err).To(MatchError(ContainSubstring("permission denied for table evidence")))

		// And the append-only trigger says the same thing to anyone whose
		// privileges would let them past the grants.
		_, err = l.db.Exec(ctx, `UPDATE evidence SET kind = 'url' WHERE step_run_id = $1`, closed.GetId())
		Expect(err).To(MatchError(ContainSubstring("evidence is append-only")))
		_, err = l.db.Exec(ctx, `DELETE FROM evidence WHERE step_run_id = $1`, closed.GetId())
		Expect(err).To(MatchError(ContainSubstring("evidence is append-only")))
	}, SpecTimeout(time.Minute))

	It(`Invariant 7: "Attempts are contiguous." attempt for (run_id, step_name) is max + 1, assigned in the insert`, func(ctx SpecContext) {
		l := newLedger(ctx)
		run, _ := l.runWithWorkflow(ctx, "mill#8")

		first := l.fail(ctx, run.GetId(), "implement")
		second := l.fail(ctx, run.GetId(), "implement")
		Expect(first.GetAttempt()).To(Equal(int64(1)))
		Expect(second.GetAttempt()).To(Equal(int64(2)))

		_, err := l.db.Exec(ctx, `
			INSERT INTO step_runs (id, run_id, step_name, attempt, kind)
			VALUES ($1, $2, 'implement', 7, 'skill')`, newID(), run.GetId())
		Expect(err).To(MatchError(ContainSubstring("must be 3")), "the number is not the caller's to choose")
	}, SpecTimeout(time.Minute))

	It(`Invariant 8: "A live token belongs to a running step run." Revocation is written in the same transaction that moves the step run out of running`, func(ctx SpecContext) {
		l := newLedger(ctx)
		run, _ := l.runWithWorkflow(ctx, "mill#8")
		open := l.start(ctx, run.GetId(), "implement")

		token, err := l.store.IssueStepToken(ctx, l.lease, open.GetId(),
			[]millv1.Capability{millv1.Capability_CAPABILITY_READ, millv1.Capability_CAPABILITY_WRITE})
		Expect(err).NotTo(HaveOccurred())
		Expect(token.GetToken()).NotTo(BeEmpty(), "the bearer is returned once")
		Expect(token.GetTokenHash()).NotTo(Equal(token.GetToken()), "the store keeps only the hash")

		// The step run cannot leave running with the token still live.
		err = inTx(ctx, l, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE step_runs SET status = 'aborted', ended_at = now() WHERE id = $1`, open.GetId())
			return err
		})
		Expect(err).To(MatchError(ContainSubstring("still live")))

		// Closing the attempt revokes it in the same transaction.
		_, _, err = l.store.CloseStepRun(ctx, l.lease, store.CloseStepRunParams{
			StepRunID: open.GetId(), Evidence: commitEvidence("sha"),
		})
		Expect(err).NotTo(HaveOccurred())
		var revoked *time.Time
		Expect(l.db.QueryRow(ctx, `SELECT revoked_at FROM step_tokens WHERE id = $1`, token.GetId()).
			Scan(&revoked)).To(Succeed())
		Expect(revoked).NotTo(BeNil())
	}, SpecTimeout(time.Minute))

	It(`Invariant 9: "Every store write is fenced by the lease, atomically." The runs row lock, a re-read of lease_id after the lock, a comparison, and a rollback on mismatch`, func(ctx SpecContext) {
		l := newLedger(ctx)
		run, _ := l.runWithWorkflow(ctx, "mill#8")
		open := l.start(ctx, run.GetId(), "implement")

		_, err := l.store.SetLease(ctx, run.GetId(), "L2", "worker-2")
		Expect(err).NotTo(HaveOccurred())

		// Every mutation for the run, presenting the lease the claim
		// replaced, is refused and writes nothing.
		_, _, err = l.store.CloseStepRun(ctx, l.lease, store.CloseStepRunParams{
			StepRunID: open.GetId(), Evidence: commitEvidence("sha")})
		Expect(err).To(MatchError(store.ErrFenced))
		_, err = l.store.FailStepRun(ctx, l.lease, open.GetId(), nil, nil)
		Expect(err).To(MatchError(store.ErrFenced))
		_, err = l.store.StartStepRun(ctx, l.lease, store.StartStepRunParams{
			RunID: run.GetId(), StepName: "simplify", Kind: millv1.StepKind_STEP_KIND_SKILL})
		Expect(err).To(MatchError(store.ErrFenced))
		_, err = l.store.IssueStepToken(ctx, l.lease, open.GetId(), nil)
		Expect(err).To(MatchError(store.ErrFenced))
		_, err = l.store.RecordUsage(ctx, l.lease, open.GetId(),
			[]*millv1.Usage{{Model: "opus", InputTokens: 1}})
		Expect(err).To(MatchError(store.ErrFenced))
		_, err = l.store.MarkUsagePosted(ctx, l.lease, open.GetId())
		Expect(err).To(MatchError(store.ErrFenced))
		_, err = l.store.SupersedeSteps(ctx, l.lease, run.GetId(), "implement", "review")
		Expect(err).To(MatchError(store.ErrFenced))
		_, err = l.store.MarkWaiting(ctx, l.lease, run.GetId(), "gate-1", open.GetId())
		Expect(err).To(MatchError(store.ErrFenced))
		_, err = l.store.Park(ctx, l.lease, run.GetId(), "gate-1", millv1.ParkedReason_PARKED_REASON_RETRIES)
		Expect(err).To(MatchError(store.ErrFenced))
		_, err = l.store.Done(ctx, l.lease, run.GetId())
		Expect(err).To(MatchError(store.ErrFenced))

		Expect(l.statusOf(ctx, open.GetId())).To(Equal("running"))
		var stepRuns, evidence, tokens, usage int
		Expect(l.db.QueryRow(ctx, `SELECT count(*) FROM step_runs WHERE run_id = $1`, run.GetId()).
			Scan(&stepRuns)).To(Succeed())
		Expect(stepRuns).To(Equal(2), "the assessment and the implement attempt, and nothing the stale worker added")
		Expect(l.db.QueryRow(ctx, `SELECT count(*) FROM evidence e JOIN step_runs sr ON sr.id = e.step_run_id
			WHERE sr.run_id = $1`, run.GetId()).Scan(&evidence)).To(Succeed())
		Expect(evidence).To(Equal(1), "only the assessment's evidence")
		Expect(l.db.QueryRow(ctx, `SELECT count(*) FROM step_tokens WHERE step_run_id = $1`, open.GetId()).
			Scan(&tokens)).To(Succeed())
		Expect(tokens).To(BeZero())
		Expect(l.db.QueryRow(ctx, `SELECT count(*) FROM usage WHERE step_run_id = $1`, open.GetId()).
			Scan(&usage)).To(Succeed())
		Expect(usage).To(BeZero())
	}, SpecTimeout(time.Minute))

	It(`Invariant 10: "current_step is derived." There is no column to drift`, func(ctx SpecContext) {
		l := newLedger(ctx)
		var columns []string
		rows, err := l.db.Query(ctx,
			`SELECT column_name FROM information_schema.columns WHERE table_name = 'runs'`)
		Expect(err).NotTo(HaveOccurred())
		for rows.Next() {
			var name string
			Expect(rows.Scan(&name)).To(Succeed())
			columns = append(columns, name)
		}
		rows.Close()
		Expect(rows.Err()).NotTo(HaveOccurred())
		Expect(columns).NotTo(ContainElement("current_step"))

		// And the derivation moves with the step runs, with nothing written
		// to the run at all.
		run, workflow := l.runWithWorkflow(ctx, "mill#8")
		Expect(l.currentStep(ctx, run.GetId(), workflow)).To(Equal("implement"))
		l.close(ctx, run.GetId(), "implement")
		Expect(l.currentStep(ctx, run.GetId(), workflow)).To(Equal("simplify"))
	}, SpecTimeout(time.Minute))

	It(`Invariant 11: "Skift first, store second." gate_node_id is recorded only after skift has returned the gate's id; the store never asserts a fact about skift that skift has not yet made true`, func(ctx SpecContext) {
		l := newLedger(ctx)
		run, _ := l.runWithWorkflow(ctx, "mill#8")
		open := l.start(ctx, run.GetId(), "implement")

		_, err := l.store.MarkWaiting(ctx, l.lease, run.GetId(), "", open.GetId())
		Expect(err).To(MatchError(ContainSubstring("runs_blocked_has_gate")),
			"a run cannot be waiting on a gate the patch has not returned")

		_, err = l.db.Exec(ctx, `UPDATE runs SET status = 'parked', parked_reason = 'retries' WHERE id = $1`, run.GetId())
		Expect(err).To(MatchError(ContainSubstring("runs_blocked_has_gate")))

		waiting, err := l.store.MarkWaiting(ctx, l.lease, run.GetId(), "gate-1", open.GetId())
		Expect(err).NotTo(HaveOccurred())
		Expect(waiting.GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_WAITING))
		Expect(waiting.GetLeaseId()).To(BeEmpty(), "the store records the gate before the release")
	}, SpecTimeout(time.Minute))

	It(`Invariant 12: "A gate is filed once per step run." gate_node_id set and the current step a gate step means resume reads the gate; it never files another`, func(ctx SpecContext) {
		l := newLedger(ctx)
		run, _ := l.runWithWorkflow(ctx, "mill#8")
		open := l.startKind(ctx, run.GetId(), "review", millv1.StepKind_STEP_KIND_GATE)
		_, err := l.store.MarkWaiting(ctx, l.lease, run.GetId(), "gate-1", open.GetId())
		Expect(err).NotTo(HaveOccurred())

		// A worker that resumes the run and files a second gate for it is
		// refused: the one already filed is the one to read.
		_, err = l.store.SetLease(ctx, run.GetId(), "L2", "worker-2")
		Expect(err).NotTo(HaveOccurred())
		_, err = l.store.MarkWaiting(ctx, "L2", run.GetId(), "gate-2", open.GetId())
		Expect(err).To(MatchError(ContainSubstring("a gate is filed once per step run")))

		// Closing the gate step is what releases it, and only then may the
		// next gate be filed.
		_, _, err = l.store.CloseStepRun(ctx, "L2", store.CloseStepRunParams{
			StepRunID: open.GetId(),
			Evidence: &millv1.Evidence{
				Kind:  millv1.EvidenceKind_EVIDENCE_KIND_GATE,
				Value: &millv1.Evidence_Gate{Gate: "gate-1"},
			},
		})
		Expect(err).NotTo(HaveOccurred())
		var gate *string
		Expect(l.db.QueryRow(ctx, `SELECT gate_node_id FROM runs WHERE id = $1`, run.GetId()).Scan(&gate)).To(Succeed())
		Expect(gate).To(BeNil())
	}, SpecTimeout(time.Minute))
})

// inTx runs fn in a transaction of its own and returns the first error,
// from the statement or from the commit: a deferred constraint trigger
// fires at commit, so a spec that only watched the statement would miss it.
func inTx(ctx SpecContext, l *ledger, fn func(tx pgx.Tx) error) error {
	GinkgoHelper()
	tx, err := l.db.Begin(ctx)
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
