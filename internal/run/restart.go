package run

import (
	"context"
	"errors"
	"fmt"

	daggerv1 "github.com/ivy/mill/gen/dagger/v1"
	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/dagger"
	"github.com/ivy/mill/internal/forge"
	"github.com/ivy/mill/internal/workspace"
)

// Restart is docs/domain-model.md §Restart, step for step. It is never
// automatic: the circuit breaker parks and a human restarts, because a step
// that fails repeatedly must cost a human decision rather than a generation.
//
// A restart is not a resume. The point of the new generation is that every
// session id in it is one the harness has never seen — [SessionID] is over
// the context id, the *generation* and the session's name — so the model
// starts from the work order rather than from the transcript that went
// wrong. Nothing of generation 1 is deleted: its step runs, evidence and
// usage stay as the record of what went wrong.
func (r *Runner) Restart(ctx context.Context, nodeRef, reason string) (*millv1.Run, error) {
	if reason == "" {
		reason = "restart"
	}
	node, err := r.options.Dagger.Node(ctx, nodeRef)
	if err != nil {
		return nil, err
	}
	if node.GetAssigneeKind() == daggerv1.AssigneeKind_ASSIGNEE_KIND_USER {
		return nil, &dagger.UserNodeError{Ref: nodeRef, Verb: "restart"}
	}
	old, err := r.runByNode(ctx, node.GetId())
	if err != nil {
		return nil, err
	}
	if old == nil {
		return nil, fmt.Errorf("%w: %s has no run to restart", ErrRefused, nodeRef)
	}
	if old.GetStatus() == millv1.RunStatus_RUN_STATUS_ABANDONED {
		return nil, fmt.Errorf("%w: run %s is already abandoned", ErrRefused, old.GetId())
	}

	if err := r.preempt(ctx, old, reason); err != nil {
		return nil, err
	}
	if _, err := call(ctx, r.options.Store.Abandon, &millv1.AbandonRequest{
		RunId:  old.GetId(),
		Reason: reason,
	}); err != nil {
		return nil, err
	}
	r.printf("run %s of %s abandoned at generation %d\n", old.GetId(), nodeRef, old.GetGeneration())

	r.discardWorkspace(ctx, old)
	if err := r.closeBranch(ctx, old, reason); err != nil {
		return nil, err
	}

	created, err := call(ctx, r.options.Store.CreateRun, &millv1.CreateRunRequest{
		NodeId:     node.GetId(),
		NodeRef:    node.GetReference(),
		ContextId:  node.GetContextId(),
		Generation: old.GetGeneration() + 1,
		WorkerRef:  r.options.WorkerRef,
		Branch:     workspace.BranchFor(node.GetReference()),
	})
	if err != nil {
		return nil, err
	}
	r.printf("%s restarted at generation %d as run %s\n",
		nodeRef, created.GetRun().GetGeneration(), created.GetRun().GetId())
	return created.GetRun(), nil
}

// preempt stops whoever holds the run. The cancel is recorded first so that
// a worker still alive learns why at its next heartbeat; the release is what
// actually takes the node back, with the reason docs/domain-model.md names —
// an operator taking a lease away is what `preempted` means.
func (r *Runner) preempt(ctx context.Context, old *millv1.Run, reason string) error {
	lease := old.GetLeaseId()
	if lease == "" {
		return nil
	}
	if _, err := call(ctx, r.options.Store.Cancel, &millv1.CancelRequest{
		RunId:  old.GetId(),
		Reason: reason,
	}); err != nil && !errors.Is(err, ErrFenced) {
		return err
	}
	if err := r.options.Dagger.Release(ctx, lease,
		daggerv1.ReleaseReason_RELEASE_REASON_PREEMPTED, reason); err != nil {
		return err
	}
	if _, err := call(ctx, r.options.Store.AbortOpenStepRuns, &millv1.AbortOpenStepRunsRequest{
		LeaseId: lease,
		RunId:   old.GetId(),
		Reason:  reason,
	}); err != nil {
		return err
	}
	return nil
}

// discardWorkspace destroys the tree the abandoned run worked in. A
// workspace this host never had is not an error: the restart is about the
// run, and a worker elsewhere left its own behind for its own cleanup.
func (r *Runner) discardWorkspace(ctx context.Context, old *millv1.Run) {
	ws, err := r.options.Workspaces(ctx, workspace.Spec{
		RunID:   old.GetId(),
		NodeRef: old.GetNodeRef(),
		Origin:  r.options.Origin,
		Ref:     old.GetBranch(),
	})
	if err != nil {
		r.options.Log.Debug("no workspace to destroy", "run", old.GetId(), "error", err)
		return
	}
	if err := ws.Destroy(ctx); err != nil {
		r.options.Log.Warn("destroying the workspace", "run", old.GetId(), "error", err)
	}
}

// closeBranch takes the abandoned generation off the forge: the pull request
// closed with a comment naming the restart, and the branch deleted. The new
// generation pushes the same branch name from the default branch again, so
// leaving the old one would have the next run build on work a human has
// already declared a dead end.
//
// A project with no forge configured has nothing to close, and a restart is
// not the moment to insist on one.
func (r *Runner) closeBranch(ctx context.Context, old *millv1.Run, reason string) error {
	client, err := r.options.Forges(ctx)
	if err != nil {
		r.options.Log.Debug("no forge to close the branch on", "run", old.GetId(), "error", err)
		return nil
	}
	comment := fmt.Sprintf(
		"Superseded: run %s of %s was restarted (%s). Generation %d starts from the default branch with fresh sessions.",
		old.GetId(), old.GetNodeRef(), reason, old.GetGeneration()+1)
	pr, err := client.PR(ctx, old.GetBranch())
	switch {
	case errors.Is(err, forge.ErrNoPullRequest):
	case err != nil:
		return err
	default:
		if err := client.ClosePR(ctx, pr, comment); err != nil {
			return err
		}
		r.printf("closed %s\n", pr.URL)
	}
	if err := client.DeleteBranch(ctx, old.GetBranch()); err != nil {
		return err
	}
	r.printf("deleted branch %s\n", old.GetBranch())
	return nil
}
