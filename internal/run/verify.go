package run

import (
	"context"
	"fmt"
	"strings"

	daggerv1 "github.com/ivy/mill/gen/dagger/v1"
	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/dagger"
	"github.com/ivy/mill/internal/evidence"
)

// verify asks the verifier for the step's declared evidence kind whether the
// fact the mill observed holds. Every seam it can consult is wired here and
// nowhere else, which is what keeps internal/evidence free of the run loop.
func (r *Runner) verify(ctx context.Context, s *state, step *millv1.Step, sr *millv1.StepRun, detail *millv1.InspectResponse, res result) (evidence.Evidence, error) {
	asked := evidence.Step{
		Name:         step.GetName(),
		Definition:   step,
		Workflow:     s.workflow,
		Branch:       s.run.GetBranch(),
		Claim:        res.claim,
		AutoApproved: autoApprovedBy(step, res),
		Base:         sr.GetBaseCommit(),
		EarliestBase: earliestBase(step, detail),
		Committer:    committer(s.workspace),
		Exit:         res.exit,
		Session:      res.session,
		Elapsed:      elapsed(detail, step.GetName(), r.options.Now()),
		Git:          s.workspace.GitRunner(),
		Forge:        s.repo.Forge,
		Artifacts:    artifacts{client: s.dagger, ref: s.node.GetReference()},
		Gates:        gates{runner: r, state: s},
		Now:          r.options.Now,
	}
	return evidence.Verify(ctx, step.GetEvidence(), asked)
}

// autoApprovedBy is the predicate that closed a gate step without a human,
// for the verifier that has no node to read because none was ever filed.
func autoApprovedBy(step *millv1.Step, res result) string {
	if !res.autoApproved {
		return ""
	}
	return step.GetGate().GetAuto()
}

// committer is the identity the run's workspace commits under, as the
// `commit` verifier compares against. A run with no workspace has none, and
// the verifier refuses rather than accepting a commit nobody's identity
// stands behind.
func committer(ws Workspace) evidence.Committer {
	if ws == nil {
		return evidence.Committer{}
	}
	identity := ws.Committer()
	return evidence.Committer{Name: identity.Name, Email: identity.Email}
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
	if err := s.dagger.AttachText(ctx,
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

// A verdict, once the verifier has parsed it, is the mill's to record and to
// carry: docs/domain-model.md §Verdict stores it twice — as evidence in the
// ledger, and as a text artifact on the node's result so a human reads it
// next to the work — and a `revise` is what the implementer is handed as its
// next prompt. Both halves happen whichever way the decision went: a revise
// is as much the record of what the run did as an approve, and the findings
// are exactly what the next attempt has to answer.

// reviewed is what the run loop does with a step that closed, or did not
// close, on a verdict. It returns the feedback the revise target receives,
// which is empty for an approve and for output that did not parse at all.
func (r *Runner) reviewed(ctx context.Context, s *state, step *millv1.Step, sr *millv1.StepRun, verified evidence.Evidence) (string, error) {
	verdict := verified.Verdict
	if step.GetEvidence() != millv1.EvidenceKind_EVIDENCE_KIND_VERDICT || verdict == nil {
		return "", nil
	}
	if err := r.attachVerdict(ctx, s, step, sr, verdict); err != nil {
		return "", err
	}
	if verdict.GetDecision() != millv1.VerdictDecision_VERDICT_DECISION_REVISE {
		return "", nil
	}
	return verdictFeedback(verdict), nil
}

// attachVerdict puts the verdict on the node as a text artifact named for
// the step, the generation and the attempt — `review-1.2.md` — carrying the
// decision, the findings, and the id of the session that reviewed. It is
// the half of the record a person reads: the ledger holds the same verdict
// as evidence, and a node in dagger is where somebody looks when asking why
// a change was sent back. The generation is in the name because dagger
// admits one artifact per name on a node and a restart's generation reviews
// again from attempt 1 (mill#93).
func (r *Runner) attachVerdict(ctx context.Context, s *state, step *millv1.Step, sr *millv1.StepRun, verdict *millv1.Verdict) error {
	name := fmt.Sprintf("%s-%d.%d.md", step.GetName(), s.run.GetGeneration(), sr.GetAttempt())
	body := verdictDocument(s, step, sr, verdict)
	return s.dagger.AttachText(ctx,
		dagger.RequestID(s.run.GetId(), name, sr.GetAttempt()), s.lease,
		s.node.GetReference(), name, "the "+step.GetName()+" step's verdict", body,
		map[string]any{
			"run_id":     s.run.GetId(),
			"step":       step.GetName(),
			"generation": float64(s.run.GetGeneration()),
			"attempt":    float64(sr.GetAttempt()),
			"session_id": sr.GetSessionId(),
			"decision":   decisionWord(verdict.GetDecision()),
			"digest":     digest(body),
		})
}

// verdictDocument is the verdict as a person reads it. It is written from
// the parsed verdict and never from the session's prose: what is on the node
// is what the mill acted on.
func verdictDocument(s *state, step *millv1.Step, sr *millv1.StepRun, verdict *millv1.Verdict) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s of %s\n\n", step.GetName(), s.node.GetReference())
	fmt.Fprintf(&b, "- decision: %s\n", decisionWord(verdict.GetDecision()))
	fmt.Fprintf(&b, "- run: %s\n", s.run.GetId())
	fmt.Fprintf(&b, "- step: %s attempt %d\n", step.GetName(), sr.GetAttempt())
	fmt.Fprintf(&b, "- session: %s\n", sr.GetSessionId())
	if len(verdict.GetFindings()) == 0 {
		b.WriteString("\nNo findings.\n")
		return b.String()
	}
	b.WriteString("\n## Findings\n")
	for _, finding := range verdict.GetFindings() {
		fmt.Fprintf(&b, "\n- **%s** %s: %s\n", severityWord(finding.GetSeverity()),
			dashed(finding.GetCriterion()), finding.GetSummary())
		if finding.GetLocation() != "" {
			fmt.Fprintf(&b, "  - where: %s\n", finding.GetLocation())
		}
	}
	return b.String()
}

// verdictFeedback is what the implementer is given as its next prompt: the
// blocking findings first and by criterion, then the notes, because the
// blocking ones are the contract and the notes are advice.
func verdictFeedback(verdict *millv1.Verdict) string {
	var b strings.Builder
	b.WriteString("The reviewer asked for changes. Its findings, against the node's acceptance criteria:\n")
	for _, severity := range []millv1.FindingSeverity{
		millv1.FindingSeverity_FINDING_SEVERITY_BLOCKING,
		millv1.FindingSeverity_FINDING_SEVERITY_NOTE,
	} {
		for _, finding := range verdict.GetFindings() {
			if finding.GetSeverity() != severity {
				continue
			}
			fmt.Fprintf(&b, "\n- %s (%s)", finding.GetSummary(), severityWord(severity))
			if criterion := finding.GetCriterion(); criterion != "" {
				fmt.Fprintf(&b, " on %s", criterion)
			}
			if location := finding.GetLocation(); location != "" {
				fmt.Fprintf(&b, " at %s", location)
			}
			b.WriteString("\n")
		}
	}
	b.WriteString("\nA blocking finding is a criterion the change does not yet meet; a note is advice you may " +
		"take or leave. Fix the blocking findings and nothing else.\n")
	return b.String()
}

// decisionWord is a decision as the record and the schema spell it.
func decisionWord(decision millv1.VerdictDecision) string {
	return strings.ToLower(strings.TrimPrefix(decision.String(), "VERDICT_DECISION_"))
}

// severityWord is a finding's severity as the schema spells it.
func severityWord(severity millv1.FindingSeverity) string {
	return strings.ToLower(strings.TrimPrefix(severity.String(), "FINDING_SEVERITY_"))
}

// dashed is a value or a dash, for a line that reads either way.
func dashed(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

// reviewBrief is the work the reviewer judges, put in front of it by the
// mill: which step's work, and where it is.
//
// A target that closes on a file — `plan` — is named by that file's path:
// the plan is untracked until `commit`, so a diff would not show it, and
// the file is the whole of what the step wrote. A reviewer that had to
// guess the path found it once and answered from memory the next time
// (mill#90).
//
// For any other target the reviewer is told which span is this run's — the
// base commit of the first attempt at the step this step's `on_failure`
// returns to, the branch, and the files changed since — and fetches the
// diff itself: on the machine backend it has a shell against a read-only
// checkout (docs/sandbox.md §Exec), so the mill names what to read rather
// than pasting a diff that crowds out the contract it is judging against.
//
// Empty when there is nothing to show: a review step with no revise target,
// a run whose target step has not written yet, or a span with no diff in it.
func (r *Runner) reviewBrief(ctx context.Context, s *state, step *millv1.Step, detail *millv1.InspectResponse) (string, error) {
	if step.GetEvidence() != millv1.EvidenceKind_EVIDENCE_KIND_VERDICT || s.workspace == nil {
		return "", nil
	}
	target := step.GetOnFailure().GetReviseTarget()
	if target == "" {
		return "", nil
	}
	path, err := r.artifactPath(s, stepNamed(s.workflow, target))
	if err != nil {
		return "", err
	}
	if path != "" {
		return "## The file you are judging\n\n" +
			fmt.Sprintf("The `%s` step wrote `%s` in your working directory, and the mill read that file back as the step's evidence. "+
				"Read it; it is the whole of what the step produced.\n", target, path), nil
	}
	base := firstBase(detail, target)
	if base == "" {
		return "", nil
	}
	git := s.workspace.GitRunner()
	stat, err := git.Run(ctx, nil, "diff", "--stat", base)
	if err != nil {
		return "", fmt.Errorf("step %s: the diff since %s could not be read: %w", step.GetName(), base, err)
	}
	if strings.TrimSpace(stat) == "" {
		return "", nil
	}

	var b strings.Builder
	b.WriteString("## The change you are reviewing\n\n")
	fmt.Fprintf(&b, "This run's work is everything since it started at the `%s` step: "+
		"the commits after %s on branch %s. Run `git diff %s` for the change; "+
		"the files below are what it touched, and they are in your working directory.\n\n",
		target, short(base), s.run.GetBranch(), short(base))
	b.WriteString("```\n" + strings.TrimRight(stat, "\n") + "\n```\n")
	return b.String(), nil
}

// revisionBrief is what a verdict step is told when it runs again after its
// own `revise` sent the work back: the target ran again in answer to its
// findings, where the work is, and whether it changed. The rest — the skill,
// the contract, the reviewer's own findings — is in the session already,
// because the reviewer's session is resumed; this is the part that is new.
//
// Everything here is read from the ledger, never remembered by the worker
// (Invariant 10): the target's latest closed attempt names, in
// `feedback_from`, the attempt of this step whose verdict it answers, which
// is what makes this a re-entry and not a first round; and where the target
// closes on a file, the closed attempt's artifact digest against the
// superseded one's says whether the file changed. A target without a file
// gets the fact of the re-run and the rule.
//
// Empty when this is the step's first look at the work.
func (r *Runner) revisionBrief(s *state, step *millv1.Step, detail *millv1.InspectResponse) (string, error) {
	if step.GetEvidence() != millv1.EvidenceKind_EVIDENCE_KIND_VERDICT {
		return "", nil
	}
	target := step.GetOnFailure().GetReviseTarget()
	if target == "" {
		return "", nil
	}
	closed := latestClosed(detail, target)
	if closed == nil {
		return "", nil
	}
	answered := stepRunByID(detail, closed.GetStepRun().GetFeedbackFrom())
	if answered == nil || answered.GetStepRun().GetStepName() != step.GetName() {
		return "", nil
	}
	path, err := r.artifactPath(s, stepNamed(s.workflow, target))
	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString("## What changed since your last verdict\n\n")
	fmt.Fprintf(&b, "The `%s` step ran again (attempt %d) in answer to your findings.", target, closed.GetStepRun().GetAttempt())
	if path != "" {
		fmt.Fprintf(&b, " The file you are judging is `%s`; ", path)
		prior := supersededBefore(detail, target, closed.GetStepRun().GetAttempt())
		switch {
		case prior == nil || prior.GetEvidence().GetArtifact() == "":
			b.WriteString("read it again.")
		case prior.GetEvidence().GetArtifact() == closed.GetEvidence().GetArtifact():
			b.WriteString("it is byte-identical to the one you judged last.")
		default:
			b.WriteString("it differs from the one you judged last.")
		}
	}
	b.WriteString(" Judge it against the same criteria as before, and read the work rather than answering from memory: " +
		"a blocking finding you did not raise last round must be a gap the revision introduced, not one you could have raised then.\n")
	return b.String(), nil
}

// latestClosed is the closed attempt at a step with the highest attempt
// number, or nil. Inspect orders step runs by when they started, which a
// revise makes a different order from attempt.
func latestClosed(detail *millv1.InspectResponse, step string) *millv1.StepRunDetail {
	var found *millv1.StepRunDetail
	for _, d := range detail.GetSteps() {
		sr := d.GetStepRun()
		if sr.GetStepName() != step || sr.GetStatus() != millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED {
			continue
		}
		if found == nil || sr.GetAttempt() > found.GetStepRun().GetAttempt() {
			found = d
		}
	}
	return found
}

// supersededBefore is the superseded attempt at a step with the highest
// attempt number below the given one, or nil: the work the reviewer judged
// last, before the revise reopened the step.
func supersededBefore(detail *millv1.InspectResponse, step string, attempt int64) *millv1.StepRunDetail {
	var found *millv1.StepRunDetail
	for _, d := range detail.GetSteps() {
		sr := d.GetStepRun()
		if sr.GetStepName() != step || sr.GetStatus() != millv1.StepRunStatus_STEP_RUN_STATUS_SUPERSEDED || sr.GetAttempt() >= attempt {
			continue
		}
		if found == nil || sr.GetAttempt() > found.GetStepRun().GetAttempt() {
			found = d
		}
	}
	return found
}

// stepRunByID is one attempt by its id, or nil.
func stepRunByID(detail *millv1.InspectResponse, id string) *millv1.StepRunDetail {
	if id == "" {
		return nil
	}
	for _, d := range detail.GetSteps() {
		if d.GetStepRun().GetId() == id {
			return d
		}
	}
	return nil
}

// firstBase is the base commit of the earliest attempt at a step: where this
// run's work on that step started, across every attempt and every revise.
func firstBase(detail *millv1.InspectResponse, step string) string {
	for _, d := range detail.GetSteps() {
		if sr := d.GetStepRun(); sr.GetStepName() == step && sr.GetBaseCommit() != "" {
			return sr.GetBaseCommit()
		}
	}
	return ""
}

// earliestBase is the base commit of a step's earliest attempt this
// generation, for `commit` evidence to judge an unmoved head against
// (evidence.Step.EarliestBase). No other evidence kind reads it, so the scan
// only runs for a commit step.
func earliestBase(step *millv1.Step, detail *millv1.InspectResponse) string {
	if step.GetEvidence() != millv1.EvidenceKind_EVIDENCE_KIND_COMMIT {
		return ""
	}
	return firstBase(detail, step.GetName())
}
