package run

import (
	"context"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/notify"
)

// Carrying a filed gate to a person.
//
// A gate in dagger is an answer nobody has been asked for yet: the node is
// blocked, the run has given its lease back, and until somebody opens dagger
// nothing says so. Dagger's own notifier is what will wake them, and until
// that bridge exists the deployment is handed the gate through one
// configured command (docs/architecture.md §"Gate steps", docs/prd.md D1).
//
// Everything about it is deliberately weak. The mill runs the command, waits
// no more than [notify.Timeout], writes what happened to the audit trail,
// and carries on. A command that fails, hangs, or does not exist changes
// nothing about the run: the gate is filed and the run waits either way,
// because the record a human answers is in dagger and not in the message.

// notified runs the configured command for one filed gate and records what
// it did. It is called while the gate's lease is still held — the write that
// parks or waits the run takes the lease off it, and this row of the trail
// is fenced like every other (internal/store/store.go, RecordNotice).
func (r *Runner) notified(ctx context.Context, s *state, stepRunID string, notice notify.Notice) {
	if r.options.Notifier == nil {
		return
	}
	notice.Node = s.node.GetReference()
	notice.Run = s.run.GetId()
	result, err := r.options.Notifier.Notify(ctx, notice)
	if err != nil {
		// Nothing ran, so there is nothing about the world to record: this
		// is the mill misconfigured, and the log is where that belongs.
		r.options.Log.Warn("notify", "gate", notice.Gate, "kind", notice.Kind, "error", err)
		r.printf("  the notify command could not be run: %v\n", err)
		return
	}
	if result.OK() {
		r.printf("  %s carried to the notifier\n", notice.Gate)
	} else {
		r.options.Log.Warn("notify", "gate", notice.Gate, "kind", notice.Kind,
			"exit", result.Status, "detail", result.Detail)
		r.printf("  the notify command failed for %s: %s\n", notice.Gate, result)
	}
	if _, err := call(ctx, r.options.Store.RecordNotice, &millv1.RecordNoticeRequest{
		LeaseId:   s.leaseID(),
		RunId:     s.run.GetId(),
		StepRunId: stepRunID,
		Kind:      string(notice.Kind),
		GateRef:   notice.Gate,
		ExitCode:  result.Status,
		Detail:    result.Detail,
	}); err != nil {
		// The audit trail's own write, and the last thing that should stop a
		// run from parking on the gate it has already filed.
		r.options.Log.Warn("notify", "gate", notice.Gate, "recording the notice", err)
	}
}

// noticeKind is what a gate step's gate decides, as a notifier routes on it.
// The mill knows this from the evidence the step closes on and nothing else:
// a gate declaring `status` evidence authorizes the merge of the branch it
// is about ([Runner.approve]), and a gate declaring anything else is a
// decision about the work itself — a plan to approve — that authorizes no
// act. The circuit breaker's own gate is neither, and says so where it files
// it (internal/run/breaker.go).
func noticeKind(step *millv1.Step) notify.Kind {
	if step.GetEvidence() == millv1.EvidenceKind_EVIDENCE_KIND_STATUS {
		return notify.KindMerge
	}
	return notify.KindPlan
}
