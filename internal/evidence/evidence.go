// Package evidence is where a step is closed, or is not.
//
// A step is complete when the mill has verified a fact *outside* the session:
// a commit SHA that resolves on the run's branch, a process that exited with
// the code the definition names, a verdict that parsed and approved, a URL
// the forge returns when asked for the branch's pull request, a status the
// forge reports as success, a digest skift accepted, a gate node that is
// done. An agent saying "done", a transcript that reads well, or a task list
// marked complete is not evidence and never closes a step
// (docs/core-principles.md §8, docs/domain-model.md §Evidence).
//
// Three rules shape this package.
//
// **Nothing a session says is consulted.** The verifiers reach the world
// through the seams on [Step] — git in the run's checkout, the forge, skift
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

	harnessv1 "github.com/steading-ai/mill/gen/harness/v1"
	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/forge"
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
// forge. It is the read half of [github.com/steading-ai/mill/internal/forge.Forge],
// which satisfies it.
type Forge interface {
	// PR is the pull request for a branch, forge.ErrNoPullRequest when the
	// forge has none.
	PR(ctx context.Context, branch string) (forge.PullRequest, error)
	// Checks is what the forge's checks say about a commit.
	Checks(ctx context.Context, sha string) (forge.Status, error)
	// FailingChecks is every check on a commit that is not green, so a
	// failed `status` can say which check and, through CheckLog, why.
	FailingChecks(ctx context.Context, sha string) ([]forge.CheckResult, error)
	// CheckLog is one failing check's log, filtered and capped (mill#95).
	CheckLog(ctx context.Context, check forge.CheckResult) (string, error)
}

// Committer is the identity the mill's own commits carry: the name and
// address git is given in the run's checkout. It is
// [github.com/steading-ai/mill/internal/workspace.Committer] as this package needs
// it, restated rather than imported so that verification depends on nothing
// but the fact it is checking.
type Committer struct {
	// Name is the name on the commit, the mill's `--name`.
	Name string
	// Email is the address it commits under, the mill's `--identity`.
	Email string
}

// String renders an identity the way git does: `Name <email>`.
func (c Committer) String() string { return c.Name + " <" + c.Email + ">" }

// Artifacts is skift, asked whether it holds an artifact. `artifact`
// evidence is a digest skift accepted, so the acceptance is skift's to
// confirm and not the session's to claim.
type Artifacts interface {
	Accepted(ctx context.Context, digest string) (bool, error)
}

// GateNode is a skift node as the `gate` verifier reads it.
type GateNode struct {
	// ID is the gate node's id.
	ID string
	// Status is skift's own: `open`, `done`, or `dropped`.
	Status string
	// Result is what the node completed with, attached to the gate.
	Result string
}

// Gates is skift, asked about the node a gate step filed. It is an
// interface because the skift client belongs to the run loop: this package
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

// StatusRead is one past read of the forge's checks for a head, folded by
// the `status` verifier into how long that head itself has been waited on
// (see Step.headWait). A run's `status` step may read many heads across its
// attempts — a catch-up or a revise can move the branch between them — and
// a head's own wait is measured from its own earliest read, not the step's.
type StatusRead struct {
	// Head is the sha the read was for.
	Head string
	// At is when the read happened.
	At time.Time
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
	// AutoApproved is the `auto` predicate that closed a gate step without
	// asking a human, and is empty for every other step and for a gate a
	// person answered. A gate closed by policy has no gate node to read —
	// nothing reaches skift at all — so the policy that fired is the fact
	// (docs/domain-model.md §Gate).
	AutoApproved string
	// Exit is how the check's process ended, as the check runner recorded
	// it. Only `exit` reads it.
	Exit *harnessv1.ProcessExit
	// Session is what the session produced. Only `verdict` and `structured`
	// read it, and only its structured output.
	Session Session
	// StatusHistory is every past read of the forge's checks at this step,
	// across attempts, for the `status` verifier's headWait to fold down to
	// how long the head it is currently asked about has itself been waited
	// on. A head that changed since the last read starts its own wait at
	// zero rather than inheriting an earlier head's (mill#91).
	StatusHistory []StatusRead
	// Base is the head of the run's branch when this attempt started, as the
	// ledger recorded it. `commit` evidence has to descend from it and not
	// be it: a step closes on a commit it made, not on whatever the branch
	// already pointed at. The one exception is an attempt whose head has
	// not moved from Base at all: see EarliestBase.
	Base string
	// EarliestBase is the base commit of this step's earliest attempt in this
	// generation, before any revise — the head the step was first handed.
	// Empty behaves as if this were the earliest attempt, i.e. the same as
	// Base; see verifyCommit for how an unmoved head is judged against it.
	EarliestBase string
	// Committer is the identity the mill commits under. `commit` evidence
	// has to carry it as both author and committer.
	Committer Committer
	// Git runs git in the run's checkout, for `commit`.
	Git Git
	// Forge is the project's forge, for `url` and `status`.
	Forge Forge
	// Artifacts is skift's artifact store, for `artifact`.
	Artifacts Artifacts
	// Gates is skift, for `gate`.
	Gates Gates
	// Now is the clock, for `verified_at`. Nil means [time.Now].
	Now func() time.Time
}

// Evidence is what a verifier established.
type Evidence struct {
	// Value is the row the ledger writes, in the shape
	// [github.com/steading-ai/mill/internal/domain.ValidateEvidence] accepts. Nil
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
	// Malformed says the session's output was not the document the step
	// asked for, rather than a fact that does not hold. A reviewer whose
	// verdict does not parse has judged nothing: there is no decision to
	// act on and nothing to send anybody, so a caller distinguishes the two
	// to avoid spending another step's attempt on a failure that is this
	// session's own (docs/domain-model.md §Verdict).
	Malformed bool
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

// Malformed reports whether an error is a session's output the mill could
// not parse, rather than a fact that does not hold.
func Malformed(err error) bool {
	var refused *NotVerifiedError
	return errors.As(err, &refused) && refused.Malformed
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
	// A revise verdict carries both: a value, because it is still evidence
	// recorded on the attempt it fails, and a NotVerifiedError, because it
	// does not close the step. The value is stamped whichever way this
	// came back, or a revise's evidence would reach the ledger with no
	// kind.
	if evidence.Value != nil {
		evidence.Value.Kind = kind
		evidence.Value.VerifiedBy = verifiedBy(kind, step)
		evidence.Value.VerifiedAt = timestamppb.New(step.now())
	}
	return evidence, err
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

// verifyCommit proves a commit the step made: the object resolves in the
// run's checkout, it is on the run's branch, it descends from the head the
// attempt started at without being that head, and the mill is its author and
// its committer.
//
// Every clause is load-bearing. An object that exists proves only that
// something was written — a commit made on a detached head, a commit fetched
// from elsewhere, an object a session created by hand — and the run's claim
// on the repository is its branch, so a SHA that is not on it is refused. The
// base is what makes the fact the step's own rather than the branch's: the
// head a commit step was handed was already there, and closing on it would
// close the step on work nobody did (docs/domain-model.md §Evidence). The
// identity is what stops a commit that arrived on the branch some other way
// — a fetch, a merge, a person pushing — from counting as the step's.
//
// The one exception is a head that has not moved this attempt. A revise can
// hand the step back a branch that already carries its earlier commit,
// because nothing since has changed it, and the step still closes — naming
// that commit — when three things hold: the working tree is clean, so
// nothing the session did is being dropped; the head is not, but does
// descend from, EarliestBase, the base of the step's first attempt this
// generation, so the fact stays the step's own and not merely the branch's;
// and the mill is still the head's author and committer. A tree that is
// dirty, or a head that is EarliestBase itself, or one that does not descend
// from it, is refused exactly as if nothing had moved at all.
//
// Each clause refuses in its own words, because "this commit is not the
// step's" is several different repairs, and which one it is is what a
// failed step's message has to say.
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
	base := strings.TrimSpace(step.Base)
	if base == "" {
		return refuse("no base commit was recorded for this attempt, so nothing says this commit is the step's", nil)
	}
	if !commitSHA.MatchString(base) {
		return refuse(fmt.Sprintf("the recorded base commit %q is not a git object name", base), nil)
	}
	if _, err := step.Git.Run(ctx, nil, "cat-file", "-e", sha+"^{commit}"); err != nil {
		return refuse(fmt.Sprintf("%s does not resolve to a commit in the checkout", sha), err)
	}
	if _, err := step.Git.Run(ctx, nil, "merge-base", "--is-ancestor", sha, step.Branch); err != nil {
		return refuse(fmt.Sprintf("%s is not on %s", sha, step.Branch), err)
	}
	if sha == base {
		// The head has not moved this attempt. That is still the step's work
		// when the tree is clean and the head already descends from the
		// step's earliest base — a revise that changed nothing on the
		// branch. Anything else refuses in the usual words.
		status, err := step.Git.Run(ctx, nil, "status", "--porcelain")
		if err != nil {
			return refuse("the checkout would not say whether the tree is clean", err)
		}
		if strings.TrimSpace(status) != "" {
			return refuse(fmt.Sprintf("the branch head is still %s: the session made no commit", sha), nil)
		}
		earliest := strings.TrimSpace(step.EarliestBase)
		if earliest == "" {
			earliest = base
		}
		if sha == earliest {
			return refuse(fmt.Sprintf("%s is the head the step started at: the session made no commit", sha), nil)
		}
		if _, err := step.Git.Run(ctx, nil, "merge-base", "--is-ancestor", earliest, sha); err != nil {
			return refuse(fmt.Sprintf("%s does not descend from %s, the earliest base of this step", sha, earliest), err)
		}
	} else if _, err := step.Git.Run(ctx, nil, "merge-base", "--is-ancestor", base, sha); err != nil {
		return refuse(fmt.Sprintf("%s does not descend from %s, the head the step started at", sha, base), err)
	}
	if err := verifyCommitter(ctx, step, sha); err != nil {
		return Evidence{}, err
	}
	return Evidence{Value: &millv1.Evidence{Value: &millv1.Evidence_Commit{Commit: sha}}}, nil
}

// verifyCommitter proves the mill made the commit. Both identities are read,
// because a commit authored by somebody else and committed by the mill is as
// much "not the work this step did" as the other way round, and the message
// names the identity git returned so that the repair is visible without
// opening the checkout.
func verifyCommitter(ctx context.Context, step Step, sha string) error {
	kind := millv1.EvidenceKind_EVIDENCE_KIND_COMMIT
	refuse := func(reason string, err error) error {
		return &NotVerifiedError{Kind: kind, Reason: reason, Err: err}
	}
	mill := step.Committer
	if mill.Name == "" || mill.Email == "" {
		return refuse("the mill has no identity to compare the commit against", nil)
	}
	// The unit separator is what splits the two identities, because no field
	// of a git identity can contain it.
	out, err := step.Git.Run(ctx, nil, "show", "--no-patch", "--format=%an <%ae>\x1f%cn <%ce>", sha)
	if err != nil {
		return refuse(fmt.Sprintf("%s: the checkout would not say who made it", sha), err)
	}
	author, committer, found := strings.Cut(strings.TrimSpace(out), "\x1f")
	if !found {
		return refuse(fmt.Sprintf("%s: the checkout would not say who made it", sha), nil)
	}
	want := mill.String()
	if committer != want {
		return refuse(fmt.Sprintf("%s was committed by %s, not by %s", sha, committer, want), nil)
	}
	if author != want {
		return refuse(fmt.Sprintf("%s was authored by %s, not by %s", sha, author, want), nil)
	}
	return nil
}

// verifyExit proves a check's exit: the process the workspace ran exited
// with a code the definition names. A process that never ran — status -1 —
// proves nothing at all, whatever the definition would have accepted.
//
// A preparatory step is the one exception, and it is the whole of the
// difference between the two kinds: its evidence is the record of how its
// command ended, whichever way it did, because a step that readies the
// machine never fails the run and the record is what says whether a finding
// was owed (docs/sandbox.md §"Preparatory steps"). A budget it spent is
// recorded as the reason its process has no status; that too is the record.
func verifyExit(_ context.Context, step Step) (Evidence, error) {
	kind := millv1.EvidenceKind_EVIDENCE_KIND_EXIT
	exit := step.Exit
	if exit == nil {
		return Evidence{}, &NotVerifiedError{Kind: kind, Reason: "no process ran"}
	}
	if step.Definition.GetKind() == millv1.StepKind_STEP_KIND_PREPARATORY {
		return Evidence{Value: &millv1.Evidence{Value: &millv1.Evidence_Exit{Exit: exit}}}, nil
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
// not a revise: a reviewer that asks for changes and names none has asked
// for nothing, so the output is refused as malformed — the same class of
// failure as output that does not parse at all — rather than being read as
// either answer. Sending the work back on it would spend an implementer's
// attempt on nothing, and closing the step on it would approve work a
// reviewer declined to approve.
//
// Every refusal here is [NotVerifiedError.Malformed]: the reviewer produced
// no verdict, so nothing was judged and there is nothing to send back.
func verifyVerdict(_ context.Context, step Step) (Evidence, error) {
	kind := millv1.EvidenceKind_EVIDENCE_KIND_VERDICT
	refuse := func(reason string, err error) (Evidence, error) {
		return Evidence{}, &NotVerifiedError{Kind: kind, Reason: reason, Err: err, Malformed: true}
	}
	if step.Session.Output == nil {
		return refuse("the session produced no structured output: a verdict carries a decision field, and no prose is read", nil)
	}
	if err := ValidateJSON(VerdictSchema, step.Session.Output); err != nil {
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

	result := Evidence{
		Value:    &millv1.Evidence{Value: &millv1.Evidence_Verdict{Verdict: verdict}},
		Warnings: warnings,
		Verdict:  verdict,
	}
	if verdict.GetDecision() == millv1.VerdictDecision_VERDICT_DECISION_REVISE {
		if blocking == 0 {
			return refuse("the verdict says revise and no finding has severity \"blocking\": "+
				"a revise carries the finding that blocks it", nil)
		}
		// A revise never closes the step, but it is still evidence: the
		// caller records it on the attempt it failed, so Inspect returns the
		// findings for a failed review the same way it returns an approved
		// one's (docs/domain-model.md §Evidence).
		return result, &NotVerifiedError{Kind: kind, Reason: fmt.Sprintf(
			"the reviewer asked for changes: %d blocking %s", blocking, plural(blocking, "finding", "findings"))}
	}
	return result, nil
}

// VerdictSchema is docs/domain-model.md §Verdict as a schema. The verdict is
// parsed, never read: this is the whole of what a reviewer's output is
// allowed to be. It is exported because the run loop hands it to a
// reviewer's session as that session's output schema, so a reviewer is asked
// for exactly what this package will accept.
const VerdictSchema = `{
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
		return refuse(failureReason(ctx, step.Forge, sha), nil)
	case forge.Pending:
		interval, limit := step.statusPolicy()
		if step.headWait(sha) >= limit {
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

// failureReason is what a failed status tells the implementer: the bare
// sentence naming the failed status, followed by one block per failing
// check naming its log — or, when a check's log could not be read, naming
// why — so the implementer fixes what the checks actually reported instead
// of guessing (docs/prd.md R-28, mill#95). A forge that cannot say more than
// "failed" — FailingChecks itself errors, or names no check — falls back to
// the bare sentence rather than inventing a message with no check to point
// at, which keeps this additive for the case nothing here changes.
func failureReason(ctx context.Context, f Forge, sha string) string {
	bare := fmt.Sprintf("the forge reports the checks on %s as failed", sha)
	checks, err := f.FailingChecks(ctx, sha)
	if err != nil || len(checks) == 0 {
		return bare
	}
	var b strings.Builder
	b.WriteString(bare)
	b.WriteString(":\n")
	for _, check := range checks {
		fmt.Fprintf(&b, "\n%s (%s)\n", check.Name, check.Conclusion)
		excerpt, err := f.CheckLog(ctx, check)
		switch {
		case err != nil:
			fmt.Fprintf(&b, "the log could not be read: %s\n", err)
		case excerpt == "":
			b.WriteString("the log named nothing that looked like a failure\n")
		default:
			b.WriteString(excerpt)
			b.WriteString("\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
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

// verifyArtifact proves an artifact: skift accepted something with this
// digest. Skift holding the bytes is the fact; a session naming a digest is
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
		return refuse(fmt.Sprintf("skift could not be asked about %s", digest), err)
	}
	if !accepted {
		return refuse(fmt.Sprintf("skift holds no artifact with digest %s", digest), nil)
	}
	return Evidence{Value: &millv1.Evidence{Value: &millv1.Evidence_Artifact{Artifact: digest}}}, nil
}

// verifyGate proves a gate: the gate node skift holds is `done` and its
// result is attached — or, for a gate the workflow's own `auto` policy
// closed, the policy that fired.
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
	if policy := strings.TrimSpace(step.AutoApproved); policy != "" {
		// The decision was the workflow's own, taken before anything was
		// filed. There is no node to read and there never was one; what
		// closes the step is the policy, recorded so the ledger says which.
		return Evidence{Value: &millv1.Evidence{Value: &millv1.Evidence_Gate{Gate: "auto:" + policy}}}, nil
	}
	if step.Gates == nil {
		return refuse("there is no skift to read the gate from", nil)
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
	if step.Definition.GetKind() == millv1.StepKind_STEP_KIND_PREPARATORY && step.Name != "" {
		return "preparatory " + step.Name
	}
	return "evidence." + kindName(kind)
}

// headWait is how long head has itself been waited on: now, minus the
// earliest StatusHistory entry that read that same head, or zero when this
// is the first read of it. A different head's history never counts against
// this one (mill#91).
func (s Step) headWait(head string) time.Duration {
	now := s.now()
	var earliest time.Time
	for _, read := range s.StatusHistory {
		if read.Head != head {
			continue
		}
		if earliest.IsZero() || read.At.Before(earliest) {
			earliest = read.At
		}
	}
	if earliest.IsZero() {
		return 0
	}
	return now.Sub(earliest)
}

func (s Step) now() time.Time {
	if s.Now == nil {
		return time.Now()
	}
	return s.Now()
}

// kindName is the evidence kind as the model spells it, which is also how
// [github.com/steading-ai/mill/internal/domain] spells it in the ledger.
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
