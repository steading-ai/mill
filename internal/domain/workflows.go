package domain

import (
	"context"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/store"
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

// ListWorkflows lists workflow versions in a scope, newest first.
func (s *Service) ListWorkflows(ctx context.Context, p WorkflowFilter) ([]*millv1.Workflow, error) {
	return s.store.ListWorkflows(ctx, store.WorkflowFilter{
		Name:        p.Name,
		Scope:       p.Scope,
		Project:     p.Project,
		AllVersions: p.AllVersions,
	})
}

// PutWorkflow stores a workflow version. Idempotent by content hash: the
// same definition twice returns the existing version.
func (s *Service) PutWorkflow(ctx context.Context, workflow *millv1.Workflow) (*millv1.Workflow, bool, error) {
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
