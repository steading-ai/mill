package run

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/evidence"
	"github.com/ivy/mill/internal/workspace"
)

// selectionStep is the name of the step run that records which workflow a
// run follows.
//
// A run is created `selecting`, and the only write that moves it out is a
// step run closed with a workflow id (docs/ledger.md §runs, and the store's
// SetRunWorkflow). The assessment step that would ordinarily do that does
// not exist yet — mill#10 left it to the router — so O0 records the project
// default here: one closed attempt, with the choice as structured evidence,
// so the ledger says which workflow was selected and why rather than a run
// silently acquiring one.
const selectionStep = "select"

// selectWorkflow resolves the run's workflow, recording the selection when
// the run has none yet.
func (r *Runner) selectWorkflow(ctx context.Context, s *state) error {
	if id := s.run.GetWorkflowId(); id != "" {
		res, err := call(ctx, r.options.Store.GetWorkflow, &millv1.GetWorkflowRequest{Id: id})
		if err != nil {
			return err
		}
		s.workflow = res.GetWorkflow()
		return nil
	}

	// The scope is left unspecified on purpose: that is what asks the
	// store to resolve project over org, which is the precedence
	// docs/domain-model.md §"Workflow selection" states.
	res, err := call(ctx, r.options.Store.GetWorkflow, &millv1.GetWorkflowRequest{
		Name:    r.options.Workflow,
		Project: projectOf(s.run.GetNodeRef()),
	})
	if err != nil {
		return err
	}
	workflow := res.GetWorkflow()
	if workflow == nil {
		return fmt.Errorf("run %s: no workflow named %q", s.run.GetId(), r.options.Workflow)
	}

	sr, err := call(ctx, r.options.Store.StartStepRun, &millv1.StartStepRunRequest{
		LeaseId:   s.lease,
		RunId:     s.run.GetId(),
		StepName:  selectionStep,
		Kind:      millv1.StepKind_STEP_KIND_SKILL,
		WorkerRef: r.options.WorkerRef,
	})
	if err != nil {
		return err
	}
	chosen, err := structpb.NewStruct(map[string]any{
		"workflow": workflow.GetName(),
		"version":  workflow.GetVersion(),
		"tier":     tierName(r.options.Tier),
		"basis":    "project default",
	})
	if err != nil {
		return err
	}
	if _, err := call(ctx, r.options.Store.CloseStepRun, &millv1.CloseStepRunRequest{
		LeaseId:    s.lease,
		StepRunId:  sr.GetStepRun().GetId(),
		WorkflowId: workflow.GetId(),
		Tier:       r.options.Tier,
		Evidence: &millv1.Evidence{
			StepRunId:  sr.GetStepRun().GetId(),
			Kind:       millv1.EvidenceKind_EVIDENCE_KIND_STRUCTURED,
			Value:      &millv1.Evidence_Structured{Structured: chosen},
			VerifiedBy: "mill: project default",
		},
	}); err != nil {
		return err
	}
	s.workflow = workflow
	r.printf("workflow %s@%s selected\n", workflow.GetName(), short(workflow.GetVersion()))
	return nil
}

// requireHarnesses refuses a workflow that names a harness this worker has no
// adapter for, before the first step runs.
//
// The check is at run start rather than at the step because the answer is
// known there and a run that cannot finish should not spend a session
// finding out: a workflow whose fifth step names an unknown harness would
// otherwise commit and open a pull request first. The message names the
// harness and the step, which is what a definition's author has to fix.
func (r *Runner) requireHarnesses(s *state) error {
	for _, step := range s.workflow.GetSteps() {
		if step.GetKind() != millv1.StepKind_STEP_KIND_SKILL {
			continue
		}
		names := make([]string, 0, len(step.GetHarness()))
		for name := range step.GetHarness() {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if _, err := r.options.Harnesses.Lookup(name); err != nil {
				return fmt.Errorf("run %s: workflow %s step %s: %w",
					s.run.GetId(), s.workflow.GetName(), step.GetName(), err)
			}
		}
	}
	return nil
}

// walk runs each open step in order, starting at the run's current step. The
// step to run is derived from the ledger on every pass, never carried, so a
// revise that moves the current step backwards moves this loop with it
// (Invariant 10).
func (r *Runner) walk(ctx context.Context, s *state) error {
	var feedback string
	var feedbackFrom string
	for {
		if err := ctx.Err(); err != nil {
			return r.stopped(ctx, s)
		}
		detail, err := call(ctx, r.options.Store.Inspect, &millv1.InspectRequest{Id: s.run.GetId()})
		if err != nil {
			return err
		}
		s.run = detail.GetSummary().GetRun()
		name := detail.GetSummary().GetCurrentStep()
		if name == "" {
			return r.finish(ctx, s, detail)
		}
		step := stepNamed(s.workflow, name)
		if step == nil {
			return fmt.Errorf("run %s: the workflow has no step %q", s.run.GetId(), name)
		}

		outcome, err := r.step(ctx, s, step, detail, feedback, feedbackFrom)
		if err != nil {
			return err
		}
		feedback, feedbackFrom = outcome.feedback, outcome.feedbackFrom
	}
}

// stopped is what a cancelled step context means: the heartbeat stopped it,
// and why decides what is written. A fence writes nothing at all.
func (r *Runner) stopped(ctx context.Context, s *state) error {
	err := s.beat.err()
	switch {
	case errors.Is(err, ErrFenced):
		r.printf("fenced out of %s; abandoning the run locally\n", s.node.GetReference())
		return ErrFenced
	case errors.Is(err, errCancelled):
		return r.cancelStep(context.WithoutCancel(ctx), s)
	case errors.Is(err, errShutdown):
		return r.shutdownStep(context.WithoutCancel(ctx), s)
	case err != nil:
		return err
	default:
		return context.Canceled
	}
}

// outcome is what one step left behind for the next pass of the loop.
type outcome struct {
	// feedback is a check's stderr or a reviewer's findings, which the
	// revise target receives as its next prompt.
	feedback string
	// feedbackFrom is the attempt whose failure the next one answers.
	feedbackFrom string
}

// step is one attempt: token, rotate, execute by kind, verify, record, post
// usage, revoke. The order is docs/ledger.md §Resume's.
func (r *Runner) step(ctx context.Context, s *state, step *millv1.Step, detail *millv1.InspectResponse, feedback, feedbackFrom string) (outcome, error) {
	// The gate branch sits at the top of the loop: a run resuming with a
	// gate already filed reads it instead of filing another (Invariant 12).
	if step.GetKind() == millv1.StepKind_STEP_KIND_GATE && s.run.GetGateNodeId() != "" {
		return outcome{}, r.resumeGate(ctx, s, step, detail)
	}

	sessionID := ""
	if step.GetSession() != "" {
		sessionID = SessionID(s.run.GetContextId(), s.run.GetGeneration(), step.GetSession())
	}
	started, err := call(ctx, r.options.Store.StartStepRun, &millv1.StartStepRunRequest{
		LeaseId:      s.lease,
		RunId:        s.run.GetId(),
		StepName:     step.GetName(),
		Kind:         step.GetKind(),
		SessionId:    sessionID,
		WorkerRef:    r.options.WorkerRef,
		FeedbackFrom: feedbackFrom,
	})
	if err != nil {
		return outcome{}, err
	}
	sr := started.GetStepRun()
	r.printf("step %s attempt %d\n", step.GetName(), sr.GetAttempt())

	token, err := call(ctx, r.options.Store.IssueStepToken, &millv1.IssueStepTokenRequest{
		LeaseId:      s.lease,
		StepRunId:    sr.GetId(),
		Capabilities: step.GetCapabilities(),
	})
	if err != nil {
		return outcome{}, err
	}
	if err := s.workspace.Rotate(ctx, workspace.TokenFromProto(token.GetStepToken())); err != nil {
		return outcome{}, fmt.Errorf("run %s: rotate the workspace token: %w", s.run.GetId(), err)
	}

	s.token = token.GetStepToken().GetId()
	result, execErr := r.execute(ctx, s, step, sr, detail, token.GetStepToken(), feedback)

	// A gate step that filed its gate has ended the run's turn: the lease is
	// released and this attempt is deliberately left open, so that the
	// answer closes the attempt that asked rather than opening a second one
	// (docs/domain-model.md §Gate). Nothing below it may run — every one of
	// those writes is fenced by a lease this run no longer holds.
	if errors.Is(execErr, errReleased) || errors.Is(execErr, errAborted) {
		return outcome{}, execErr
	}
	// The gate was answered on the spot. The attempt stays open and the
	// loop goes round: the next pass finds the gate recorded on the run and
	// takes the resume branch, which closes this very attempt on the
	// answer.
	if errors.Is(execErr, errAnswered) {
		return outcome{}, nil
	}

	// A step stopped by the heartbeat is the end of this worker's run, and
	// what happens next is not this loop's to decide: a fence writes
	// nothing at all, a cancellation aborts the attempt and gives the lease
	// back. Either way nothing below runs, because everything below is a
	// write under a lease this worker may no longer hold.
	if ctx.Err() != nil || isFenced(execErr) {
		return outcome{}, r.stopped(ctx, s)
	}

	// Usage is written to the ledger and posted to dagger whether the
	// attempt closes or fails, once per model, before anything releases the
	// lease that ReportUsage needs.
	if err := r.recordUsage(ctx, s, sr, result.usage); err != nil {
		return outcome{}, err
	}
	if _, err := call(ctx, r.options.Store.RevokeStepToken, &millv1.RevokeStepTokenRequest{
		LeaseId:     s.lease,
		StepTokenId: token.GetStepToken().GetId(),
	}); err != nil {
		return outcome{}, err
	}
	if err := r.postUsage(ctx, s, sr); err != nil {
		return outcome{}, err
	}
	verified, verifyErr := r.verify(ctx, s, step, detail, result)
	switch {
	case verifyErr == nil && execErr == nil:
		if err := r.closeStep(ctx, s, sr, step, verified, result); err != nil {
			return outcome{}, err
		}
		return outcome{}, nil

	case evidence.Pending(verifyErr):
		// "Not yet" is not a failure: the attempt stays open until the fact
		// settles or the step's cap is reached, which the verifier decides.
		var pending *evidence.PendingError
		_ = errors.As(verifyErr, &pending)
		select {
		case <-ctx.Done():
			return outcome{}, r.stopped(ctx, s)
		case <-time.After(pending.RetryAfter):
		}
		if _, err := call(ctx, r.options.Store.AbortOpenStepRuns, &millv1.AbortOpenStepRunsRequest{
			LeaseId: s.lease,
			RunId:   s.run.GetId(),
			Reason:  pending.Error(),
		}); err != nil {
			return outcome{}, err
		}
		return outcome{}, nil

	default:
		return r.failStep(ctx, s, step, sr, detail, result, errors.Join(execErr, verifyErr))
	}
}

// closeStep records the evidence that closes an attempt.
func (r *Runner) closeStep(ctx context.Context, s *state, sr *millv1.StepRun, step *millv1.Step, verified evidence.Evidence, res result) error {
	value := verified.Value
	value.StepRunId = sr.GetId()
	if _, err := call(ctx, r.options.Store.CloseStepRun, &millv1.CloseStepRunRequest{
		LeaseId:      s.lease,
		StepRunId:    sr.GetId(),
		Evidence:     value,
		AutoApproved: res.autoApproved,
	}); err != nil {
		return err
	}
	// What the run carries forward to its result: the pull request a `url`
	// step proved, and the implementer's last word.
	if value.GetKind() == millv1.EvidenceKind_EVIDENCE_KIND_URL {
		s.pullRequestURL = value.GetUrl()
	}
	if message := res.session.FinalMessage; message != "" {
		s.summary = message
	}
	for _, warning := range verified.Warnings {
		r.options.Log.Warn("evidence", "step", step.GetName(), "warning", warning)
	}
	r.printf("  closed on %s evidence\n", evidenceName(value.GetKind()))
	return nil
}

// tierName is a tier as the project default names it, which is the word a
// gate predicate compares against.
func tierName(tier millv1.Tier) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(tier.String(), "TIER_"), "_", "-"))
}

// projectOf is the project half of a node reference such as `mill#12`.
func projectOf(nodeRef string) string {
	for i, r := range nodeRef {
		if r == '#' {
			return nodeRef[:i]
		}
	}
	return ""
}

// stepNamed finds a step in a workflow definition.
func stepNamed(workflow *millv1.Workflow, name string) *millv1.Step {
	for _, step := range workflow.GetSteps() {
		if step.GetName() == name {
			return step
		}
	}
	return nil
}

// short is a content hash, abbreviated for a progress line.
func short(version string) string {
	if len(version) > 12 {
		return version[:12]
	}
	return version
}

// evidenceName is an evidence kind without its enum prefix.
func evidenceName(kind millv1.EvidenceKind) string {
	switch kind {
	case millv1.EvidenceKind_EVIDENCE_KIND_COMMIT:
		return "commit"
	case millv1.EvidenceKind_EVIDENCE_KIND_EXIT:
		return "exit"
	case millv1.EvidenceKind_EVIDENCE_KIND_VERDICT:
		return "verdict"
	case millv1.EvidenceKind_EVIDENCE_KIND_STRUCTURED:
		return "structured"
	case millv1.EvidenceKind_EVIDENCE_KIND_URL:
		return "url"
	case millv1.EvidenceKind_EVIDENCE_KIND_STATUS:
		return "status"
	case millv1.EvidenceKind_EVIDENCE_KIND_ARTIFACT:
		return "artifact"
	case millv1.EvidenceKind_EVIDENCE_KIND_GATE:
		return "gate"
	default:
		return "unspecified"
	}
}
