// Package evidence is where a step is closed, or is not.
//
// A step is complete when the mill has verified a fact *outside* the session:
// a commit SHA that resolves on the run's branch, a process that exited with
// the code the definition names, a verdict that parsed and approved, a URL
// the forge returns when asked for the branch's pull request, a status the
// forge reports as success, a digest dagger accepted, a gate node that is
// done. An agent saying "done", a transcript that reads well, or a task list
// marked complete is not evidence and never closes a step
// (docs/core-principles.md §8, docs/domain-model.md §Evidence).
//
// Three rules shape this package.
//
// **Nothing a session says is consulted.** The verifiers reach the world
// through the seams on [Step] — git in the run's checkout, the forge, dagger
// — and not through the session. [Session.FinalMessage] and
// [Session.Transcript] are on [Step] precisely so that the rule is checkable:
// a spec hands the verifiers a transcript that records every open and asserts
// nothing opened it. The two kinds that read a session at all read its
// *structured output*: `verdict` and `structured`, both of which parse a
// schema-constrained document rather than prose, and both of which the
// domain model defines that way.
//
// **A refusal is not a failure.** [PendingError] is "not yet" — the forge's
// checks have not settled — and leaves the step open to be verified again on
// the workflow's interval. [NotVerifiedError] is "no": the fact is not true,
// and the step's `on_failure` decides what happens next. A caller that
// cannot tell them apart would either park a run on a pipeline that is
// merely slow or retry one that is red.
//
// **Verification never writes.** [Verify] returns the evidence row for the
// caller to record inside its own transaction, and returns nothing at all
// when the fact does not hold, so a refused verification leaves no row
// behind.
package evidence

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	harnessv1 "github.com/ivy/mill/gen/harness/v1"
	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/forge"
)

// How a `status` step watches the forge's checks when the workflow does not
// say. The interval is short enough that a pipeline finishing does not sit
// unnoticed and long enough that a run watching one is not a load generator;
// the cap is what stops a run waiting forever on a pipeline nobody will ever
// finish. Both are workflow-level settings — `status.interval` and
// `status.timeout` in workflow.yaml.
const (
	// DefaultStatusInterval is how often the forge is asked.
	DefaultStatusInterval = time.Minute
	// DefaultStatusTimeout is how long a step may keep asking.
	DefaultStatusTimeout = time.Hour
)

// Git is one git command in the run's checkout, run through the workspace's
// chokepoint. The commit verifier is its only caller: it asks git whether an
// object exists and whether it is on the run's branch, which are facts about
// the repository and not about the session.
type Git interface {
	Run(ctx context.Context, env map[string]string, args ...string) (string, error)
}

// Forge is what the `url` and `status` verifiers need of the project's
// forge. It is the read half of [github.com/ivy/mill/internal/forge.Forge],
// which satisfies it.
type Forge interface {
	// PR is the pull request for a branch, forge.ErrNoPullRequest when the
	// forge has none.
	PR(ctx context.Context, branch string) (forge.PullRequest, error)
	// Checks is what the forge's checks say about a commit.
	Checks(ctx context.Context, sha string) (forge.Status, error)
}

// Artifacts is dagger, asked whether it holds an artifact. `artifact`
// evidence is a digest dagger accepted, so the acceptance is dagger's to
// confirm and not the session's to claim.
type Artifacts interface {
	Accepted(ctx context.Context, digest string) (bool, error)
}

// GateNode is a dagger node as the `gate` verifier reads it.
type GateNode struct {
	// ID is the gate node's id.
	ID string
	// Status is dagger's own: `open`, `done`, or `dropped`.
	Status string
	// Result is what the node completed with, attached to the gate.
	Result string
}

// Gates is dagger, asked about the node a gate step filed. It is an
// interface because the dagger client belongs to the run loop: this package
// reads one node through whatever the worker built, and depends on none of
// it.
type Gates interface {
	Node(ctx context.Context, id string) (GateNode, error)
}

// Transcript is the session's transcript as the mill can reach it. It is
// here to be left alone: a transcript is not evidence, and a spec that hands
// the verifiers a recording transcript and finds the recorder empty is what
// makes that more than an assertion in a comment.
type Transcript struct {
	// Path is where the harness keeps it on the worker's host.
	Path string
	// Open opens it. Nil when the harness offers no way to.
	Open func(ctx context.Context) (io.ReadCloser, error)
}

// Session is what the mill holds of the session that ran the step.
//
// FinalMessage and Transcript are prose and are never evidence. Output is
// the harness's structured channel — a document the session was required to
// produce against a schema — and is what the `verdict` and `structured`
// verifiers parse.
type Session struct {
	// FinalMessage is the session's last assistant message.
	FinalMessage string
	// Output is the session's structured output, as the harness returned it.
	Output map[string]any
	// Transcript is where the transcript lives.
	Transcript Transcript
}

// Step is one step run, and everything a verifier may consult about it.
type Step struct {
	// Name is the step's name in the workflow, for the record of what
	// verified the evidence.
	Name string
	// Definition is the step as the workflow defines it: its check's exit
	// codes, its output schema.
	Definition *millv1.Step
	// Workflow is the definition the run follows, read for the workflow-level
	// settings a verifier honours. Optional; the defaults apply without it.
	Workflow *millv1.Workflow
	// Branch is the run's working branch. `commit` evidence is a SHA on it,
	// and `url` evidence is the pull request for it.
	Branch string
	// Claim is the fact the mill observed and is asking to have verified: a
	// SHA the workspace's own `git.commit` returned, the head the run
	// pushed, an artifact digest, the gate node the step filed.
	//
	// It is never anything a session said. A session that reports a commit
	// it did not make reports it into a transcript nothing here reads, and
	// the step stays open.
	Claim string
	// Exit is how the check's process ended, as the check runner recorded
	// it. Only `exit` reads it.
	Exit *harnessv1.ProcessExit
	// Session is what the session produced. Only `verdict` and `structured`
	// read it, and only its structured output.
	Session Session
	// Elapsed is how long this step has been waiting for a fact that has not
	// settled, across attempts. The `status` verifier compares it against
	// the workflow's cap.
	Elapsed time.Duration
	// Git runs git in the run's checkout, for `commit`.
	Git Git
	// Forge is the project's forge, for `url` and `status`.
	Forge Forge
	// Artifacts is dagger's artifact store, for `artifact`.
	Artifacts Artifacts
	// Gates is dagger, for `gate`.
	Gates Gates
	// Now is the clock, for `verified_at`. Nil means [time.Now].
	Now func() time.Time
}

// Evidence is what a verifier established.
type Evidence struct {
	// Value is the row the ledger writes, in the shape
	// [github.com/ivy/mill/internal/domain.ValidateEvidence] accepts. Nil
	// when nothing was verified.
	Value *millv1.Evidence
	// Warnings are what the ledger records beside the row: a verdict that
	// said revise with nothing blocking is the one the domain model names.
	Warnings []string
	// Verdict is the reviewer's verdict as it parsed, present whenever it
	// parsed at all — including when it did not close the step. The run
	// loop needs the findings for the revise prompt and for the comment it
	// leaves on the node, and a refusal is exactly when it needs them.
	Verdict *millv1.Verdict
}

// NotVerifiedError is a fact the mill could not establish. The step stays
// open and its `on_failure` decides what happens next.
type NotVerifiedError struct {
	// Kind is the evidence that was asked for.
	Kind millv1.EvidenceKind
	// Reason says why it does not hold.
	Reason string
	// Err is what the seam returned, when a seam was the reason.
	Err error
}

func (e *NotVerifiedError) Error() string {
	return fmt.Sprintf("%s evidence: %s", kindName(e.Kind), e.Reason)
}

func (e *NotVerifiedError) Unwrap() error { return e.Err }

// PendingError is "not yet": the fact has not settled, and asking again
// later is the whole of what to do about it. A pending status is not a
// failed step — the step stays open and is verified again after RetryAfter
// (docs/domain-model.md §Evidence, `status`).
type PendingError struct {
	// Kind is the evidence that was asked for.
	Kind millv1.EvidenceKind
	// Reason says what has not settled.
	Reason string
	// RetryAfter is how long to wait before asking again.
	RetryAfter time.Duration
}

func (e *PendingError) Error() string {
	return fmt.Sprintf("%s evidence: %s; not yet", kindName(e.Kind), e.Reason)
}

// Pending reports whether an error is "not yet" rather than "no".
func Pending(err error) bool {
	var pending *PendingError
	return errors.As(err, &pending)
}

// commitSHA is a git object name: forty hex digits under SHA-1, sixty-four
// under SHA-256. A claim that is not one is refused before git is asked, so
// that nothing shaped like an option ever reaches an argument list.
var commitSHA = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// Verify establishes the fact that closes a step, or refuses.
//
// The context comes first, as everything else in the tree takes it; the
// arguments are otherwise the node's contract's: the kind of evidence the
// step names, and the step.
func Verify(ctx context.Context, kind millv1.EvidenceKind, step Step) (Evidence, error) {
	verifier, ok := verifiers[kind]
	if !ok {
		return Evidence{}, &NotVerifiedError{Kind: kind, Reason: "no verifier: a step is closed by evidence of a kind the model defines"}
	}
	evidence, err := verifier(ctx, step)
	if err != nil {
		return evidence, err
	}
	if evidence.Value != nil {
		evidence.Value.Kind = kind
		evidence.Value.VerifiedBy = verifiedBy(kind, step)
		evidence.Value.VerifiedAt = timestamppb.New(step.now())
	}
	return evidence, nil
}

// verifiers is one verifier per evidence kind. The table is the domain
// model's own: docs/domain-model.md §Evidence lists what each kind proves,
// and each function here is that sentence in code.
var verifiers = map[millv1.EvidenceKind]func(context.Context, Step) (Evidence, error){
	millv1.EvidenceKind_EVIDENCE_KIND_COMMIT:     verifyCommit,
	millv1.EvidenceKind_EVIDENCE_KIND_EXIT:       verifyExit,
	millv1.EvidenceKind_EVIDENCE_KIND_VERDICT:    verifyVerdict,
	millv1.EvidenceKind_EVIDENCE_KIND_STRUCTURED: verifyStructured,
	millv1.EvidenceKind_EVIDENCE_KIND_URL:        verifyURL,
	millv1.EvidenceKind_EVIDENCE_KIND_STATUS:     verifyStatus,
	millv1.EvidenceKind_EVIDENCE_KIND_ARTIFACT:   verifyArtifact,
	millv1.EvidenceKind_EVIDENCE_KIND_GATE:       verifyGate,
}

// verifyCommit proves a commit: the object resolves in the run's checkout,
// and it is on the run's branch.
//
// Both halves are load-bearing. An object that exists proves only that
// something was written — a commit made on a detached head, a commit fetched
// from elsewhere, an object a session created by hand — and the run's claim
// on the repository is its branch. So a SHA that resolves but is not an
// ancestor of the branch head is refused.
func verifyCommit(ctx context.Context, step Step) (Evidence, error) {
	kind := millv1.EvidenceKind_EVIDENCE_KIND_COMMIT
	refuse := func(reason string, err error) (Evidence, error) {
		return Evidence{}, &NotVerifiedError{Kind: kind, Reason: reason, Err: err}
	}
	if step.Git == nil {
		return refuse("no checkout to resolve it in", nil)
	}
	if step.Branch == "" {
		return refuse("the run has no branch to be on", nil)
	}
	sha := strings.TrimSpace(step.Claim)
	if sha == "" {
		// Nothing observed a commit. What the session said it did is not
		// consulted, so there is nothing else to check.
		return refuse("no commit was recorded for this step", nil)
	}
	if !commitSHA.MatchString(sha) {
		return refuse(fmt.Sprintf("%q is not a git object name", sha), nil)
	}
	if _, err := step.Git.Run(ctx, nil, "cat-file", "-e", sha+"^{commit}"); err != nil {
		return refuse(fmt.Sprintf("%s does not resolve to a commit in the checkout", sha), err)
	}
	if _, err := step.Git.Run(ctx, nil, "merge-base", "--is-ancestor", sha, step.Branch); err != nil {
		return refuse(fmt.Sprintf("%s is not on %s", sha, step.Branch), err)
	}
	return Evidence{Value: &millv1.Evidence{Value: &millv1.Evidence_Commit{Commit: sha}}}, nil
}

// verifyExit proves a check's exit: the process the workspace ran exited
// with a code the definition names. A process that never ran — status -1 —
// proves nothing at all, whatever the definition would have accepted.
func verifyExit(_ context.Context, step Step) (Evidence, error) {
	kind := millv1.EvidenceKind_EVIDENCE_KIND_EXIT
	exit := step.Exit
	if exit == nil {
		return Evidence{}, &NotVerifiedError{Kind: kind, Reason: "no process ran"}
	}
	if exit.GetStatus() < 0 {
		return Evidence{}, &NotVerifiedError{Kind: kind, Reason: fmt.Sprintf(
			"the process never ran (%s)", reasonName(exit.GetReason()))}
	}
	for _, code := range Codes(step.Definition.GetCheck()) {
		if exit.GetStatus() == code {
			return Evidence{Value: &millv1.Evidence{Value: &millv1.Evidence_Exit{Exit: exit}}}, nil
		}
	}
	return Evidence{}, &NotVerifiedError{Kind: kind, Reason: fmt.Sprintf(
		"exited %d, not %s", exit.GetStatus(), joinCodes(Codes(step.Definition.GetCheck())))}
}

// Codes are the exit statuses a check passes with. An empty definition means
// zero, which is what every convention in reach already means by it.
func Codes(check *millv1.Check) []int32 {
	if codes := check.GetExit(); len(codes) > 0 {
		return codes
	}
	return []int32{0}
}

// verifyVerdict proves a review: the session's structured output parsed as a
// verdict, its decision is one of the two the model allows, and its findings
// are shaped like findings.
//
// Two rules from docs/domain-model.md §Verdict are applied here rather than
// left to a reader. A finding that names no criterion is recorded as a
// `note`, because a reviewer that cannot tie a finding to a criterion is
// drifting from its contract. And a `revise` with zero blocking findings is
// a definition error: it is recorded as an `approve` with a warning, because
// a reviewer that asks for changes and names none has not asked for
// anything, and sending the work back on it would spend an attempt on
// nothing.
func verifyVerdict(_ context.Context, step Step) (Evidence, error) {
	kind := millv1.EvidenceKind_EVIDENCE_KIND_VERDICT
	refuse := func(reason string, err error) (Evidence, error) {
		return Evidence{}, &NotVerifiedError{Kind: kind, Reason: reason, Err: err}
	}
	if step.Session.Output == nil {
		return refuse("the session produced no structured output", nil)
	}
	if err := ValidateJSON(verdictSchema, step.Session.Output); err != nil {
		return refuse(fmt.Sprintf("the verdict does not parse: %s", err), err)
	}
	verdict := &millv1.Verdict{}
	var warnings []string
	decision, _ := step.Session.Output["decision"].(string)
	switch decision {
	case "approve":
		verdict.Decision = millv1.VerdictDecision_VERDICT_DECISION_APPROVE
	case "revise":
		verdict.Decision = millv1.VerdictDecision_VERDICT_DECISION_REVISE
	default:
		return refuse(fmt.Sprintf("%q is not a decision: a reviewer approves or revises", decision), nil)
	}

	findings, _ := step.Session.Output["findings"].([]any)
	blocking := 0
	for i, item := range findings {
		fields, ok := item.(map[string]any)
		if !ok {
			return refuse(fmt.Sprintf("finding %d is not an object", i), nil)
		}
		finding := &millv1.Finding{
			Criterion: text(fields["criterion"]),
			Summary:   text(fields["summary"]),
			Location:  text(fields["location"]),
			Severity:  millv1.FindingSeverity_FINDING_SEVERITY_NOTE,
		}
		if finding.GetCriterion() == "" {
			warnings = append(warnings, fmt.Sprintf(
				"finding %d names no acceptance criterion and is recorded as a note", i))
		} else if text(fields["severity"]) == "blocking" {
			finding.Severity = millv1.FindingSeverity_FINDING_SEVERITY_BLOCKING
			blocking++
		}
		verdict.Findings = append(verdict.Findings, finding)
	}

	if verdict.GetDecision() == millv1.VerdictDecision_VERDICT_DECISION_REVISE {
		if blocking > 0 {
			return Evidence{Verdict: verdict, Warnings: warnings}, &NotVerifiedError{Kind: kind, Reason: fmt.Sprintf(
				"the reviewer asked for changes: %d blocking %s", blocking, plural(blocking, "finding", "findings"))}
		}
		verdict.Decision = millv1.VerdictDecision_VERDICT_DECISION_APPROVE
		warnings = append(warnings, "the verdict said revise with no blocking finding, which is a definition error; recorded as approve")
	}
	return Evidence{
		Value:    &millv1.Evidence{Value: &millv1.Evidence_Verdict{Verdict: verdict}},
		Warnings: warnings,
		Verdict:  verdict,
	}, nil
}

// verdictSchema is docs/domain-model.md §Verdict as a schema. The verdict is
// parsed, never read: this is the whole of what a reviewer's output is
// allowed to be.
const verdictSchema = `{
  "type": "object",
  "required": ["decision"],
  "properties": {
    "decision": {"type": "string", "enum": ["approve", "revise"]},
    "findings": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["summary"],
        "properties": {
          "criterion": {"type": "string"},
          "severity": {"type": "string", "enum": ["blocking", "note"]},
          "summary": {"type": "string", "minLength": 1},
          "location": {"type": "string"}
        },
        "additionalProperties": false
      }
    }
  },
  "additionalProperties": false
}`

// verifyStructured proves an assessment or a plan: the session's structured
// output validated against the step's own schema. A failure names the first
// violating path, because that is what a session can act on.
func verifyStructured(_ context.Context, step Step) (Evidence, error) {
	kind := millv1.EvidenceKind_EVIDENCE_KIND_STRUCTURED
	refuse := func(reason string, err error) (Evidence, error) {
		return Evidence{}, &NotVerifiedError{Kind: kind, Reason: reason, Err: err}
	}
	schema := step.Definition.GetOutputSchema()
	if strings.TrimSpace(schema) == "" {
		return refuse("the step declares no output schema, so nothing validates the output", nil)
	}
	if step.Session.Output == nil {
		return refuse("the session produced no structured output", nil)
	}
	if err := ValidateJSON(schema, step.Session.Output); err != nil {
		return refuse(err.Error(), err)
	}
	value, err := structpb.NewStruct(step.Session.Output)
	if err != nil {
		return refuse(fmt.Sprintf("the output is not a JSON document: %s", err), err)
	}
	return Evidence{Value: &millv1.Evidence{Value: &millv1.Evidence_Structured{Structured: value}}}, nil
}

// verifyURL proves a pull request: the forge returns one when asked for the
// run's branch. The session's word for the URL is not asked for and would
// not be believed; the forge's answer is the evidence.
func verifyURL(ctx context.Context, step Step) (Evidence, error) {
	kind := millv1.EvidenceKind_EVIDENCE_KIND_URL
	refuse := func(reason string, err error) (Evidence, error) {
		return Evidence{}, &NotVerifiedError{Kind: kind, Reason: reason, Err: err}
	}
	if step.Forge == nil {
		return refuse("the run has no forge to ask", nil)
	}
	if step.Branch == "" {
		return refuse("the run has no branch to ask about", nil)
	}
	pr, err := step.Forge.PR(ctx, step.Branch)
	switch {
	case errors.Is(err, forge.ErrNoPullRequest):
		return refuse(fmt.Sprintf("the forge has no pull request for %s", step.Branch), err)
	case err != nil:
		return refuse(fmt.Sprintf("the forge could not be asked about %s", step.Branch), err)
	case pr.URL == "":
		return refuse("the forge returned a pull request with no URL", nil)
	}
	return Evidence{Value: &millv1.Evidence{Value: &millv1.Evidence_Url{Url: pr.URL}}}, nil
}

// verifyStatus proves CI: the forge reports the commit's checks as success.
//
// The three answers are three different things. `success` is evidence.
// `failed` is a refusal, and the step's `on_failure` decides. `pending` is
// neither: the pipeline has not finished, so the step stays open and is
// asked again on the workflow's interval, until the workflow's cap turns the
// wait into a refusal.
func verifyStatus(ctx context.Context, step Step) (Evidence, error) {
	kind := millv1.EvidenceKind_EVIDENCE_KIND_STATUS
	refuse := func(reason string, err error) (Evidence, error) {
		return Evidence{}, &NotVerifiedError{Kind: kind, Reason: reason, Err: err}
	}
	if step.Forge == nil {
		return refuse("the run has no forge to ask", nil)
	}
	sha := strings.TrimSpace(step.Claim)
	if sha == "" {
		return refuse("no commit was recorded to read checks for", nil)
	}
	status, err := step.Forge.Checks(ctx, sha)
	if err != nil {
		return refuse(fmt.Sprintf("the forge could not be asked about %s", sha), err)
	}
	switch status {
	case forge.Success:
		return Evidence{Value: &millv1.Evidence{Value: &millv1.Evidence_Status{Status: string(forge.Success)}}}, nil
	case forge.Failed:
		return refuse(fmt.Sprintf("the forge reports the checks on %s as failed", sha), nil)
	case forge.Pending:
		interval, limit := step.statusPolicy()
		if step.Elapsed >= limit {
			return refuse(fmt.Sprintf("the checks on %s had not settled after %s", sha, limit), nil)
		}
		return Evidence{}, &PendingError{
			Kind:       kind,
			Reason:     fmt.Sprintf("the checks on %s are still running", sha),
			RetryAfter: interval,
		}
	default:
		return refuse(fmt.Sprintf("the forge reports %q, which is not a status", status), nil)
	}
}

// statusPolicy is how this run watches the forge's checks: the workflow's
// setting, or the documented default.
func (s Step) statusPolicy() (interval, timeout time.Duration) {
	interval, timeout = DefaultStatusInterval, DefaultStatusTimeout
	policy := s.Workflow.GetStatus()
	if d := policy.GetInterval().AsDuration(); d > 0 {
		interval = d
	}
	if d := policy.GetTimeout().AsDuration(); d > 0 {
		timeout = d
	}
	return interval, timeout
}

// verifyArtifact proves an artifact: dagger accepted something with this
// digest. Dagger holding the bytes is the fact; a session naming a digest is
// not.
func verifyArtifact(ctx context.Context, step Step) (Evidence, error) {
	kind := millv1.EvidenceKind_EVIDENCE_KIND_ARTIFACT
	refuse := func(reason string, err error) (Evidence, error) {
		return Evidence{}, &NotVerifiedError{Kind: kind, Reason: reason, Err: err}
	}
	if step.Artifacts == nil {
		return refuse("there is nothing to confirm the digest against", nil)
	}
	digest := strings.TrimSpace(step.Claim)
	if digest == "" {
		return refuse("no artifact was recorded for this step", nil)
	}
	accepted, err := step.Artifacts.Accepted(ctx, digest)
	if err != nil {
		return refuse(fmt.Sprintf("dagger could not be asked about %s", digest), err)
	}
	if !accepted {
		return refuse(fmt.Sprintf("dagger holds no artifact with digest %s", digest), nil)
	}
	return Evidence{Value: &millv1.Evidence{Value: &millv1.Evidence_Artifact{Artifact: digest}}}, nil
}

// verifyGate proves a gate: the gate node dagger holds is `done` and its
// result is attached.
//
// An `open` gate is a human who has not answered yet, which is pending
// rather than failed — though a run parked at a gate is waiting on a lease
// it has released, not polling. A `dropped` gate is the denial, and it is a
// refusal: the run records the definition's deny_result and stops.
func verifyGate(ctx context.Context, step Step) (Evidence, error) {
	kind := millv1.EvidenceKind_EVIDENCE_KIND_GATE
	refuse := func(reason string, err error) (Evidence, error) {
		return Evidence{}, &NotVerifiedError{Kind: kind, Reason: reason, Err: err}
	}
	if step.Gates == nil {
		return refuse("there is no dagger to read the gate from", nil)
	}
	id := strings.TrimSpace(step.Claim)
	if id == "" {
		return refuse("no gate node was filed for this step", nil)
	}
	node, err := step.Gates.Node(ctx, id)
	if err != nil {
		return refuse(fmt.Sprintf("gate node %s could not be read", id), err)
	}
	switch node.Status {
	case "done":
		if strings.TrimSpace(node.Result) == "" {
			return refuse(fmt.Sprintf("gate node %s is done with no result attached", id), nil)
		}
		return Evidence{Value: &millv1.Evidence{Value: &millv1.Evidence_Gate{Gate: id}}}, nil
	case "dropped":
		return refuse(fmt.Sprintf("gate node %s was dropped: the decision is no", id), nil)
	default:
		interval, _ := step.statusPolicy()
		return Evidence{}, &PendingError{
			Kind:       kind,
			Reason:     fmt.Sprintf("gate node %s is %s", id, node.Status),
			RetryAfter: interval,
		}
	}
}

// verifiedBy is what the ledger records as having established the evidence:
// the check, when a check ran, and the mill routine otherwise.
func verifiedBy(kind millv1.EvidenceKind, step Step) string {
	if step.Definition.GetKind() == millv1.StepKind_STEP_KIND_CHECK && step.Name != "" {
		return "check " + step.Name
	}
	return "evidence." + kindName(kind)
}

func (s Step) now() time.Time {
	if s.Now == nil {
		return time.Now()
	}
	return s.Now()
}

// kindName is the evidence kind as the model spells it, which is also how
// [github.com/ivy/mill/internal/domain] spells it in the ledger.
func kindName(kind millv1.EvidenceKind) string {
	return strings.ToLower(strings.TrimPrefix(kind.String(), "EVIDENCE_KIND_"))
}

// reasonName is why a session never ran, as the ledger spells it.
func reasonName(reason harnessv1.ExitReason) string {
	return strings.ToLower(strings.TrimPrefix(reason.String(), "EXIT_REASON_"))
}

func text(value any) string {
	s, _ := value.(string)
	return s
}

func joinCodes(codes []int32) string {
	rendered := make([]string, 0, len(codes))
	for _, code := range codes {
		rendered = append(rendered, fmt.Sprint(code))
	}
	return strings.Join(rendered, " or ")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
