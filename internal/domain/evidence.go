package domain

import (
	"fmt"
	"net/url"
	"regexp"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
)

// EvidenceError is evidence the boundary refuses: a value that is not the
// shape its kind defines, or a kind the closing step does not name. Nothing
// is written, so the ledger never holds a fact it cannot read back.
type EvidenceError struct {
	// Kind is the kind the evidence claims to be.
	Kind millv1.EvidenceKind
	// Reason says what is wrong with it.
	Reason string
}

func (e *EvidenceError) Error() string {
	return fmt.Sprintf("evidence of kind %s: %s", EvidenceKindName(e.Kind), e.Reason)
}

// commitSHA is a git object name, which is what `commit` evidence is: forty
// hex digits under SHA-1, sixty-four under SHA-256.
var commitSHA = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// ValidateEvidence checks that evidence carries the value its kind defines.
// The kind and the value are two fields of one message — the kind is stored
// beside the value so a row reads without unpacking it — so a request can
// disagree with itself, and this is where that is caught: before the store
// is touched, so a rejected close leaves no evidence row and no closed
// attempt behind.
//
// docs/domain-model.md §Evidence is the table of what each kind proves.
func ValidateEvidence(evidence *millv1.Evidence) error {
	if evidence == nil {
		return &EvidenceError{Reason: "no evidence: a step run is closed by evidence or not at all"}
	}
	kind := evidence.GetKind()
	if kind == millv1.EvidenceKind_EVIDENCE_KIND_UNSPECIFIED {
		return &EvidenceError{Kind: kind, Reason: "no kind"}
	}
	if evidence.GetValue() == nil {
		return &EvidenceError{Kind: kind, Reason: "no value"}
	}
	if got := kindOfValue(evidence); got != kind {
		return &EvidenceError{Kind: kind, Reason: fmt.Sprintf(
			"value is %s, not %s", EvidenceKindName(got), EvidenceKindName(kind))}
	}
	return validateValue(evidence)
}

// kindOfValue is the kind the value set on evidence belongs to, whatever the
// kind field says.
func kindOfValue(evidence *millv1.Evidence) millv1.EvidenceKind {
	switch evidence.GetValue().(type) {
	case *millv1.Evidence_Commit:
		return millv1.EvidenceKind_EVIDENCE_KIND_COMMIT
	case *millv1.Evidence_Exit:
		return millv1.EvidenceKind_EVIDENCE_KIND_EXIT
	case *millv1.Evidence_Verdict:
		return millv1.EvidenceKind_EVIDENCE_KIND_VERDICT
	case *millv1.Evidence_Structured:
		return millv1.EvidenceKind_EVIDENCE_KIND_STRUCTURED
	case *millv1.Evidence_Url:
		return millv1.EvidenceKind_EVIDENCE_KIND_URL
	case *millv1.Evidence_Status:
		return millv1.EvidenceKind_EVIDENCE_KIND_STATUS
	case *millv1.Evidence_Artifact:
		return millv1.EvidenceKind_EVIDENCE_KIND_ARTIFACT
	case *millv1.Evidence_Gate:
		return millv1.EvidenceKind_EVIDENCE_KIND_GATE
	default:
		return millv1.EvidenceKind_EVIDENCE_KIND_UNSPECIFIED
	}
}

// validateValue checks what the value itself has to be for the kind to mean
// anything: a commit that is an object name, a URL that is a URL, a verdict
// that approves. A kind whose value is any non-empty string is checked for
// that alone.
func validateValue(evidence *millv1.Evidence) error {
	kind := evidence.GetKind()
	fail := func(reason string) error { return &EvidenceError{Kind: kind, Reason: reason} }
	switch kind {
	case millv1.EvidenceKind_EVIDENCE_KIND_COMMIT:
		if !commitSHA.MatchString(evidence.GetCommit()) {
			return fail(fmt.Sprintf("%q is not a git object name", evidence.GetCommit()))
		}
	case millv1.EvidenceKind_EVIDENCE_KIND_VERDICT:
		// A verdict closes a review step only when it approves. A revise
		// fails the step and returns to its target; it never closes one.
		if decision := evidence.GetVerdict().GetDecision(); decision != millv1.VerdictDecision_VERDICT_DECISION_APPROVE {
			return fail("a verdict closes a step only when its decision is approve")
		}
	case millv1.EvidenceKind_EVIDENCE_KIND_URL:
		parsed, err := url.Parse(evidence.GetUrl())
		if err != nil || !parsed.IsAbs() || parsed.Host == "" {
			return fail(fmt.Sprintf("%q is not an absolute URL", evidence.GetUrl()))
		}
	case millv1.EvidenceKind_EVIDENCE_KIND_STRUCTURED:
		if len(evidence.GetStructured().GetFields()) == 0 {
			return fail("the structured output is empty")
		}
	case millv1.EvidenceKind_EVIDENCE_KIND_EXIT:
		if evidence.GetExit() == nil {
			return fail("no process exit")
		}
	case millv1.EvidenceKind_EVIDENCE_KIND_STATUS:
		if evidence.GetStatus() == "" {
			return fail("no status")
		}
	case millv1.EvidenceKind_EVIDENCE_KIND_ARTIFACT:
		if evidence.GetArtifact() == "" {
			return fail("no artifact digest")
		}
	case millv1.EvidenceKind_EVIDENCE_KIND_GATE:
		if evidence.GetGate() == "" {
			return fail("no gate node id")
		}
	case millv1.EvidenceKind_EVIDENCE_KIND_UNSPECIFIED:
		return fail("no kind")
	}
	return nil
}

// checkDeclaredKind refuses evidence of a kind the step definition does not
// name. A step definition names one evidence kind (docs/domain-model.md
// §Evidence), so evidence of another kind closes nothing whatever it proves.
// A run that is still selecting has no definition to check against.
//
// A gate step has one more kind it may close on, whatever it declares:
// `gate` itself. "A gate step produces no evidence of its own; it closes
// when the gate node completes, and the gate's result is the evidence"
// (docs/domain-model.md §Step) — so a gate a human answered closes the step
// on the answer, while the same step auto-approved closes on the fact its
// definition names, which is what the approval authorized.
func checkDeclaredKind(workflow *millv1.Workflow, stepName string, evidence *millv1.Evidence) error {
	for _, step := range workflow.GetSteps() {
		if step.GetName() != stepName {
			continue
		}
		declared := step.GetEvidence()
		if declared == millv1.EvidenceKind_EVIDENCE_KIND_UNSPECIFIED || declared == evidence.GetKind() {
			return nil
		}
		if step.GetKind() == millv1.StepKind_STEP_KIND_GATE &&
			evidence.GetKind() == millv1.EvidenceKind_EVIDENCE_KIND_GATE {
			return nil
		}
		return &EvidenceError{Kind: evidence.GetKind(), Reason: fmt.Sprintf(
			"step %q is closed by %s evidence", stepName, EvidenceKindName(declared))}
	}
	return nil
}
