package run

import (
	"context"
	"fmt"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/workspace"
)

// recordWorkspace writes the ledger's record of the run's workspace once it
// is open: its kind, this worker, the ref it holds, and — for the machine
// backend — the machine, unit, digest, veth and restored lineages the
// handle carries (docs/ledger.md §workspaces, docs/sandbox.md
// §Reconciliation). A workspace reattached on resume brings its row up to
// date rather than adding one.
func (r *Runner) recordWorkspace(ctx context.Context, s *state) error {
	handle := s.workspace.Handle()
	if handle == nil {
		return fmt.Errorf("run %s: the workspace is open but has no handle", s.run.GetId())
	}
	_, err := call(ctx, r.options.Store.RecordWorkspace, &millv1.RecordWorkspaceRequest{
		LeaseId:   s.leaseID(),
		RunId:     s.run.GetId(),
		Kind:      workspaceKindProto(handle.Kind),
		WorkerRef: r.options.WorkerRef,
		Ref:       handle.Ref,
		Sandbox:   sandboxProto(handle.Sandbox),
	})
	return err
}

// finishWorkspace records the workspace's end after Destroy: its files are
// gone and, on the machine backend, each lineage's promoted generation is
// what Destroy's promotion made of it. It is written after the run is done,
// which the store admits for a run that has ended.
func (r *Runner) finishWorkspace(ctx context.Context, s *state) error {
	return r.finishWorkspaceOf(ctx, s.leaseID(), s.run.GetId(), s.workspace)
}

// finishWorkspaceOf is finishWorkspace for a workspace the loop is not
// driving a run through: the one a restart discards, under no lease, after
// the run is abandoned.
func (r *Runner) finishWorkspaceOf(ctx context.Context, lease, runID string, ws Workspace) error {
	var promoted []*millv1.SandboxLineage
	if promoter, ok := ws.(workspace.Promoter); ok {
		for _, lineage := range promoter.Promoted() {
			if lineage.Promoted == nil {
				continue
			}
			generation := int64(*lineage.Promoted)
			promoted = append(promoted, &millv1.SandboxLineage{
				Scope: lineage.Scope, Name: lineage.Name, Restored: int64(lineage.Restored), Promoted: &generation,
			})
		}
	}
	_, err := call(ctx, r.options.Store.FinishWorkspace, &millv1.FinishWorkspaceRequest{
		LeaseId: lease, RunId: runID, Promoted: promoted,
	})
	return err
}

// workspaceKindProto is a backend's kind as the ledger spells it.
func workspaceKindProto(kind workspace.Kind) millv1.WorkspaceKind {
	switch kind {
	case workspace.KindLocal:
		return millv1.WorkspaceKind_WORKSPACE_KIND_LOCAL
	case workspace.KindMachine:
		return millv1.WorkspaceKind_WORKSPACE_KIND_MACHINE
	case workspace.KindRemote:
		return millv1.WorkspaceKind_WORKSPACE_KIND_REMOTE
	default:
		return millv1.WorkspaceKind_WORKSPACE_KIND_UNSPECIFIED
	}
}

// sandboxProto is the handle's sandbox as the ledger records it; nil for a
// workspace with no machine.
func sandboxProto(sandbox *workspace.Sandbox) *millv1.Sandbox {
	if sandbox == nil {
		return nil
	}
	out := &millv1.Sandbox{
		Machine: sandbox.Machine, Unit: sandbox.Unit, ImageDigest: sandbox.Image, Veth: sandbox.Veth,
		Lineages: make([]*millv1.SandboxLineage, 0, len(sandbox.Lineages)),
	}
	for _, lineage := range sandbox.Lineages {
		entry := &millv1.SandboxLineage{Scope: lineage.Scope, Name: lineage.Name, Restored: int64(lineage.Restored)}
		if lineage.Promoted != nil {
			generation := int64(*lineage.Promoted)
			entry.Promoted = &generation
		}
		out.Lineages = append(out.Lineages, entry)
	}
	return out
}
