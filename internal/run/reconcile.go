package run

import (
	"context"

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
}

// NewLedger is the store's reconciliation view, recording under workerRef.
func NewLedger(store millv1connect.StoreServiceClient, workerRef string) *Ledger {
	return &Ledger{store: store, workerRef: workerRef}
}

// LiveRuns is every run the ledger holds in a non-terminal state, with the
// step it is at and the machine its workspace record names. `Status` with
// no filter is exactly that set.
func (l *Ledger) LiveRuns(ctx context.Context) ([]machine.LiveRun, error) {
	res, err := call(ctx, l.store.Status, &millv1.StatusRequest{})
	if err != nil {
		return nil, err
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
