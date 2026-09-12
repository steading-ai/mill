package run

import (
	"context"
	"log/slog"

	"google.golang.org/protobuf/types/known/timestamppb"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/gen/mill/v1/millv1connect"
	"github.com/ivy/mill/internal/workspace/machine"
)

// Ledger is the store as the machine backend's reconciliation reads it
// (machine.Ledger): which runs are live, on any host, and where the pass's
// report goes. It is the only thing reconciliation asks of the ledger, and
// it asks before the worker holds any lease, so nothing here is fenced.
type Ledger struct {
	store     millv1connect.StoreServiceClient
	workerRef string
	log       *slog.Logger
}

// NewLedger is the store's reconciliation view, recording under workerRef
// and logging to log what the store could list but not read.
func NewLedger(store millv1connect.StoreServiceClient, workerRef string, log *slog.Logger) *Ledger {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Ledger{store: store, workerRef: workerRef, log: log}
}

// LiveRuns is every run the ledger holds in a non-terminal state, with the
// step it is at and the machine its workspace record names. `Status` with
// no filter is exactly that set.
//
// A workflow row the store could not decode is logged and the pass goes on:
// the run on it is still live, still listed, and still reconciled by its
// machine; only its step is unknown. A worker that refused to start over
// one stale row would be a mill down for a fact about the ledger that
// stops no run (docs/runbook.md names the operator's remedy).
func (l *Ledger) LiveRuns(ctx context.Context) ([]machine.LiveRun, error) {
	res, err := call(ctx, l.store.Status, &millv1.StatusRequest{})
	if err != nil {
		return nil, err
	}
	for _, finding := range res.GetFindings() {
		l.log.Warn("workflow row does not decode; listed without it",
			"workflow", finding.GetWorkflowId(), "field", finding.GetField(), "detail", finding.GetDetail())
	}
	live := make([]machine.LiveRun, 0, len(res.GetRuns()))
	for _, summary := range res.GetRuns() {
		live = append(live, machine.LiveRun{
			RunID:   summary.GetRun().GetId(),
			NodeRef: summary.GetRun().GetNodeRef(),
			Step:    summary.GetCurrentStep(),
			Machine: summary.GetWorkspace().GetSandbox().GetMachine(),
		})
	}
	return live, nil
}

// RecordReconciliation writes the pass's report as one row of the ledger.
func (l *Ledger) RecordReconciliation(ctx context.Context, report machine.Report) error {
	findings := make([]*millv1.ReconciliationFinding, 0, len(report.Findings))
	for _, finding := range report.Findings {
		findings = append(findings, &millv1.ReconciliationFinding{
			Kind: string(finding.Kind), Name: finding.Name, Action: finding.Action, Detail: finding.Detail,
		})
	}
	_, err := call(ctx, l.store.RecordReconciliation, &millv1.RecordReconciliationRequest{
		WorkerRef: l.workerRef,
		StartedAt: timestamppb.New(report.StartedAt),
		Findings:  findings,
	})
	return err
}

var _ machine.Ledger = (*Ledger)(nil)
