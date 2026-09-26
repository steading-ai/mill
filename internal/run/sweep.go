package run

import (
	"context"
	"fmt"
	"time"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/workspace/machine"
)

// The sweep: the ledger reconciled against skift (docs/ledger.md §Sweep).
//
// Every way a run ends happens inside a claim, and a node skift has closed
// — dropped, or completed by someone else — never enters the ready set
// again, so no claim ever comes to end the run it strands. The host's
// reconciliation pass cannot help: it compares machines with live runs and
// never asks skift what became of a run's node. The sweep is that missing
// axis. It reads every live run this worker holds, asks skift about its
// node, and catches the run up with a node that is closed — through the
// same resolution `mill run` on such a node makes — so that the cleaners
// that already exist (Destroy, FinishWorkspace, the orphan and leftover
// passes) do the rest.
//
// What it never does is act on a node that is open in any readiness. A
// parked run's node is blocked by its own gate and a held node is parked
// by a person; both are live runs waiting for exactly what they are waiting
// for. The predicate is skift's closed status and nothing looser, by name
// and never by "everything not in the ledger" (mill#49).

const (
	// DefaultSweepInterval is how long a worker goes between sweeps. It is
	// skift's lease TTL: a run touched inside one TTL may be another
	// process's on this host, mid-write, and is left for that process.
	DefaultSweepInterval = DefaultTTL
	// FindingStranded is a live run whose node skift had closed, caught up
	// with it: `failed`, or `done` when the run's own completing patch had
	// landed and the store missed it.
	FindingStranded machine.FindingKind = "stranded"
	// FindingHealFiled is a new bug node filed for a project's default
	// branch found red.
	FindingHealFiled machine.FindingKind = "heal-filed"
	// FindingHealCommented is a comment added to an already-filed healing
	// node: a new red sha, or main going green underneath a claim.
	FindingHealCommented machine.FindingKind = "heal-commented"
	// FindingHealDropped is a healing node dropped because its branch went
	// green and nobody had claimed it.
	FindingHealDropped machine.FindingKind = "heal-dropped"
)

// Sweep is one pass of the sweep, recorded in the ledger like the host's
// reconciliation. driving says whether this process is driving a node right
// now, which the sweep leaves alone whatever skift says of it. projects is
// the worker's own project filter (`WorkerOptions.Projects`): the red-main
// probe and its reconciler run over exactly these projects and no others,
// so a worker configured for none heals nothing.
//
// The only error is a ledger that cannot be listed or a pass that cannot be
// recorded; anything one run does wrong is a `failure` finding and the pass
// goes on to the next run. A repository the probe cannot reach, or a
// reconciliation write that fails, is logged and never stops the pass.
func (r *Runner) Sweep(ctx context.Context, driving func(nodeID string) bool, projects []string) (machine.Report, error) {
	report := machine.Report{StartedAt: r.options.Now()}
	res, err := call(ctx, r.options.Store.Status, &millv1.StatusRequest{})
	if err != nil {
		return report, err
	}
	for _, summary := range res.GetRuns() {
		r.sweepOne(ctx, &report, summary, driving)
	}
	r.heal(ctx, &report, projects)
	if err := NewLedger(r.options.Store, r.options.WorkerRef, r.options.Log).RecordReconciliation(ctx, report); err != nil {
		return report, err
	}
	return report, nil
}

// heal probes every project's default branch and converges skift's healing
// nodes against what it finds (docs/architecture.md §"Failure and stop").
// Neither half's failure is fatal to the sweep: a project the probe cannot
// reach is logged and skipped, and a reconciliation write that fails is
// logged, leaving whatever it managed as this pass's findings.
func (r *Runner) heal(ctx context.Context, report *machine.Report, projects []string) {
	findings := RedMainFindings(ctx, projects, r.options.Skift.ProjectNamed, r.options.Repositories,
		func(project string, err error) {
			r.options.Log.Warn("probing a project's default branch", "project", project, "error", err)
		})
	actions, err := Reconcile(ctx, r.options.Skift, projects, findings)
	if err != nil {
		r.options.Log.Warn("healing reconciliation", "error", err)
	}
	for _, action := range actions {
		report.Findings = append(report.Findings, machine.Finding{
			Kind: healFindingKind(action.Verb), Name: action.Ref, Action: action.Verb, Detail: action.Key,
		})
	}
}

// healFindingKind maps a HealAction's verb to the machine.FindingKind it is
// recorded under.
func healFindingKind(verb string) machine.FindingKind {
	switch verb {
	case "filed":
		return FindingHealFiled
	case "dropped":
		return FindingHealDropped
	default:
		return FindingHealCommented
	}
}

// sweepOne decides one live run, in the order the predicate is written in
// docs/ledger.md §Sweep: this worker's, not in flight here, untouched for a
// lease TTL, and only then skift's word on the node.
func (r *Runner) sweepOne(ctx context.Context, report *machine.Report, summary *millv1.RunSummary, driving func(string) bool) {
	run := summary.GetRun()
	if run.GetWorkerRef() != r.options.WorkerRef {
		return
	}
	if driving != nil && driving(run.GetNodeId()) {
		return
	}
	if run.GetUpdatedAt().AsTime().After(r.options.Now().Add(-r.options.TTL)) {
		return
	}
	node, err := r.options.Skift.Node(ctx, run.GetNodeRef())
	if err != nil {
		report.Findings = append(report.Findings, machine.Finding{
			Kind: machine.FindingFailure, Name: run.GetId(),
			Detail: fmt.Sprintf("reading %s: %v", run.GetNodeRef(), err),
		})
		return
	}
	if node.GetStatus() == skiftv1.Status_STATUS_OPEN {
		return
	}
	action, err := r.reconcileClosed(ctx, run, node)
	if err != nil {
		report.Findings = append(report.Findings, machine.Finding{
			Kind: machine.FindingFailure, Name: run.GetId(),
			Detail: fmt.Sprintf("%s is %s: %v", run.GetNodeRef(), skiftStatus(node.GetStatus()), err),
		})
		return
	}
	step := summary.GetCurrentStep()
	if step == "" {
		step = "(selecting)"
	}
	report.Findings = append(report.Findings, machine.Finding{
		Kind: FindingStranded, Name: run.GetId(), Action: action,
		Detail: fmt.Sprintf("%s is %s; the ledger's step was %s", run.GetNodeRef(), skiftStatus(node.GetStatus()), step),
	})
}

// sweepDue reports whether a worker that last swept at last is due another
// sweep under interval, as of now.
func sweepDue(last time.Time, interval time.Duration, now time.Time) bool {
	return last.IsZero() || !now.Before(last.Add(interval))
}
