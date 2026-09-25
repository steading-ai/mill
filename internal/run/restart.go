package run

import (
	"context"
	"errors"
	"fmt"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/forge"
	"github.com/steading-ai/mill/internal/skift"
	"github.com/steading-ai/mill/internal/workspace"
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
	node, err := r.options.Skift.Node(ctx, nodeRef)
	if err != nil {
		return nil, err
	}
	if node.GetAssigneeKind() == skiftv1.AssigneeKind_ASSIGNEE_KIND_USER {
		return nil, &skift.UserNodeError{Ref: nodeRef, Verb: "restart"}
	}
	// A closed node has no next generation to start: a restart of a
	// dropped node would open a run nothing can ever claim, which is
	// exactly the stranded run the sweep exists to end (docs/ledger.md
	// §Sweep).
	if node.GetStatus() != skiftv1.Status_STATUS_OPEN {
		return nil, fmt.Errorf("%w: %s is %s, not open; nothing to restart",
			ErrRefused, nodeRef, skiftStatus(node.GetStatus()))
	}
	old, next, err := r.runByNode(ctx, node.GetId())
	if err != nil {
		return nil, err
	}
	switch {
	case old == nil && next == 1:
		return nil, fmt.Errorf("%w: %s has no run to restart", ErrRefused, nodeRef)
	case old == nil:
		// The last generation is abandoned and nothing replaced it: a
		// restart that got as far as the abandon and stopped — the forge
		// refused to close the branch, say. Running it again finishes it;
		// there is nothing left to take back, discard or close.
		r.printf("%s has no live run; generation %d is abandoned, finishing its restart\n", nodeRef, next-1)
	default:
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

		// A restart closes the pull request and deletes the branch, so it
		// needs the same repository a run has. A project with none still
		// restarts; only the forge half is skipped.
		repo, err := r.repository(ctx, nil)
		if err != nil {
			r.options.Log.Debug("no repository for the restart", "run", old.GetId(), "error", err)
		}
		r.discardWorkspace(ctx, old, repo)
		comment := fmt.Sprintf(
			"Superseded: run %s of %s was restarted (%s). Generation %d starts from the default branch with fresh sessions.",
			old.GetId(), old.GetNodeRef(), reason, old.GetGeneration()+1)
		if err := r.closeBranch(ctx, old, comment, repo); err != nil {
			return nil, err
		}
	}

	created, err := call(ctx, r.options.Store.CreateRun, &millv1.CreateRunRequest{
		NodeId:     node.GetId(),
		NodeRef:    node.GetReference(),
		ContextId:  node.GetContextId(),
		Generation: next,
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
//
// A lease skift says is not live is not an error here. The run whose worker
// died holding one is the very run a human restarts, and the release exists
// only to stop a worker that is still alive: there is nothing to take back
// from a lease that is already gone. It is named in the output and the
// restart carries on, because a restart that stopped there would leave the
// node recoverable by nothing.
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
	switch err := r.options.Skift.Release(ctx, lease,
		skiftv1.ReleaseReason_RELEASE_REASON_PREEMPTED, reason); {
	case errors.Is(err, skift.ErrFenced):
		r.printf("lease %s is no longer live in skift; skipped the release\n", lease)
	case err != nil:
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

// discardWorkspace destroys the tree a run that has ended worked in, through
// the attach-only seam: it never makes a workspace in order to delete it. A
// workspace this host never had is not an error: the restart or the sweep
// is about the run, and a worker elsewhere left its own behind for its own
// cleanup.
func (r *Runner) discardWorkspace(ctx context.Context, old *millv1.Run, repo Repository) {
	if r.options.Attach == nil {
		r.options.Log.Debug("no attach-only workspaces; nothing destroyed", "run", old.GetId())
		return
	}
	ws, err := r.options.Attach(ctx, workspace.Spec{
		RunID:   old.GetId(),
		NodeRef: old.GetNodeRef(),
		Origin:  repo.Origin,
		Ref:     old.GetBranch(),
	}, repo)
	if err != nil {
		r.options.Log.Debug("no workspace to destroy", "run", old.GetId(), "error", err)
		return
	}
	if err := ws.Destroy(ctx); err != nil {
		r.options.Log.Warn("destroying the workspace", "run", old.GetId(), "error", err)
		return
	}
	// The run has ended and holds no lease, which is what admits the
	// record of its workspace's end. A run that recorded no workspace has
	// no row to finish, and that is a note rather than a failure.
	if err := r.finishWorkspaceOf(ctx, "", old.GetId(), ws); err != nil {
		r.options.Log.Debug("recording the workspace's end", "run", old.GetId(), "error", err)
	}
}

// closeBranch takes a run that has ended off the forge: the pull request
// closed with the comment given, and the branch deleted. After a restart
// the new generation pushes the same branch name from the default branch
// again, so leaving the old one would have the next run build on work a
// human has already declared a dead end; after a sweep nothing will push
// it again, and a branch nobody will merge is noise on the forge.
//
// A project with no repository has nothing to close, and neither moment is
// one to insist on one.
func (r *Runner) closeBranch(ctx context.Context, old *millv1.Run, comment string, repo Repository) error {
	client := repo.Forge
	if client == nil {
		r.options.Log.Debug("no forge to close the branch on", "run", old.GetId())
		return nil
	}
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
