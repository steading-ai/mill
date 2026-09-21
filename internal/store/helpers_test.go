package store_test

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/store"
)

// The workflow every spec that needs one runs against: the five steps of
// docs/ledger.md's revise example, implement through review.
var fiveSteps = []string{"implement", "simplify", "commit", "pr", "review"}

// ledger is one spec's database, the store over it, and the fixtures it has
// made so far. Every spec builds its own; nothing is shared but the server.
type ledger struct {
	db    *pgxpool.Pool
	store *store.Store
	// lease is the lease the fixtures were created under, and the one a
	// spec presents unless it is testing what happens when it does not.
	lease string
}

// newLedger clones a migrated database and opens a store on it.
func newLedger(ctx context.Context) *ledger {
	GinkgoHelper()
	db := pg.Database(ctx)
	return &ledger{db: db, store: store.FromPool(db), lease: "lease-1"}
}

// workflow stores the five-step workflow and returns it.
func (l *ledger) workflow(ctx context.Context) *millv1.Workflow {
	GinkgoHelper()
	steps := make([]*millv1.Step, 0, len(fiveSteps))
	for _, name := range fiveSteps {
		steps = append(steps, &millv1.Step{
			Name:     name,
			Kind:     millv1.StepKind_STEP_KIND_SKILL,
			Evidence: millv1.EvidenceKind_EVIDENCE_KIND_COMMIT,
		})
	}
	stored, _, err := l.store.PutWorkflow(ctx, &millv1.Workflow{
		Name:  "feature",
		Scope: millv1.Scope_SCOPE_ORG,
		Steps: steps,
	})
	Expect(err).NotTo(HaveOccurred())
	return stored
}

// corrupt merges extra JSON into a stored workflow's definition, which is
// what a row written by a mill whose schema had a field this one's does not
// looks like.
func (l *ledger) corrupt(ctx context.Context, workflowID, extra string) {
	GinkgoHelper()
	_, err := l.db.Exec(ctx, `UPDATE workflows SET definition = definition || $2::jsonb WHERE id = $1`, workflowID, extra)
	Expect(err).NotTo(HaveOccurred())
}

// run creates a run on a node, holding l.lease.
func (l *ledger) run(ctx context.Context, nodeRef string) *millv1.Run {
	GinkgoHelper()
	run, err := l.store.CreateRun(ctx, l.lease, store.CreateRunParams{
		NodeID:    "node-" + nodeRef,
		NodeRef:   nodeRef,
		ContextID: "ctx-" + nodeRef,
		WorkerRef: "worker-1",
		Branch:    "mill/" + nodeRef,
	})
	Expect(err).NotTo(HaveOccurred())
	return run
}

// runWithWorkflow creates a run and pins the five-step workflow to it, the
// state a run is in once selection has closed.
func (l *ledger) runWithWorkflow(ctx context.Context, nodeRef string) (*millv1.Run, *millv1.Workflow) {
	GinkgoHelper()
	return l.runFollowing(ctx, nodeRef, l.workflow(ctx))
}

// runFollowing creates a run and pins a stored workflow to it.
func (l *ledger) runFollowing(ctx context.Context, nodeRef string, workflow *millv1.Workflow) (*millv1.Run, *millv1.Workflow) {
	GinkgoHelper()
	run := l.run(ctx, nodeRef)
	stepRun := l.start(ctx, run.GetId(), "assess")
	_, _, err := l.store.CloseStepRun(ctx, l.lease, store.CloseStepRunParams{
		StepRunID:  stepRun.GetId(),
		Evidence:   commitEvidence("assessed"),
		WorkflowID: workflow.GetId(),
		Tier:       millv1.Tier_TIER_FEATURE,
	})
	Expect(err).NotTo(HaveOccurred())
	run, err = l.store.GetRun(ctx, run.GetId())
	Expect(err).NotTo(HaveOccurred())
	return run, workflow
}

// start opens an attempt at a skill step.
func (l *ledger) start(ctx context.Context, runID, stepName string) *millv1.StepRun {
	GinkgoHelper()
	return l.startKind(ctx, runID, stepName, millv1.StepKind_STEP_KIND_SKILL)
}

// startKind opens an attempt at a step of a given kind.
func (l *ledger) startKind(ctx context.Context, runID, stepName string, kind millv1.StepKind) *millv1.StepRun {
	GinkgoHelper()
	stepRun, err := l.store.StartStepRun(ctx, l.lease, store.StartStepRunParams{
		RunID:     runID,
		StepName:  stepName,
		Kind:      kind,
		WorkerRef: "worker-1",
	})
	Expect(err).NotTo(HaveOccurred())
	return stepRun
}

// close opens an attempt at a step and closes it with commit evidence.
func (l *ledger) close(ctx context.Context, runID, stepName string) *millv1.StepRun {
	GinkgoHelper()
	stepRun := l.start(ctx, runID, stepName)
	closed, _, err := l.store.CloseStepRun(ctx, l.lease, store.CloseStepRunParams{
		StepRunID: stepRun.GetId(),
		Evidence:  commitEvidence(stepName + "-sha"),
	})
	Expect(err).NotTo(HaveOccurred())
	return closed
}

// fail opens an attempt at a step and fails it.
func (l *ledger) fail(ctx context.Context, runID, stepName string) *millv1.StepRun {
	GinkgoHelper()
	stepRun := l.start(ctx, runID, stepName)
	failed, err := l.store.FailStepRun(ctx, l.lease, stepRun.GetId(), nil)
	Expect(err).NotTo(HaveOccurred())
	return failed
}

// currentStep is the run's derived current step.
func (l *ledger) currentStep(ctx context.Context, runID string, workflow *millv1.Workflow) string {
	GinkgoHelper()
	state, err := l.store.RunState(ctx, runID)
	Expect(err).NotTo(HaveOccurred())
	name, _ := store.CurrentStep(state, workflow)
	return name
}

// endAt overwrites a run's ended_at directly, for a spec that needs to place
// it at a chosen instant to test a window boundary or a tie-break — Done,
// Failed and Abandon only ever set it to now().
func (l *ledger) endAt(ctx context.Context, runID string, endedAt time.Time) {
	GinkgoHelper()
	_, err := l.db.Exec(ctx, `UPDATE runs SET ended_at = $2 WHERE id = $1`, runID, endedAt)
	Expect(err).NotTo(HaveOccurred())
}

// revise supersedes a step's latest attempt and opens the next one as the
// round that revises it, the way a human's revise request does.
func (l *ledger) revise(ctx context.Context, runID, stepName, feedbackFrom string) *millv1.StepRun {
	GinkgoHelper()
	_, err := l.store.SupersedeSteps(ctx, l.lease, runID, stepName, stepName)
	Expect(err).NotTo(HaveOccurred())
	stepRun, err := l.store.StartStepRun(ctx, l.lease, store.StartStepRunParams{
		RunID:        runID,
		StepName:     stepName,
		Kind:         millv1.StepKind_STEP_KIND_SKILL,
		WorkerRef:    "worker-1",
		FeedbackFrom: feedbackFrom,
	})
	Expect(err).NotTo(HaveOccurred())
	return stepRun
}

// statusOf reads a step run's status straight from the table, for the specs
// that must see the row rather than what a method returned.
func (l *ledger) statusOf(ctx context.Context, stepRunID string) string {
	GinkgoHelper()
	var status string
	Expect(l.db.QueryRow(ctx, `SELECT status FROM step_runs WHERE id = $1`, stepRunID).Scan(&status)).To(Succeed())
	return status
}

// commitEvidence is the simplest fact that closes a step.
func commitEvidence(sha string) *millv1.Evidence {
	return &millv1.Evidence{
		Kind:       millv1.EvidenceKind_EVIDENCE_KIND_COMMIT,
		Value:      &millv1.Evidence_Commit{Commit: sha},
		VerifiedBy: "spec",
	}
}

// newID mints an identifier of the shape the is_ulid check accepts, for the
// specs that insert rows by hand to test a constraint the store API cannot
// reach.
func newID() string {
	const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	id := make([]byte, 26)
	id[0] = '0'
	for i := 1; i < len(id); i++ {
		id[i] = crockford[rand.IntN(len(crockford))]
	}
	return string(id)
}

// asAppRole opens a connection of its own and switches it to the role the
// mill runs as, so a spec sees exactly the privileges production has.
func (l *ledger) asAppRole(ctx context.Context) *pgx.Conn {
	GinkgoHelper()
	conn, err := pgx.ConnectConfig(ctx, l.db.Config().ConnConfig)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func(ctx context.Context) error { return conn.Close(ctx) })
	_, err = conn.Exec(ctx, `SET ROLE mill_app`)
	Expect(err).NotTo(HaveOccurred())
	return conn
}
