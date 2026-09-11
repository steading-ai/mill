package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	millv1 "github.com/ivy/mill/gen/mill/v1"
)

// WorkspaceParams is what RecordWorkspace writes: the workspace as the run
// loop opened it (docs/ledger.md §workspaces).
type WorkspaceParams struct {
	RunID     string
	Kind      millv1.WorkspaceKind
	WorkerRef string
	Ref       string
	// Sandbox is the machine behind a machine-backed workspace; nil for one
	// with no machine.
	Sandbox *millv1.Sandbox
}

// sandboxLineage is one lineage as the `lineages` column holds it.
type sandboxLineage struct {
	Scope    string `json:"scope"`
	Name     string `json:"name"`
	Restored int64  `json:"restored"`
	Promoted *int64 `json:"promoted,omitempty"`
}

// RecordWorkspace records a run's workspace once it is open. A live row
// the run already has — the workspace a resume reattached — is brought up
// to date rather than joined by a second one, which the partial unique
// index on live rows would refuse. Fenced by the lease like every other
// write.
func (s *Store) RecordWorkspace(ctx context.Context, leaseID string, p WorkspaceParams) (*millv1.Workspace, error) {
	var workspace *millv1.Workspace
	err := s.withTx(ctx, func(ctx context.Context, tx pgx.Tx, q *Queries) error {
		if _, err := fence(ctx, tx, p.RunID, leaseID); err != nil {
			return err
		}
		lineages, err := encodeLineages(p.Sandbox)
		if err != nil {
			return err
		}
		var row Workspace
		live, err := q.GetLiveWorkspace(ctx, p.RunID)
		switch {
		case err == nil:
			row, err = q.UpdateWorkspace(ctx, UpdateWorkspaceParams{
				ID: live.ID, Kind: enumText(p.Kind), WorkerRef: p.WorkerRef, Ref: p.Ref,
				Machine: p.Sandbox.GetMachine(), Unit: p.Sandbox.GetUnit(),
				ImageDigest: p.Sandbox.GetImageDigest(), Veth: p.Sandbox.GetVeth(), Lineages: lineages,
			})
		case errors.Is(err, pgx.ErrNoRows):
			row, err = q.InsertWorkspace(ctx, InsertWorkspaceParams{
				ID: newULID(), RunID: p.RunID, Kind: enumText(p.Kind), WorkerRef: p.WorkerRef, Ref: p.Ref,
				Machine: p.Sandbox.GetMachine(), Unit: p.Sandbox.GetUnit(),
				ImageDigest: p.Sandbox.GetImageDigest(), Veth: p.Sandbox.GetVeth(), Lineages: lineages,
			})
		default:
			return fmt.Errorf("live workspace of run %s: %w", p.RunID, err)
		}
		if err != nil {
			return fmt.Errorf("record workspace of run %s: %w", p.RunID, err)
		}
		if workspace, err = workspaceProto(row); err != nil {
			return err
		}
		after := map[string]any{"workspace_id": row.ID, "kind": row.Kind, "ref": row.Ref}
		if row.Machine != "" {
			after["machine"] = row.Machine
			after["image_digest"] = row.ImageDigest
		}
		return record(ctx, q, event{runID: p.RunID, op: "record_workspace", after: after, workerRef: p.WorkerRef})
	})
	if err != nil {
		return nil, err
	}
	return workspace, nil
}

// FinishWorkspace records the workspace's end: its files destroyed and, for
// each lineage the run promoted, the generation it made. It is the one
// write made after the run has ended — the files go once the run is marked
// done, and the promotion happens as they go — so the fence admits it when
// the run has ended and holds no lease; while the run is live the lease is
// compared as for every other write.
func (s *Store) FinishWorkspace(ctx context.Context, leaseID, runID string, promoted []*millv1.SandboxLineage) (*millv1.Workspace, error) {
	var workspace *millv1.Workspace
	err := s.withTx(ctx, func(ctx context.Context, tx pgx.Tx, q *Queries) error {
		state, err := lockRunRow(ctx, tx, runID)
		if err != nil {
			return err
		}
		ended := state.lease == nil && terminalStatuses[state.status]
		if !ended && (state.lease == nil || *state.lease != leaseID) {
			return fmt.Errorf("run %s holds lease %s, not %s: %w",
				runID, leaseText(state.lease), leaseText(&leaseID), ErrFenced)
		}
		live, err := q.GetLiveWorkspace(ctx, runID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("run %s has no live workspace: %w", runID, ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("live workspace of run %s: %w", runID, err)
		}
		lineages, err := promoteLineages(live.Lineages, promoted)
		if err != nil {
			return fmt.Errorf("workspace %s: %w", live.ID, err)
		}
		row, err := q.DestroyWorkspace(ctx, DestroyWorkspaceParams{ID: live.ID, Lineages: lineages})
		if err != nil {
			return fmt.Errorf("finish workspace of run %s: %w", runID, err)
		}
		if workspace, err = workspaceProto(row); err != nil {
			return err
		}
		after := map[string]any{"workspace_id": row.ID, "state": row.State}
		if len(promoted) > 0 {
			names := make([]string, 0, len(promoted))
			for _, lineage := range promoted {
				names = append(names, fmt.Sprintf("%s/%s@%d", lineage.GetScope(), lineage.GetName(), lineage.GetPromoted()))
			}
			after["promoted"] = names
		}
		return record(ctx, q, event{runID: runID, op: "finish_workspace", after: after, workerRef: row.WorkerRef})
	})
	if err != nil {
		return nil, err
	}
	return workspace, nil
}

// LatestWorkspace is the run's newest workspace row, live or destroyed, or
// nil when the run has recorded none.
func latestWorkspace(ctx context.Context, q *Queries, runID string) (*millv1.Workspace, error) {
	row, err := q.GetLatestWorkspace(ctx, runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil //nolint:nilnil // no row is not an error: the run has recorded no workspace
	}
	if err != nil {
		return nil, fmt.Errorf("workspace of run %s: %w", runID, err)
	}
	return workspaceProto(row)
}

// encodeLineages renders a sandbox's lineages for the column: NULL for a
// workspace with no machine.
func encodeLineages(sandbox *millv1.Sandbox) ([]byte, error) {
	if sandbox == nil {
		return nil, nil
	}
	lineages := make([]sandboxLineage, 0, len(sandbox.GetLineages()))
	for _, lineage := range sandbox.GetLineages() {
		entry := sandboxLineage{Scope: lineage.GetScope(), Name: lineage.GetName(), Restored: lineage.GetRestored()}
		if lineage.Promoted != nil {
			promoted := lineage.GetPromoted()
			entry.Promoted = &promoted
		}
		lineages = append(lineages, entry)
	}
	encoded, err := json.Marshal(lineages)
	if err != nil {
		return nil, fmt.Errorf("encode lineages: %w", err)
	}
	return encoded, nil
}

// promoteLineages writes each promoted generation onto the lineage it
// belongs to. A promotion of a lineage the run never restored is refused:
// the record says what the run had, and a promotion of something it did not
// have is a bug in the caller.
func promoteLineages(stored []byte, promoted []*millv1.SandboxLineage) ([]byte, error) {
	if len(promoted) == 0 {
		return stored, nil
	}
	if stored == nil {
		return nil, errors.New("a workspace with no machine promoted a lineage")
	}
	var lineages []sandboxLineage
	if err := json.Unmarshal(stored, &lineages); err != nil {
		return nil, fmt.Errorf("decode lineages: %w", err)
	}
	for _, p := range promoted {
		found := false
		for i := range lineages {
			if lineages[i].Scope == p.GetScope() && lineages[i].Name == p.GetName() {
				generation := p.GetPromoted()
				lineages[i].Promoted = &generation
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("lineage %s/%s was never restored", p.GetScope(), p.GetName())
		}
	}
	encoded, err := json.Marshal(lineages)
	if err != nil {
		return nil, fmt.Errorf("encode lineages: %w", err)
	}
	return encoded, nil
}

// workspaceProto renders a workspace row.
func workspaceProto(row Workspace) (*millv1.Workspace, error) {
	workspace := &millv1.Workspace{
		Id:          row.ID,
		RunId:       row.RunID,
		Kind:        parseEnum[millv1.WorkspaceKind](row.Kind),
		Ref:         row.Ref,
		WorkerRef:   row.WorkerRef,
		State:       parseEnum[millv1.WorkspaceState](row.State),
		CreatedAt:   stamp(&row.CreatedAt),
		DestroyedAt: stamp(row.DestroyedAt),
	}
	if row.Machine == "" && row.Lineages == nil {
		return workspace, nil
	}
	sandbox := &millv1.Sandbox{
		Machine: row.Machine, Unit: row.Unit, ImageDigest: row.ImageDigest, Veth: row.Veth,
	}
	if row.Lineages != nil {
		var lineages []sandboxLineage
		if err := json.Unmarshal(row.Lineages, &lineages); err != nil {
			return nil, fmt.Errorf("decode lineages of workspace %s: %w", row.ID, err)
		}
		sandbox.Lineages = make([]*millv1.SandboxLineage, 0, len(lineages))
		for _, lineage := range lineages {
			sandbox.Lineages = append(sandbox.Lineages, &millv1.SandboxLineage{
				Scope: lineage.Scope, Name: lineage.Name, Restored: lineage.Restored, Promoted: lineage.Promoted,
			})
		}
	}
	workspace.Sandbox = sandbox
	return workspace, nil
}
