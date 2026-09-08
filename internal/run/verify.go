package run

import (
	"context"
	"fmt"

	daggerv1 "github.com/ivy/mill/gen/dagger/v1"
	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/dagger"
	"github.com/ivy/mill/internal/evidence"
	"github.com/ivy/mill/internal/forge"
)

// verify asks the verifier for the step's declared evidence kind whether the
// fact the mill observed holds. Every seam it can consult is wired here and
// nowhere else, which is what keeps internal/evidence free of the run loop.
func (r *Runner) verify(ctx context.Context, s *state, step *millv1.Step, detail *millv1.InspectResponse, res result) (evidence.Evidence, error) {
	asked := evidence.Step{
		Name:       step.GetName(),
		Definition: step,
		Workflow:   s.workflow,
		Branch:     s.run.GetBranch(),
		Claim:      res.claim,
		Exit:       res.exit,
		Session:    res.session,
		Elapsed:    elapsed(detail, step.GetName(), r.options.Now()),
		Git:        s.workspace.GitRunner(),
		Forge:      forgeReader{ctx: ctx, runner: r},
		Artifacts:  artifacts{client: r.options.Dagger, ref: s.node.GetReference()},
		Gates:      gates{client: r.options.Dagger},
		Now:        r.options.Now,
	}
	return evidence.Verify(ctx, step.GetEvidence(), asked)
}

// forgeReader is the read half of the project's forge, resolved lazily: a
// run that never reaches a forge step never needs one configured.
type forgeReader struct {
	ctx    context.Context
	runner *Runner
}

func (f forgeReader) PR(ctx context.Context, branch string) (forge.PullRequest, error) {
	client, err := f.runner.options.Forges(ctx)
	if err != nil {
		return forge.PullRequest{}, err
	}
	return client.PR(ctx, branch)
}

func (f forgeReader) Checks(ctx context.Context, sha string) (forge.Status, error) {
	client, err := f.runner.options.Forges(ctx)
	if err != nil {
		return "", err
	}
	return client.Checks(ctx, sha)
}

// artifacts asks dagger whether it holds an artifact of a digest. `artifact`
// evidence is a digest dagger accepted, so the acceptance is dagger's to
// confirm and never the session's to claim.
type artifacts struct {
	client *dagger.Client
	ref    string
}

func (a artifacts) Accepted(ctx context.Context, want string) (bool, error) {
	held, err := a.client.Artifacts(ctx, a.ref)
	if err != nil {
		return false, err
	}
	for _, artifact := range held {
		if artifactDigest(artifact) == want {
			return true, nil
		}
	}
	return false, nil
}

// attachArtifact puts a step's output on the node as a dagger artifact and
// returns its digest, which is what the `artifact` verifier then asks dagger
// to confirm. The metadata carries the run id, as every artifact mill
// attaches does (docs/ledger.md §"What goes to dagger, and when").
func (r *Runner) attachArtifact(ctx context.Context, s *state, step *millv1.Step, body string) (string, error) {
	if body == "" {
		return "", nil
	}
	name := fmt.Sprintf("%s-%s", s.run.GetId(), step.GetName())
	if err := r.options.Dagger.AttachText(ctx,
		dagger.RequestID(s.run.GetId(), step.GetName(), attachAttempt), s.lease,
		s.node.GetReference(), name, step.GetName(), body, map[string]any{
			"run_id": s.run.GetId(),
			"step":   step.GetName(),
			"digest": digest(body),
		}); err != nil {
		return "", err
	}
	return digest(body), nil
}

// attachAttempt distinguishes the patch that attaches a step's artifact from
// the patch that completes the node. They share a run and a step name, so
// they must not share an idempotency key.
const attachAttempt = -1

// artifactDigest reads the digest mill recorded on an artifact's metadata.
func artifactDigest(artifact *daggerv1.Artifact) string {
	value, ok := artifact.GetMetadata().GetFields()["digest"]
	if !ok {
		return ""
	}
	return value.GetStringValue()
}

// failStep records a failed attempt and applies the step's on_failure: a
// revise that has retries left, or the circuit breaker.
func (r *Runner) failStep(ctx context.Context, s *state, step *millv1.Step, sr *millv1.StepRun, detail *millv1.InspectResponse, res result, cause error) (outcome, error) {
	exit := res.exit
	if exit == nil {
		exit = &harnessProcessExit
	}
	if _, err := call(ctx, r.options.Store.FailStepRun, &millv1.FailStepRunRequest{
		LeaseId:   s.lease,
		StepRunId: sr.GetId(),
		Exit:      exit,
	}); err != nil {
		return outcome{}, err
	}
	r.printf("  failed: %v\n", cause)

	target := step.GetOnFailure().GetReviseTarget()
	revising := step.GetOnFailure().GetKind() == millv1.OnFailureKind_ON_FAILURE_KIND_REVISE && target != ""
	if revising && sr.GetAttempt() < int64(step.GetRetries()) {
		// Every step from the revise target through the failing step is
		// superseded, so the revised work is re-committed, re-checked and
		// re-reviewed. The evidence those attempts produced stays and
		// closes nothing.
		if _, err := call(ctx, r.options.Store.SupersedeSteps, &millv1.SupersedeStepsRequest{
			LeaseId:     s.lease,
			RunId:       s.run.GetId(),
			FromStep:    target,
			ThroughStep: step.GetName(),
		}); err != nil {
			return outcome{}, err
		}
		feedback := res.feedback
		if feedback == "" {
			feedback = cause.Error()
		}
		r.printf("  revising from %s\n", target)
		return outcome{feedback: feedback, feedbackFrom: sr.GetId()}, nil
	}
	return outcome{}, r.park(ctx, s, step, sr, detail, res, cause)
}

// harnessProcessExit is the exit recorded for a failure with no process
// behind it: the mill could not establish the fact, and nothing ran.
var harnessProcessExit = neverRan()

// park is the circuit breaker: post nothing further, comment the last
// findings on the node, file a gate asking a human what to do, record it,
// and release with budget (docs/architecture.md §"The circuit breaker").
func (r *Runner) park(ctx context.Context, s *state, step *millv1.Step, sr *millv1.StepRun, _ *millv1.InspectResponse, res result, cause error) error {
	findings := res.feedback
	if findings == "" {
		findings = cause.Error()
	}
	if err := r.options.Dagger.Comment(ctx,
		dagger.RequestID(s.run.GetId(), step.GetName()+"-findings", sr.GetAttempt()), s.lease,
		s.node.GetReference(), findings); err != nil {
		return err
	}
	gate, err := r.options.Dagger.CreateGate(ctx,
		dagger.RequestID(s.run.GetId(), step.GetName()+"-park", sr.GetAttempt()), s.lease,
		dagger.Gate{
			Title:  fmt.Sprintf("%s: %s stopped after %d attempts", s.node.GetReference(), step.GetName(), sr.GetAttempt()),
			Body:   parkBody(s, step, findings),
			Blocks: s.node.GetReference(),
		})
	if err != nil {
		return err
	}
	if _, err := call(ctx, r.options.Store.Park, &millv1.ParkRequest{
		LeaseId:    s.lease,
		RunId:      s.run.GetId(),
		GateNodeId: gate.GetId(),
		Reason:     millv1.ParkedReason_PARKED_REASON_RETRIES,
	}); err != nil {
		return err
	}
	if err := r.options.Dagger.Release(ctx, s.lease, daggerv1.ReleaseReason_RELEASE_REASON_BUDGET, "retries exhausted on "+step.GetName()); err != nil {
		return err
	}
	r.printf("  parked on %s; %s asks a human what to do\n", step.GetName(), gate.GetReference())
	return errReleased
}

// parkBody is what the park gate asks.
func parkBody(s *state, step *millv1.Step, findings string) string {
	return fmt.Sprintf(
		"Run %s of %s stopped at step %s.\n\nWhat the last attempt said:\n\n%s\n\n"+
			"Complete this node to resume the run at that step with its attempts reset. "+
			"Drop it, or run `mill restart %s`, to start over.",
		s.run.GetId(), s.node.GetReference(), step.GetName(), findings, s.node.GetReference())
}
