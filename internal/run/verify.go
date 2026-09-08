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
		Gates:      gates{runner: r, state: s},
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
func (r *Runner) attachArtifact(ctx context.Context, s *state, step *millv1.Step, sr *millv1.StepRun, body string) (string, error) {
	if body == "" {
		return "", nil
	}
	// One artifact per attempt, named for it. Dagger refuses a second
	// artifact by a name a node already carries, and a revise means a step
	// runs again and produces another: the pass that was superseded keeps
	// its own, which is the record of what the run actually did.
	name := fmt.Sprintf("%s-%s-%d", s.run.GetId(), step.GetName(), sr.GetAttempt())
	if err := r.options.Dagger.AttachText(ctx,
		dagger.RequestID(s.run.GetId(), attachStep(step.GetName()), sr.GetAttempt()), s.lease,
		s.node.GetReference(), name, step.GetName(), body, map[string]any{
			"run_id": s.run.GetId(),
			"step":   step.GetName(),
			"digest": digest(body),
		}); err != nil {
		return "", err
	}
	return digest(body), nil
}

// attachStep names the attaching patch apart from every other patch of the
// same run, and the attempt number keeps two attempts at the same step
// apart. Dagger compares a replayed request id against the whole request,
// the lease included, so a second attempt attaching under the first's key is
// refused as the same key with different content — which is precisely what a
// revise does.
func attachStep(step string) string { return step + "-artifact" }

// artifactDigest reads the digest mill recorded on an artifact's metadata.
func artifactDigest(artifact *daggerv1.Artifact) string {
	value, ok := artifact.GetMetadata().GetFields()["digest"]
	if !ok {
		return ""
	}
	return value.GetStringValue()
}
