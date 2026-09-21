package run

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/evidence"
	"github.com/ivy/mill/internal/gateway"
	"github.com/ivy/mill/internal/workspace"
)

// requireHarnesses refuses a workflow that names a harness this worker has no
// adapter for, before the first step runs.
//
// The check is at run start rather than at the step because the answer is
// known there and a run that cannot finish should not spend a session
// finding out: a workflow whose fifth step names an unknown harness would
// otherwise commit and open a pull request first. The message names the
// harness and the step, which is what a definition's author has to fix.
func (r *Runner) requireHarnesses(s *state) error {
	return r.requireHarnessesOf(s, s.workflow)
}

// requireHarnessesOf is that check over one definition: the run's workflow,
// or the assessment's, which is read before any of it runs.
func (r *Runner) requireHarnessesOf(s *state, w *millv1.Workflow) error {
	for _, step := range w.GetSteps() {
		if step.GetKind() != millv1.StepKind_STEP_KIND_SKILL {
			continue
		}
		names := make([]string, 0, len(step.GetHarness()))
		for name := range step.GetHarness() {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if _, err := s.harnesses.Lookup(name); err != nil {
				return fmt.Errorf("run %s: workflow %s step %s: %w",
					s.run.GetId(), w.GetName(), step.GetName(), err)
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

		// What the run owes its first session — provisioning's findings
		// and every preparatory step that failed — goes the way a revise's
		// feedback goes, and only until a session has begun: the ledger
		// says whether one has, so a worker that dies before the session
		// speaks delivers them again and one that dies after does not
		// (internal/run/preparatory.go).
		if step.GetKind() == millv1.StepKind_STEP_KIND_SKILL && !anySessionStarted(detail) {
			feedback = withFindings(r.findings(s, detail), feedback)
		}

		outcome, err := r.stepUnderBoundary(ctx, s, step, detail, feedback, feedbackFrom)
		if err != nil {
			return err
		}
		feedback, feedbackFrom = outcome.feedback, outcome.feedbackFrom
	}
}

// stepUnderBoundary runs one step of the walk under the workspace's step
// boundary; the attempt's outcome is what the next pass of the loop needs.
func (r *Runner) stepUnderBoundary(ctx context.Context, s *state, step *millv1.Step, detail *millv1.InspectResponse, feedback, feedbackFrom string) (outcome, error) {
	var out outcome
	err := r.underBoundary(ctx, s, step, func() (bool, error) {
		var err error
		out, err = r.step(ctx, s, step, detail, feedback, feedbackFrom)
		return out.closed, err
	})
	return out, err
}

// underBoundary runs one attempt of a step between the workspace's step
// boundary: the slice is begun before the attempt and stopped after it,
// whichever way the attempt went, so nothing the step spawned outlives it
// and an evidence-closed step leaves a candidate (docs/sandbox.md §"Step
// boundary"). Every attempt that spawns a session goes through here — the
// walk's steps and the assessment alike, because a machine refuses an exec
// under no slice. A backend with no boundary — the local one — runs the
// attempt as before.
//
// The attempt reports whether it closed on evidence, which is what EndStep
// is told. EndStep runs under a context the step's cancellation cannot
// reach, because reaping the slice is the point of it: a cancelled step is
// exactly the one whose leftovers must go. Its error does not fail the run —
// a run is not failed because a slice teardown hiccuped — but is logged,
// because the next slice was meant not to begin until this one was inactive.
func (r *Runner) underBoundary(ctx context.Context, s *state, step *millv1.Step, attempt func() (closed bool, err error)) error {
	if s.boundary == nil {
		_, err := attempt()
		return err
	}
	s.ordinal++
	ordinal := s.ordinal
	if err := s.boundary.BeginStep(ctx, ordinal); err != nil {
		return fmt.Errorf("run %s: begin step %s: %w", s.run.GetId(), step.GetName(), err)
	}
	closed, err := attempt()
	if endErr := s.boundary.EndStep(context.WithoutCancel(ctx), ordinal, closed); endErr != nil {
		r.options.Log.Warn("ending a step's slice",
			"run", s.run.GetId(), "step", step.GetName(), "error", endErr)
	}
	return err
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
	// closed reports whether the step closed on evidence. It is what
	// EndStep is told, so the step's slice leaves a promotion candidate
	// only when the step's work was verified (docs/sandbox.md §"Step
	// boundary").
	closed bool
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
	base, err := r.baseCommit(ctx, s, step, detail)
	if err != nil {
		return outcome{}, err
	}
	started, err := call(ctx, r.options.Store.StartStepRun, &millv1.StartStepRunRequest{
		LeaseId:      s.lease,
		RunId:        s.run.GetId(),
		StepName:     step.GetName(),
		Kind:         step.GetKind(),
		SessionId:    sessionID,
		WorkerRef:    r.options.WorkerRef,
		FeedbackFrom: feedbackFrom,
		BaseCommit:   base,
	})
	if err != nil {
		return outcome{}, err
	}
	sr := started.GetStepRun()
	r.printf("step %s attempt %d\n", step.GetName(), sr.GetAttempt())

	if err := r.openActivity(s, step, sr); err != nil {
		return outcome{}, err
	}

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
	r.rotateGateway(s, step, sr, token.GetStepToken())

	s.token = token.GetStepToken().GetId()
	result, execErr := r.execute(ctx, s, step, sr, detail, token.GetStepToken(), feedback)

	// What the gateway refused the attempt for is the attempt's failure,
	// whatever the session made of being refused: a step that spent its
	// budget is over, and the budget is the reason the ledger records
	// (docs/sandbox.md §Credentials).
	if over := r.gatewayRefusal(s, sr); over != nil {
		execErr = errors.Join(execErr, over)
		if result.feedback == "" {
			result.feedback = over.Error()
		}
	}

	// A gate step that filed its gate has ended the run's turn: the lease is
	// released and this attempt is deliberately left open, so that the
	// answer closes the attempt that asked rather than opening a second one
	// (docs/domain-model.md §Gate). Nothing below it may run — every one of
	// those writes is fenced by a lease this run no longer holds.
	if errors.Is(execErr, errReleased) || errors.Is(execErr, errAborted) {
		abortActivity(s)
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
		abortActivity(s)
		return outcome{}, r.stopped(ctx, s)
	}

	// What the session installed into its machine with the shim goes into
	// the run's provenance whether the attempt closes or fails: the install
	// happened either way (internal/run/preparatory.go).
	if err := r.recordInstalls(ctx, s, sr); err != nil {
		return outcome{}, err
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
	verified, verifyErr := r.verify(ctx, s, step, sr, detail, result)

	// A verdict that parsed goes on the node whichever way it went, and a
	// revise's findings become the feedback the revise target is given
	// (internal/run/verify.go). Output that did not parse leaves both
	// alone: nothing was judged.
	findings, err := r.reviewed(ctx, s, step, sr, verified)
	if err != nil {
		return outcome{}, err
	}
	if findings != "" {
		result.feedback = findings
		result.judgment = true
	}

	switch {
	case verifyErr == nil && execErr == nil:
		if err := r.closeStep(ctx, s, sr, step, verified, result); err != nil {
			return outcome{}, err
		}
		return outcome{closed: true}, nil

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
		if err := r.endActivity(ctx, s, step, sr, millv1.StepRunStatus_STEP_RUN_STATUS_ABORTED, millv1.EvidenceKind_EVIDENCE_KIND_UNSPECIFIED, pending.Error()); err != nil {
			return outcome{}, err
		}
		return outcome{}, nil

	default:
		return r.failStep(ctx, s, step, sr, detail, result, errors.Join(execErr, verifyErr))
	}
}

// rotateGateway installs the attempt in the credential gateway beside the
// workspace's own rotation: the token the gateway authenticates from now
// on, the budget and the pin the workflow declared, and the ledger write
// each response's usage becomes.
func (r *Runner) rotateGateway(s *state, step *millv1.Step, sr *millv1.StepRun, token *millv1.StepToken) {
	if r.options.Gateway == nil {
		return
	}
	// The recorder runs on the gateway's goroutines, so it carries the
	// lease it was installed under rather than reading the run's state.
	lease := s.lease
	r.options.Gateway.Rotate(s.run.GetId(), gateway.Step{
		StepRunID: sr.GetId(),
		StepName:  step.GetName(),
		Token:     workspace.TokenFromProto(token),
		Budget:    step.GetTokenBudget(),
		Models:    step.GetModels(),
		Record: func(ctx context.Context, usage gateway.Usage) error {
			return r.recordGatewayUsage(ctx, lease, sr, usage)
		},
	})
}

// gatewayRefusal reads the budget refusal an attempt earned, if any, and
// takes the attempt out of the gateway: its token is revoked in the store
// next, and the gateway refuses it from here.
func (r *Runner) gatewayRefusal(s *state, sr *millv1.StepRun) *gateway.BudgetError {
	if r.options.Gateway == nil {
		return nil
	}
	over := r.options.Gateway.Exceeded(sr.GetId())
	r.options.Gateway.Revoke(s.run.GetId())
	return over
}

// closeStep records the evidence that closes an attempt.
func (r *Runner) closeStep(ctx context.Context, s *state, sr *millv1.StepRun, step *millv1.Step, verified evidence.Evidence, res result) error {
	value := verified.Value
	value.StepRunId = sr.GetId()
	// A preparatory step's record is its evidence, output included; no
	// other kind has any (internal/run/preparatory.go).
	value.Output = res.output
	if _, err := call(ctx, r.options.Store.CloseStepRun, &millv1.CloseStepRunRequest{
		LeaseId:      s.lease,
		StepRunId:    sr.GetId(),
		Evidence:     value,
		AutoApproved: res.autoApproved,
	}); err != nil {
		return err
	}
	if err := r.endActivity(ctx, s, step, sr, millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED, value.GetKind(), evidenceName(value.GetKind())); err != nil {
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
	// The step is closed on evidence: its cache copies are what the run
	// would promote, until a later step closes (internal/run/cache.go).
	r.cacheCandidate(ctx, s, step)
	// The attempt goes in the span whether it closed or failed: what a park
	// gate has to show a human is every attempt in the span, and a step
	// that closed on evidence can still be the reason a later one could not
	// (internal/run/breaker.go).
	s.recordAttempt(step, sr, res.exit, lastWords(res, ""), false)
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
