package run

import (
	"context"
	"fmt"
	"strings"
	"text/template"

	daggerv1 "github.com/ivy/mill/gen/dagger/v1"
	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/dagger"
	"github.com/ivy/mill/internal/evidence"
)

// The gate branch of the loop. What is here is the shape docs/ledger.md
// §Resume and docs/architecture.md §"Gate steps" fix, with one deliberate
// hole: the `auto` predicate is a language this node does not implement.
// Only the literal `true` — quick-fix's merge gate, and the only predicate
// any workflow in the tree carries — is evaluated; anything else is filed
// for a human. The `gates` node fills the language in, and until it does a
// gate is answered by a person, which is the safe direction to be wrong in.

// runGate evaluates a gate step's auto predicate and either records an
// auto-approval or files the gate.
func (r *Runner) runGate(ctx context.Context, s *state, step *millv1.Step, sr *millv1.StepRun) (result, error) {
	if autoApproves(step.GetGate().GetAuto()) {
		r.printf("  gate %s auto-approved\n", step.GetName())
		out := result{autoApproved: true}
		claim, err := r.approve(ctx, s, step)
		if err != nil {
			return out, err
		}
		out.claim = claim
		return out, nil
	}
	return result{}, r.fileGate(ctx, s, step, sr)
}

// autoApproves reports whether a gate's predicate holds without a human.
// docs/domain-model.md §Gate defines a predicate over prior evidence and the
// run's tier; until that language exists, `true` is the whole of it.
func autoApproves(predicate string) bool {
	return strings.TrimSpace(predicate) == "true"
}

// approve is what an approved gate authorizes. A gate is a decision, and the
// workflow says what the decision is for: quick-fix's merge gate declares
// `status` evidence, which is the forge's word on the branch it just agreed
// to merge, so approving it merges. A gate declaring `gate` evidence is a
// decision about nothing but itself and authorizes no act.
func (r *Runner) approve(ctx context.Context, s *state, step *millv1.Step) (string, error) {
	if step.GetEvidence() != millv1.EvidenceKind_EVIDENCE_KIND_STATUS {
		return "", nil
	}
	f, err := r.options.Forges(ctx)
	if err != nil {
		return "", err
	}
	pr, err := f.PR(ctx, s.run.GetBranch())
	if err != nil {
		return "", err
	}
	if !pr.Merged() {
		if pr, err = f.Merge(ctx, pr); err != nil {
			return "", err
		}
	}
	sha, err := f.MergeCommit(ctx, pr)
	if err != nil {
		return "", err
	}
	s.mergeCommit = sha
	r.printf("  merged as %s\n", short(sha))
	return pr.HeadSHA, nil
}

// fileGate is the three writes, in the one order that survives a crash at
// any point: the patch that creates the gate and its blocking edge, the
// store's record of the gate id, then the release. A crash before the store
// write leaves a lease that expires harmlessly; a crash after it leaves a
// gate the store already knows about and will not file twice (Invariant 12).
func (r *Runner) fileGate(ctx context.Context, s *state, step *millv1.Step, sr *millv1.StepRun) error {
	ask, err := r.renderAsk(s, step)
	if err != nil {
		return err
	}
	gate, err := r.options.Dagger.CreateGate(ctx,
		dagger.RequestID(s.run.GetId(), step.GetName(), sr.GetAttempt()), s.lease,
		dagger.Gate{Title: ask.title, Body: ask.body, Blocks: s.node.GetReference()})
	if err != nil {
		return err
	}
	if _, err := call(ctx, r.options.Store.MarkWaiting, &millv1.MarkWaitingRequest{
		LeaseId:    s.lease,
		RunId:      s.run.GetId(),
		GateNodeId: gate.GetId(),
		StepRunId:  sr.GetId(),
	}); err != nil {
		return err
	}
	if err := r.options.Dagger.Release(ctx, s.lease, daggerv1.ReleaseReason_RELEASE_REASON_GATE, "gate "+gate.GetReference()); err != nil {
		return err
	}
	r.printf("  gate %s filed as %s; waiting on a human\n", step.GetName(), gate.GetReference())
	return errReleased
}

// resumeGate is the branch at the top of the loop: a run whose gate is
// already filed reads it rather than filing another. Done closes the step
// with `gate` evidence; a denial fails the run with the gate's deny_result.
func (r *Runner) resumeGate(ctx context.Context, s *state, step *millv1.Step, detail *millv1.InspectResponse) error {
	gate, err := r.options.Dagger.Node(ctx, s.run.GetGateNodeId())
	if err != nil {
		return err
	}
	sr := latestStepRun(detail, step.GetName())
	if sr == nil {
		return fmt.Errorf("run %s: gate %s is filed but has no attempt", s.run.GetId(), step.GetName())
	}
	if gate.GetStatus() != daggerv1.Status_STATUS_DONE {
		return r.denyGate(ctx, s, step, gate)
	}
	verified, err := evidence.Verify(ctx, millv1.EvidenceKind_EVIDENCE_KIND_GATE, evidence.Step{
		Name:       step.GetName(),
		Definition: step,
		Workflow:   s.workflow,
		Claim:      gate.GetId(),
		Gates:      gates{client: r.options.Dagger},
		Now:        r.options.Now,
	})
	if err != nil {
		return err
	}
	return r.closeStep(ctx, s, sr, step, verified, result{claim: gate.GetId()})
}

// denyGate ends a run whose gate a human did not approve. The node is
// completed with the gate's deny_result, so whoever asked for the work is
// told, and the run is failed.
func (r *Runner) denyGate(ctx context.Context, s *state, step *millv1.Step, gate *daggerv1.Node) error {
	body := step.GetGate().GetDenyResult()
	if body == "" {
		body = fmt.Sprintf("The %s gate was not approved.", step.GetName())
	}
	if err := r.options.Dagger.Complete(ctx,
		dagger.RequestID(s.run.GetId(), step.GetName(), denialAttempt), s.lease,
		s.node.GetReference(), &daggerv1.Result{Body: body}); err != nil {
		return err
	}
	if _, err := call(ctx, r.options.Store.Park, &millv1.ParkRequest{
		LeaseId:    s.lease,
		RunId:      s.run.GetId(),
		GateNodeId: gate.GetId(),
		Reason:     millv1.ParkedReason_PARKED_REASON_DENIED,
	}); err != nil {
		return err
	}
	r.printf("  gate %s was denied; %s stops here\n", step.GetName(), s.node.GetReference())
	return errReleased
}

// denialAttempt distinguishes the patch that reports a denial from the patch
// that would have completed the node with the work's own result. They are
// two different writes and must never share an idempotency key.
const denialAttempt = 0

// ask is a gate's rendered title and body.
type ask struct {
	title string
	body  string
}

// renderAsk fills the gate's templates from the run. The fields are the ones
// docs/domain-model.md §Gate names: which node, which step, and the
// artifacts the decision is about.
func (r *Runner) renderAsk(s *state, step *millv1.Step) (ask, error) {
	data := map[string]any{
		"NodeRef":     s.node.GetReference(),
		"NodeTitle":   s.node.GetTitle(),
		"Step":        step.GetName(),
		"Branch":      s.run.GetBranch(),
		"PullRequest": s.pullRequestURL,
		"RunID":       s.run.GetId(),
	}
	title, err := render(step.GetGate().GetAsk().GetTitle(), data)
	if err != nil {
		return ask{}, err
	}
	body, err := render(step.GetGate().GetAsk().GetBody(), data)
	if err != nil {
		return ask{}, err
	}
	return ask{title: title, body: body}, nil
}

// render expands one gate template.
func render(text string, data map[string]any) (string, error) {
	t, err := template.New("ask").Option("missingkey=zero").Parse(text)
	if err != nil {
		return "", fmt.Errorf("gate ask: %w", err)
	}
	var b strings.Builder
	if err := t.Execute(&b, data); err != nil {
		return "", fmt.Errorf("gate ask: %w", err)
	}
	return b.String(), nil
}

// gates adapts the dagger client to the `gate` verifier's seam.
type gates struct{ client *dagger.Client }

func (g gates) Node(ctx context.Context, id string) (evidence.GateNode, error) {
	node, err := g.client.Node(ctx, id)
	if err != nil {
		return evidence.GateNode{}, err
	}
	return evidence.GateNode{
		ID:     node.GetId(),
		Status: daggerStatus(node.GetStatus()),
	}, nil
}

// daggerStatus is a node status as the `gate` verifier reads it.
func daggerStatus(status daggerv1.Status) string {
	switch status {
	case daggerv1.Status_STATUS_OPEN:
		return "open"
	case daggerv1.Status_STATUS_DONE:
		return "done"
	case daggerv1.Status_STATUS_DROPPED:
		return "dropped"
	default:
		return ""
	}
}

// latestStepRun is the newest attempt at a step in an inspect response.
func latestStepRun(detail *millv1.InspectResponse, name string) *millv1.StepRun {
	var latest *millv1.StepRun
	for _, d := range detail.GetSteps() {
		sr := d.GetStepRun()
		if sr.GetStepName() != name {
			continue
		}
		if latest == nil || sr.GetAttempt() > latest.GetAttempt() {
			latest = sr
		}
	}
	return latest
}
