package server

import (
	"context"
	"time"

	"connectrpc.com/connect"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/gen/mill/v1/millv1connect"
	"github.com/ivy/mill/internal/domain"
	"github.com/ivy/mill/internal/server/interceptor"
	"github.com/ivy/mill/internal/version"
)

// StoreService is the mill.v1 StoreService over the ledger. A handler maps
// the wire form to the domain and back and does nothing else: it holds no
// rule of its own, and it never reaches the store, which depguard enforces.
// Who the caller is comes from the connection, never from a request body.
type StoreService struct {
	svc *domain.Service
}

// NewStoreService mounts svc as the store's handlers.
func NewStoreService(svc *domain.Service) *StoreService { return &StoreService{svc: svc} }

var _ millv1connect.StoreServiceHandler = (*StoreService)(nil)

// GetRunByNode returns the live run for a dagger node, or nothing.
func (s *StoreService) GetRunByNode(ctx context.Context, req *connect.Request[millv1.GetRunByNodeRequest]) (*connect.Response[millv1.GetRunByNodeResponse], error) {
	run, err := s.svc.GetRunByNode(ctx, req.Msg.GetNodeId())
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&millv1.GetRunByNodeResponse{Run: run}), nil
}

// CreateRun opens a run on a claimed node, in status selecting.
func (s *StoreService) CreateRun(ctx context.Context, req *connect.Request[millv1.CreateRunRequest]) (*connect.Response[millv1.CreateRunResponse], error) {
	run, err := s.svc.CreateRun(ctx, req.Msg.GetLeaseId(), domain.CreateRunParams{
		NodeID:     req.Msg.GetNodeId(),
		NodeRef:    req.Msg.GetNodeRef(),
		ContextID:  req.Msg.GetContextId(),
		Generation: req.Msg.GetGeneration(),
		WorkerRef:  req.Msg.GetWorkerRef(),
		Branch:     req.Msg.GetBranch(),
	})
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&millv1.CreateRunResponse{Run: run}), nil
}

// SetLease makes a claim's lease the run's current one.
func (s *StoreService) SetLease(ctx context.Context, req *connect.Request[millv1.SetLeaseRequest]) (*connect.Response[millv1.SetLeaseResponse], error) {
	run, err := s.svc.SetLease(ctx, req.Msg.GetRunId(), req.Msg.GetLeaseId(), req.Msg.GetWorkerRef())
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&millv1.SetLeaseResponse{Run: run}), nil
}

// StartStepRun opens the next attempt at a step.
func (s *StoreService) StartStepRun(ctx context.Context, req *connect.Request[millv1.StartStepRunRequest]) (*connect.Response[millv1.StartStepRunResponse], error) {
	stepRun, err := s.svc.StartStepRun(ctx, req.Msg.GetLeaseId(), domain.StartStepRunParams{
		RunID:        req.Msg.GetRunId(),
		StepName:     req.Msg.GetStepName(),
		Kind:         req.Msg.GetKind(),
		SessionID:    req.Msg.GetSessionId(),
		WorkerRef:    req.Msg.GetWorkerRef(),
		FeedbackFrom: req.Msg.GetFeedbackFrom(),
		BaseCommit:   req.Msg.GetBaseCommit(),
	})
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&millv1.StartStepRunResponse{StepRun: stepRun}), nil
}

// RecordStepEnvironment records what a skill step's session process was
// given of the mill's own environment.
func (s *StoreService) RecordStepEnvironment(ctx context.Context, req *connect.Request[millv1.RecordStepEnvironmentRequest]) (*connect.Response[millv1.RecordStepEnvironmentResponse], error) {
	stepRun, err := s.svc.RecordStepEnvironment(ctx, req.Msg.GetLeaseId(), domain.RecordStepEnvironmentParams{
		StepRunID:      req.Msg.GetStepRunId(),
		EnvPassthrough: req.Msg.GetEnvPassthrough(),
		EnvStripped:    req.Msg.GetEnvStripped(),
	})
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&millv1.RecordStepEnvironmentResponse{StepRun: stepRun}), nil
}

// MarkSessionStarted records that an attempt's session has produced its first
// event.
func (s *StoreService) MarkSessionStarted(ctx context.Context, req *connect.Request[millv1.MarkSessionStartedRequest]) (*connect.Response[millv1.MarkSessionStartedResponse], error) {
	stepRun, err := s.svc.MarkSessionStarted(ctx, req.Msg.GetLeaseId(), req.Msg.GetStepRunId())
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&millv1.MarkSessionStartedResponse{StepRun: stepRun}), nil
}

// CloseStepRun records the evidence that closes an attempt.
func (s *StoreService) CloseStepRun(ctx context.Context, req *connect.Request[millv1.CloseStepRunRequest]) (*connect.Response[millv1.CloseStepRunResponse], error) {
	stepRun, evidence, err := s.svc.CloseStepRun(ctx, req.Msg.GetLeaseId(), domain.CloseStepRunParams{
		StepRunID:    req.Msg.GetStepRunId(),
		Evidence:     req.Msg.GetEvidence(),
		WorkflowID:   req.Msg.GetWorkflowId(),
		Tier:         req.Msg.GetTier(),
		AutoApproved: req.Msg.GetAutoApproved(),
	})
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&millv1.CloseStepRunResponse{StepRun: stepRun, Evidence: evidence}), nil
}

// FailStepRun marks an attempt failed with how its process ended.
func (s *StoreService) FailStepRun(ctx context.Context, req *connect.Request[millv1.FailStepRunRequest]) (*connect.Response[millv1.FailStepRunResponse], error) {
	stepRun, err := s.svc.FailStepRun(ctx, req.Msg.GetLeaseId(), req.Msg.GetStepRunId(), req.Msg.GetExit())
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&millv1.FailStepRunResponse{StepRun: stepRun}), nil
}

// AbortOpenStepRuns marks a dead or fenced-out worker's attempts aborted.
func (s *StoreService) AbortOpenStepRuns(ctx context.Context, req *connect.Request[millv1.AbortOpenStepRunsRequest]) (*connect.Response[millv1.AbortOpenStepRunsResponse], error) {
	stepRuns, err := s.svc.AbortOpenStepRuns(ctx, req.Msg.GetLeaseId(), req.Msg.GetRunId(), req.Msg.GetReason())
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&millv1.AbortOpenStepRunsResponse{StepRuns: stepRuns}), nil
}

// SupersedeSteps reopens a span of steps for a revise.
func (s *StoreService) SupersedeSteps(ctx context.Context, req *connect.Request[millv1.SupersedeStepsRequest]) (*connect.Response[millv1.SupersedeStepsResponse], error) {
	stepRuns, err := s.svc.SupersedeSteps(ctx, req.Msg.GetLeaseId(), req.Msg.GetRunId(), req.Msg.GetFromStep(), req.Msg.GetThroughStep())
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&millv1.SupersedeStepsResponse{StepRuns: stepRuns}), nil
}

// MarkWaiting records the gate a run is blocked on and marks it waiting.
func (s *StoreService) MarkWaiting(ctx context.Context, req *connect.Request[millv1.MarkWaitingRequest]) (*connect.Response[millv1.MarkWaitingResponse], error) {
	run, err := s.svc.MarkWaiting(ctx, req.Msg.GetLeaseId(), req.Msg.GetRunId(), req.Msg.GetGateNodeId(), req.Msg.GetStepRunId())
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&millv1.MarkWaitingResponse{Run: run}), nil
}

// Park trips the circuit breaker.
func (s *StoreService) Park(ctx context.Context, req *connect.Request[millv1.ParkRequest]) (*connect.Response[millv1.ParkResponse], error) {
	run, err := s.svc.Park(ctx, req.Msg.GetLeaseId(), req.Msg.GetRunId(), req.Msg.GetGateNodeId(), req.Msg.GetReason())
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&millv1.ParkResponse{Run: run}), nil
}

// RecordNotice appends what carrying a filed gate to a person did.
func (s *StoreService) RecordNotice(ctx context.Context, req *connect.Request[millv1.RecordNoticeRequest]) (*connect.Response[millv1.RecordNoticeResponse], error) {
	if err := s.svc.RecordNotice(ctx, req.Msg.GetLeaseId(), req.Msg.GetRunId(), req.Msg.GetStepRunId(),
		req.Msg.GetKind(), req.Msg.GetGateRef(), req.Msg.GetExitCode(), req.Msg.GetDetail()); err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&millv1.RecordNoticeResponse{}), nil
}

// Done marks a run done, after dagger has confirmed the completing patch.
// SetCompleteRequestId mints the completing patch's idempotency key before
// that patch is sent.
func (s *StoreService) SetCompleteRequestId(ctx context.Context, req *connect.Request[millv1.SetCompleteRequestIdRequest]) (*connect.Response[millv1.SetCompleteRequestIdResponse], error) {
	run, err := s.svc.SetCompleteRequestID(ctx, req.Msg.GetLeaseId(), req.Msg.GetRunId(), req.Msg.GetRequestId())
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&millv1.SetCompleteRequestIdResponse{Run: run}), nil
}

func (s *StoreService) Done(ctx context.Context, req *connect.Request[millv1.DoneRequest]) (*connect.Response[millv1.DoneResponse], error) {
	run, err := s.svc.Done(ctx, req.Msg.GetLeaseId(), req.Msg.GetRunId())
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&millv1.DoneResponse{Run: run}), nil
}

// Fail ends a run a gate denied, after dagger has been told with the gate's
// deny_result.
func (s *StoreService) Fail(ctx context.Context, req *connect.Request[millv1.FailRequest]) (*connect.Response[millv1.FailResponse], error) {
	run, err := s.svc.Fail(ctx, req.Msg.GetLeaseId(), req.Msg.GetRunId(), req.Msg.GetReason())
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&millv1.FailResponse{Run: run}), nil
}

// Resume clears the gate an answered run stopped on.
func (s *StoreService) Resume(ctx context.Context, req *connect.Request[millv1.ResumeRequest]) (*connect.Response[millv1.ResumeResponse], error) {
	run, err := s.svc.Resume(ctx, req.Msg.GetLeaseId(), req.Msg.GetRunId())
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&millv1.ResumeResponse{Run: run}), nil
}

// Abandon marks a run abandoned so a new generation can be created.
func (s *StoreService) Abandon(ctx context.Context, req *connect.Request[millv1.AbandonRequest]) (*connect.Response[millv1.AbandonResponse], error) {
	run, err := s.svc.Abandon(ctx, req.Msg.GetLeaseId(), req.Msg.GetRunId(), req.Msg.GetReason())
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&millv1.AbandonResponse{Run: run}), nil
}

// Cancel asks a run's worker to stop at its next heartbeat. Who asked is the
// connection's caller, recorded on the event; the request carries no
// principal of its own and could not be believed if it did.
func (s *StoreService) Cancel(ctx context.Context, req *connect.Request[millv1.CancelRequest]) (*connect.Response[millv1.CancelResponse], error) {
	run, err := s.svc.Cancel(ctx, req.Msg.GetRunId(), callerRef(ctx), req.Msg.GetReason())
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&millv1.CancelResponse{Run: run}), nil
}

// IssueStepToken mints the bearer a session presents on the workspace
// socket.
func (s *StoreService) IssueStepToken(ctx context.Context, req *connect.Request[millv1.IssueStepTokenRequest]) (*connect.Response[millv1.IssueStepTokenResponse], error) {
	token, err := s.svc.IssueStepToken(ctx, req.Msg.GetLeaseId(), req.Msg.GetStepRunId(), req.Msg.GetCapabilities())
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&millv1.IssueStepTokenResponse{StepToken: token}), nil
}

// RevokeStepToken revokes a bearer.
func (s *StoreService) RevokeStepToken(ctx context.Context, req *connect.Request[millv1.RevokeStepTokenRequest]) (*connect.Response[millv1.RevokeStepTokenResponse], error) {
	token, err := s.svc.RevokeStepToken(ctx, req.Msg.GetLeaseId(), req.Msg.GetStepTokenId())
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&millv1.RevokeStepTokenResponse{StepToken: token}), nil
}

// RecordUsage appends what a turn cost, per model.
func (s *StoreService) RecordUsage(ctx context.Context, req *connect.Request[millv1.RecordUsageRequest]) (*connect.Response[millv1.RecordUsageResponse], error) {
	usage, err := s.svc.RecordUsage(ctx, req.Msg.GetLeaseId(), req.Msg.GetStepRunId(), req.Msg.GetUsage())
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&millv1.RecordUsageResponse{Usage: usage}), nil
}

// MarkUsagePosted marks a step run's usage rows as posted to dagger.
func (s *StoreService) MarkUsagePosted(ctx context.Context, req *connect.Request[millv1.MarkUsagePostedRequest]) (*connect.Response[millv1.MarkUsagePostedResponse], error) {
	marked, err := s.svc.MarkUsagePosted(ctx, req.Msg.GetLeaseId(), req.Msg.GetStepRunId())
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&millv1.MarkUsagePostedResponse{Marked: marked}), nil
}

// GetWorkflow returns one workflow version, by id or by name in a scope.
func (s *StoreService) GetWorkflow(ctx context.Context, req *connect.Request[millv1.GetWorkflowRequest]) (*connect.Response[millv1.GetWorkflowResponse], error) {
	workflow, err := s.svc.GetWorkflow(ctx, domain.GetWorkflowParams{
		ID:      req.Msg.GetId(),
		Name:    req.Msg.GetName(),
		Scope:   req.Msg.GetScope(),
		Project: req.Msg.GetProject(),
		Version: req.Msg.GetVersion(),
	})
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&millv1.GetWorkflowResponse{Workflow: workflow}), nil
}

// ListWorkflows lists workflow versions in a scope, newest first.
func (s *StoreService) ListWorkflows(ctx context.Context, req *connect.Request[millv1.ListWorkflowsRequest]) (*connect.Response[millv1.ListWorkflowsResponse], error) {
	workflows, err := s.svc.ListWorkflows(ctx, domain.WorkflowFilter{
		Name:        req.Msg.GetName(),
		Scope:       req.Msg.GetScope(),
		Project:     req.Msg.GetProject(),
		AllVersions: req.Msg.GetAllVersions(),
	})
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&millv1.ListWorkflowsResponse{Workflows: workflows}), nil
}

// PutWorkflow stores a workflow version, idempotent by content hash.
func (s *StoreService) PutWorkflow(ctx context.Context, req *connect.Request[millv1.PutWorkflowRequest]) (*connect.Response[millv1.PutWorkflowResponse], error) {
	workflow, created, err := s.svc.PutWorkflow(ctx, req.Msg.GetWorkflow())
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&millv1.PutWorkflowResponse{Workflow: workflow, Created: created}), nil
}

// PutSkill stores a skill, idempotent by digest.
func (s *StoreService) PutSkill(ctx context.Context, req *connect.Request[millv1.PutSkillRequest]) (*connect.Response[millv1.PutSkillResponse], error) {
	skill, created, err := s.svc.PutSkill(ctx, req.Msg.GetSkill())
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&millv1.PutSkillResponse{Skill: skill, Created: created}), nil
}

// GetSkill returns the skill a workflow step pins, by name and digest.
func (s *StoreService) GetSkill(ctx context.Context, req *connect.Request[millv1.GetSkillRequest]) (*connect.Response[millv1.GetSkillResponse], error) {
	skill, err := s.svc.GetSkill(ctx, req.Msg.GetName(), req.Msg.GetDigest())
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&millv1.GetSkillResponse{Skill: skill}), nil
}

// Status lists runs by state with what the ledger derives about them.
func (s *StoreService) Status(ctx context.Context, req *connect.Request[millv1.StatusRequest]) (*connect.Response[millv1.StatusResponse], error) {
	runs, err := s.svc.Status(ctx, req.Msg.GetStatuses(), req.Msg.GetProject())
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&millv1.StatusResponse{Runs: runs}), nil
}

// Inspect returns one run's every attempt in order.
func (s *StoreService) Inspect(ctx context.Context, req *connect.Request[millv1.InspectRequest]) (*connect.Response[millv1.InspectResponse], error) {
	if req.Msg.GetId() == "" && req.Msg.GetNodeRef() == "" {
		return nil, connectError(errNoTarget)
	}
	detail, err := s.svc.Inspect(ctx, domain.InspectParams{ID: req.Msg.GetId(), NodeRef: req.Msg.GetNodeRef()})
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&millv1.InspectResponse{
		Summary:  detail.Summary,
		Workflow: detail.Workflow,
		Steps:    detail.Steps,
	}), nil
}

// History returns a run's audit trail, oldest first.
func (s *StoreService) History(ctx context.Context, req *connect.Request[millv1.HistoryRequest]) (*connect.Response[millv1.HistoryResponse], error) {
	if req.Msg.GetId() == "" && req.Msg.GetNodeRef() == "" {
		return nil, connectError(errNoTarget)
	}
	events, err := s.svc.History(ctx, domain.InspectParams{ID: req.Msg.GetId(), NodeRef: req.Msg.GetNodeRef()})
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&millv1.HistoryResponse{Events: events}), nil
}

// Cost rolls a window of usage up by run, workflow, step and model.
func (s *StoreService) Cost(ctx context.Context, req *connect.Request[millv1.CostRequest]) (*connect.Response[millv1.CostResponse], error) {
	var since time.Time
	if req.Msg.GetSince() != nil {
		since = req.Msg.GetSince().AsTime()
	}
	cost, err := s.svc.Cost(ctx, domain.CostParams{Since: since, Project: req.Msg.GetProject()})
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&millv1.CostResponse{
		ByRun:      cost.ByRun,
		ByWorkflow: cost.ByWorkflow,
		ByStep:     cost.ByStep,
		ByModel:    cost.ByModel,
	}), nil
}

// Health is what the service can say about itself. It answers even when the
// database does not: an unreachable database is the answer a probe wants,
// not an error to map to a Connect code.
func (s *StoreService) Health(ctx context.Context, _ *connect.Request[millv1.HealthRequest]) (*connect.Response[millv1.HealthResponse], error) {
	health := s.svc.Health(ctx)
	return connect.NewResponse(&millv1.HealthResponse{
		Version:          version.String(),
		Database:         health.Database,
		MigrationVersion: health.Version,
		MigrationWanted:  health.Wanted,
		Detail:           health.Detail,
	}), nil
}

// callerRef is how the ledger records who asked, for the calls a principal
// that holds no lease makes. A request the interceptor did not attribute
// never reaches a handler, so the empty string is unreachable in the
// service and appears only where a spec called a handler directly.
func callerRef(ctx context.Context) string {
	caller, ok := interceptor.CallerFromContext(ctx)
	if !ok {
		return ""
	}
	return caller.String()
}
