package domain

import (
	"context"
	"fmt"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/store"
)

// GetWorkflowParams names one workflow version: by id, or by name resolved
// in a scope.
type GetWorkflowParams struct {
	ID      string
	Name    string
	Scope   millv1.Scope
	Project string
	Version string
}

// WorkflowFilter narrows a listing.
type WorkflowFilter struct {
	Name        string
	Scope       millv1.Scope
	Project     string
	AllVersions bool
}

// GetWorkflow returns one workflow version, or nil when nothing resolves.
func (s *Service) GetWorkflow(ctx context.Context, p GetWorkflowParams) (*millv1.Workflow, error) {
	return s.store.GetWorkflow(ctx, store.GetWorkflowParams{
		ID:      p.ID,
		Name:    p.Name,
		Scope:   p.Scope,
		Project: p.Project,
		Version: p.Version,
	})
}

// UndecodableWorkflow is a workflow row a listing could not decode and left
// out, reported beside the rest. The row is not touched.
type UndecodableWorkflow = store.UndecodableWorkflow

// ListWorkflows lists workflow versions in a scope, newest first, with the
// rows it could not decode named beside them.
func (s *Service) ListWorkflows(ctx context.Context, p WorkflowFilter) ([]*millv1.Workflow, []UndecodableWorkflow, error) {
	return s.store.ListWorkflows(ctx, store.WorkflowFilter{
		Name:        p.Name,
		Scope:       p.Scope,
		Project:     p.Project,
		AllVersions: p.AllVersions,
	})
}

// McpCollisionError is a workflow whose mcp-servers names a server the host
// already provides. The name is reserved by the host's own configuration
// (docs/tools.md §Configuration), so a workflow declaring it would be
// ambiguous about which server a step's `mcp` names.
type McpCollisionError struct {
	Name string
}

func (e *McpCollisionError) Error() string {
	return fmt.Sprintf("mcp-servers: %q collides with a server the host already configures", e.Name)
}

// CheckMcpCollision refuses a workflow whose own mcp-servers names a server
// hostMcpServers already reserves, so a step's `mcp` name is never ambiguous
// between the two catalogs. hostMcpServers is the worker handling the call's
// own configured names, when one runs in this process (`serve --work`, the
// only deployment this repo documents — a `serve` fronting more than one
// `work` process is not modelled); nil checks against nothing.
func CheckMcpCollision(workflow *millv1.Workflow, hostMcpServers map[string]struct{}) error {
	for name := range workflow.GetMcpServers() {
		if _, collides := hostMcpServers[name]; collides {
			return &McpCollisionError{Name: name}
		}
	}
	return nil
}

// PutWorkflow stores a workflow version. Idempotent by content hash: the
// same definition twice returns the existing version. hostMcpServers is
// CheckMcpCollision's own; a colliding workflow is refused before anything
// is stored.
func (s *Service) PutWorkflow(ctx context.Context, workflow *millv1.Workflow, hostMcpServers map[string]struct{}) (*millv1.Workflow, bool, error) {
	if err := CheckMcpCollision(workflow, hostMcpServers); err != nil {
		return nil, false, err
	}
	return s.store.PutWorkflow(ctx, workflow)
}

// PutSkill stores a skill by name and digest, so a workflow version pins the
// exact prompt it ran. Idempotent by digest.
func (s *Service) PutSkill(ctx context.Context, skill *millv1.Skill) (*millv1.Skill, bool, error) {
	return s.store.PutSkill(ctx, skill)
}

// GetSkill returns the skill a workflow step pins, by name and digest.
func (s *Service) GetSkill(ctx context.Context, name, digest string) (*millv1.Skill, error) {
	return s.store.GetSkill(ctx, name, digest)
}
