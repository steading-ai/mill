package run

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/workspace"
)

// How a run gets its workflow. docs/domain-model.md §Selection names two
// rules, in order:
//
//  1. A project default — a workflow a person pinned this project to, and
//     the tier it stands for. Used when set.
//  2. Assessment — otherwise the `/build-workflow` skill runs as the first
//     step of the run, non-interactively, and emits
//     `{workflow, tier, signals, reasoning}` as structured output. The named
//     workflow must exist in scope, and the output is the selection's
//     evidence.
//
// There is no third rule and no node-level selection: dagger's nodes carry
// no workflow and mill writes nothing of its own onto one. Filing a ticket
// asks for an outcome; sizing it is the mill's job.
//
// Either rule ends the same way: one `select` step run, closed with the
// choice as structured evidence and with the run's workflow and tier. That
// attempt is the only write that moves a run out of `selecting`
// (docs/ledger.md §runs), so the ledger always says which workflow was
// chosen and on what basis rather than a run silently acquiring one.

// selectionStep is the name of the step run that records which workflow a
// run follows.
const selectionStep = "select"

const (
	// AssessmentWorkflow is the workflow that chooses a workflow. Every
	// project can select it, and its one step is what a run with no project
	// default runs as step 0. It is exported because `mill run --dry-run`
	// names it when it says a run would assess.
	AssessmentWorkflow = "build-workflow"
	// AssessmentStep is that workflow's one step.
	AssessmentStep = "assess"
)

// The basis recorded on the selection's evidence: which of the two rules
// answered.
const (
	basisDefault    = "project default"
	basisAssessment = "assessment"
)

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
	if name := strings.TrimSpace(r.options.Workflow); name != "" {
		return r.selectDefault(ctx, s, name)
	}
	return r.selectAssessed(ctx, s)
}

// selectDefault is the first rule: the workflow a person pinned the project
// to, at the tier they said it stands for.
func (r *Runner) selectDefault(ctx context.Context, s *state, name string) error {
	workflow, err := r.workflowInScope(ctx, s, name)
	if err != nil {
		return err
	}
	if workflow == nil {
		return fmt.Errorf("run %s: no workflow named %q", s.run.GetId(), name)
	}
	sr, err := r.startSelection(ctx, s)
	if err != nil {
		return err
	}
	return r.closeSelection(ctx, s, sr, workflow, r.options.Tier, basisDefault, nil)
}

// selectAssessed is the second rule: no project default, so the run sizes
// the node for itself before it walks anything.
//
// The order matters. The assessment runs first and closes on its own
// structured evidence; only then is a `select` attempt opened, so that a
// workflow the assessment named but the store does not have fails *select*
// — with the name and the scope in the message — and the run walks nothing.
func (r *Runner) selectAssessed(ctx context.Context, s *state) error {
	candidates, err := r.candidates(ctx, s)
	if err != nil {
		return err
	}
	if len(candidates) == 0 {
		return fmt.Errorf("run %s: there is no workflow %s could choose between", s.run.GetId(), scopeOf(s))
	}
	step, err := r.assessmentDefinition(ctx, s)
	if err != nil {
		return err
	}
	output, err := r.assess(ctx, s, step, candidates)
	if err != nil {
		return err
	}

	sr, err := r.startSelection(ctx, s)
	if err != nil {
		return err
	}
	named, _ := output["workflow"].(string)
	tier, err := tierOf(word(output["tier"]))
	if err != nil {
		return r.failSelection(ctx, s, sr, fmt.Errorf("run %s: the assessment named %w", s.run.GetId(), err))
	}
	workflow, err := r.workflowInScope(ctx, s, named)
	if err != nil {
		return err
	}
	if workflow == nil {
		return r.failSelection(ctx, s, sr, fmt.Errorf(
			"run %s: the assessment named workflow %q, which is not in scope: %s has %s",
			s.run.GetId(), named, scopeOf(s), strings.Join(workflowNames(candidates), ", ")))
	}
	return r.closeSelection(ctx, s, sr, workflow, tier, basisAssessment, output)
}

// assess runs the assessment step inside the run, as step 0, and returns the
// structured output it closed on.
//
// It is an ordinary skill step in everything but where it comes from: its
// own session, its own token and capabilities, its own usage on the ledger
// and in dagger, and evidence the verifier proved against the step's schema.
// What it is not is part of the run's workflow — there is no workflow yet —
// so it runs here rather than through the walk, and its retries are spent
// here rather than by the circuit breaker.
func (r *Runner) assess(ctx context.Context, s *state, step *millv1.Step, candidates []*millv1.Workflow) (map[string]any, error) {
	brief := candidateBrief(candidates)
	var feedback, feedbackFrom string
	for {
		detail, err := call(ctx, r.options.Store.Inspect, &millv1.InspectRequest{Id: s.run.GetId()})
		if err != nil {
			return nil, err
		}
		base, err := r.head(ctx, s)
		if err != nil {
			return nil, err
		}
		started, err := call(ctx, r.options.Store.StartStepRun, &millv1.StartStepRunRequest{
			LeaseId:      s.lease,
			RunId:        s.run.GetId(),
			StepName:     step.GetName(),
			Kind:         step.GetKind(),
			SessionId:    SessionID(s.run.GetContextId(), s.run.GetGeneration(), step.GetSession()),
			WorkerRef:    r.options.WorkerRef,
			FeedbackFrom: feedbackFrom,
			BaseCommit:   base,
		})
		if err != nil {
			return nil, err
		}
		sr := started.GetStepRun()
		r.printf("step %s attempt %d\n", step.GetName(), sr.GetAttempt())

		token, err := call(ctx, r.options.Store.IssueStepToken, &millv1.IssueStepTokenRequest{
			LeaseId:      s.lease,
			StepRunId:    sr.GetId(),
			Capabilities: step.GetCapabilities(),
		})
		if err != nil {
			return nil, err
		}
		if err := s.workspace.Rotate(ctx, workspace.TokenFromProto(token.GetStepToken())); err != nil {
			return nil, fmt.Errorf("run %s: rotate the workspace token: %w", s.run.GetId(), err)
		}
		r.rotateGateway(s, step, sr, token.GetStepToken())
		s.token = token.GetStepToken().GetId()

		// The attempt runs under the workspace's step boundary like every
		// step of the walk, and its token is the gateway's bearer for as
		// long as it runs: a machine refuses an exec under no slice, the
		// assessing session is spawned through the workspace, and the
		// model it reaches is behind the gateway (docs/sandbox.md §Exec,
		// §Credentials).
		var (
			assessment map[string]any
			retry      bool
		)
		err = r.underBoundary(ctx, s, step, func() (bool, error) {
			stepCtx, cancel := stepContext(ctx, step)
			res, execErr := r.runSkill(stepCtx, s, step, sr, detail, token.GetStepToken(), feedback, brief)
			cancel()
			if over := r.gatewayRefusal(s, sr); over != nil {
				execErr = errors.Join(execErr, over)
			}
			if ctx.Err() != nil || isFenced(execErr) {
				return false, r.stopped(ctx, s)
			}

			if err := r.recordUsage(ctx, s, sr, res.usage); err != nil {
				return false, err
			}
			if _, err := call(ctx, r.options.Store.RevokeStepToken, &millv1.RevokeStepTokenRequest{
				LeaseId:     s.lease,
				StepTokenId: token.GetStepToken().GetId(),
			}); err != nil {
				return false, err
			}
			if err := r.postUsage(ctx, s, sr); err != nil {
				return false, err
			}

			verified, verifyErr := r.verify(ctx, s, step, sr, detail, res)
			if cause := errors.Join(execErr, verifyErr); cause != nil {
				exit := res.exit
				if exit == nil {
					exit = &harnessProcessExit
				}
				if _, err := call(ctx, r.options.Store.FailStepRun, &millv1.FailStepRunRequest{
					LeaseId:   s.lease,
					StepRunId: sr.GetId(),
					Exit:      exit,
				}); err != nil {
					return false, err
				}
				s.record(step.GetName(), cause.Error())
				s.recordAttempt(step, sr, exit, lastWords(res, cause.Error()), true)
				r.printf("  failed: %v\n", cause)
				if sr.GetAttempt() > int64(step.GetRetries()) {
					return false, fmt.Errorf("run %s: the assessment did not close after %d attempts: %w",
						s.run.GetId(), sr.GetAttempt(), cause)
				}
				feedback, feedbackFrom = cause.Error(), sr.GetId()
				retry = true
				return false, nil
			}

			value := verified.Value
			value.StepRunId = sr.GetId()
			if _, err := call(ctx, r.options.Store.CloseStepRun, &millv1.CloseStepRunRequest{
				LeaseId:   s.lease,
				StepRunId: sr.GetId(),
				Evidence:  value,
			}); err != nil {
				return false, err
			}
			s.recordAttempt(step, sr, res.exit, lastWords(res, ""), false)
			r.printf("  closed on %s evidence\n", evidenceName(value.GetKind()))
			assessment = value.GetStructured().AsMap()
			return true, nil
		})
		if err != nil {
			return nil, err
		}
		if retry {
			continue
		}
		return assessment, nil
	}
}

// assessmentDefinition is the step the assessment runs: `build-workflow`'s
// `assess`, resolved in the project's scope like any other workflow. A
// worker whose store has never been pushed it says so here, before a session
// is spent.
func (r *Runner) assessmentDefinition(ctx context.Context, s *state) (*millv1.Step, error) {
	workflow, err := r.workflowInScope(ctx, s, AssessmentWorkflow)
	if err != nil {
		return nil, err
	}
	if workflow == nil {
		return nil, fmt.Errorf("run %s: %s has no project default and no workflow named %q to assess with",
			s.run.GetId(), scopeOf(s), AssessmentWorkflow)
	}
	step := stepNamed(workflow, AssessmentStep)
	if step == nil {
		return nil, fmt.Errorf("run %s: workflow %s has no step %q", s.run.GetId(), AssessmentWorkflow, AssessmentStep)
	}
	if err := r.requireHarnessesOf(s, workflow); err != nil {
		return nil, err
	}
	return step, nil
}

// candidates is what the assessment may choose between: every workflow in
// the project's scope but the assessment's own. A workflow that chose itself
// would be a run that never got past selecting.
func (r *Runner) candidates(ctx context.Context, s *state) ([]*millv1.Workflow, error) {
	// Listed unfiltered and narrowed here: the store's filter is exact —
	// naming a project excludes the org definitions rather than falling
	// back to them — and what a run may select is both scopes, project
	// first (docs/domain-model.md §Selection).
	res, err := call(ctx, r.options.Store.ListWorkflows, &millv1.ListWorkflowsRequest{})
	if err != nil {
		return nil, err
	}
	return InScope(res.GetWorkflows(), projectOf(s.run.GetNodeRef())), nil
}

// InScope is every workflow a run on a project may be assessed into: the
// newest version of each name visible to that project, less the
// assessment's own, in name order. A project definition shadows an org one,
// which is the precedence GetWorkflow resolves a single name by.
//
// It is exported because `mill run --dry-run` prints the same list the
// assessing session is given, so what an operator is shown is what the model
// will be asked to choose between.
func InScope(workflows []*millv1.Workflow, project string) []*millv1.Workflow {
	newest := map[string]*millv1.Workflow{}
	for _, workflow := range workflows {
		if workflow.GetName() == AssessmentWorkflow {
			continue
		}
		switch workflow.GetScope() {
		case millv1.Scope_SCOPE_PROJECT:
			if project == "" || workflow.GetProject() != project {
				continue
			}
		case millv1.Scope_SCOPE_ORG:
			// Visible everywhere, and shadowed by a project definition.
			if held, ok := newest[workflow.GetName()]; ok && held.GetScope() == millv1.Scope_SCOPE_PROJECT {
				continue
			}
		default:
			continue
		}
		newest[workflow.GetName()] = workflow
	}
	out := make([]*millv1.Workflow, 0, len(newest))
	for _, workflow := range newest {
		out = append(out, workflow)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetName() < out[j].GetName() })
	return out
}

// candidateBrief is the mill's own words to the assessing session, naming
// what it may choose between.
//
// The skill cannot carry the list: a skill is the portable part and the
// workflows in scope are the store's, so the brief is rendered per run in
// the same way an artifact's is (internal/run/artifact.go). It is also what
// makes "the named workflow must exist in scope" a rule the session was told
// about before it is held to it.
func candidateBrief(candidates []*millv1.Workflow) string {
	var b strings.Builder
	b.WriteString("## The workflows in scope\n\n")
	b.WriteString("These are the workflows this run may be assessed into, read from the mill's " +
		"store. Name exactly one of them, spelled as it appears here; any other name fails the " +
		"selection step and the run walks nothing.\n\n")
	for _, workflow := range candidates {
		fmt.Fprintf(&b, "- `%s`\n", workflow.GetName())
	}
	return b.String()
}

// workflowInScope resolves a workflow by name for this run's project, or nil
// when nothing of that name is in scope. A name that does not resolve is an
// answer here rather than a failure: it is what the caller is asking about,
// and the caller says what an absent workflow means.
func (r *Runner) workflowInScope(ctx context.Context, s *state, name string) (*millv1.Workflow, error) {
	// The scope is left unspecified on purpose: that is what asks the
	// store to resolve project over org, which is the precedence
	// docs/domain-model.md §Selection states.
	res, err := call(ctx, r.options.Store.GetWorkflow, &millv1.GetWorkflowRequest{
		Name:    name,
		Project: projectOf(s.run.GetNodeRef()),
	})
	if isNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return res.GetWorkflow(), nil
}

// isNotFound reports whether the store answered that nothing of that name
// exists, which the server maps to NOT_FOUND.
func isNotFound(err error) bool {
	var connectErr *connect.Error
	return errors.As(err, &connectErr) && connectErr.Code() == connect.CodeNotFound
}

// startSelection opens the `select` attempt.
func (r *Runner) startSelection(ctx context.Context, s *state) (*millv1.StepRun, error) {
	base, err := r.head(ctx, s)
	if err != nil {
		return nil, err
	}
	res, err := call(ctx, r.options.Store.StartStepRun, &millv1.StartStepRunRequest{
		LeaseId:    s.lease,
		RunId:      s.run.GetId(),
		StepName:   selectionStep,
		Kind:       millv1.StepKind_STEP_KIND_SKILL,
		WorkerRef:  r.options.WorkerRef,
		BaseCommit: base,
	})
	if err != nil {
		return nil, err
	}
	return res.GetStepRun(), nil
}

// closeSelection records the choice: the workflow on the run, the tier gate
// predicates will compare against, and the whole of what decided it as
// structured evidence.
func (r *Runner) closeSelection(ctx context.Context, s *state, sr *millv1.StepRun,
	workflow *millv1.Workflow, tier millv1.Tier, basis string, assessed map[string]any,
) error {
	value := map[string]any{}
	// The assessment's own output first, so its signals and reasoning are
	// on the evidence; then the four fields the mill states for itself,
	// which are the record whichever rule answered.
	for key, item := range assessed {
		value[key] = item
	}
	value["workflow"] = workflow.GetName()
	value["version"] = workflow.GetVersion()
	value["tier"] = tierName(tier)
	value["basis"] = basis

	chosen, err := structpb.NewStruct(value)
	if err != nil {
		return err
	}
	if _, err := call(ctx, r.options.Store.CloseStepRun, &millv1.CloseStepRunRequest{
		LeaseId:    s.lease,
		StepRunId:  sr.GetId(),
		WorkflowId: workflow.GetId(),
		Tier:       tier,
		Evidence: &millv1.Evidence{
			StepRunId:  sr.GetId(),
			Kind:       millv1.EvidenceKind_EVIDENCE_KIND_STRUCTURED,
			Value:      &millv1.Evidence_Structured{Structured: chosen},
			VerifiedBy: "mill: " + basis,
		},
	}); err != nil {
		return err
	}
	s.workflow = workflow
	r.printf("workflow %s@%s selected by %s\n", workflow.GetName(), short(workflow.GetVersion()), basis)
	return nil
}

// failSelection fails the `select` attempt and reports why. The run stops
// here: there is no workflow, so there is nothing to walk and no step whose
// retries could be spent on trying again.
func (r *Runner) failSelection(ctx context.Context, s *state, sr *millv1.StepRun, cause error) error {
	if _, err := call(ctx, r.options.Store.FailStepRun, &millv1.FailStepRunRequest{
		LeaseId:   s.lease,
		StepRunId: sr.GetId(),
		Exit:      &harnessProcessExit,
	}); err != nil {
		return err
	}
	r.printf("  failed: %v\n", cause)
	return cause
}

// scopeOf names the scope a workflow is resolved in, for the message a
// person reads when a name did not resolve in it.
func scopeOf(s *state) string {
	project := projectOf(s.run.GetNodeRef())
	if project == "" {
		return "the org scope"
	}
	return fmt.Sprintf("project %s (and the org scope)", project)
}

// workflowNames is the candidates as the message lists them.
func workflowNames(candidates []*millv1.Workflow) []string {
	names := make([]string, 0, len(candidates))
	for _, workflow := range candidates {
		names = append(names, workflow.GetName())
	}
	return names
}

// tierOf is a tier as the assessment spells it. The schema already refuses
// anything else, so a failure here is the mill refusing to guess rather than
// a case a session can reach.
func tierOf(name string) (millv1.Tier, error) {
	tier := millv1.Tier(millv1.Tier_value["TIER_"+strings.ToUpper(strings.ReplaceAll(name, "-", "_"))])
	if tier == millv1.Tier_TIER_UNSPECIFIED {
		return tier, fmt.Errorf("tier %q, which is not a tier: quick-fix, small, feature or large", name)
	}
	return tier, nil
}

// word is a structured output's field as a string, or empty.
func word(value any) string {
	s, _ := value.(string)
	return s
}
