package workflow

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	millv1 "github.com/ivy/mill/gen/mill/v1"
)

// Validate refuses a definition the design cannot execute. Each rule is one
// the rest of the mill relies on holding, checked once at load so that a
// broken workflow fails at push and never mid-run:
//
//   - Step names are unique. The ledger keys a step run by name; two steps
//     sharing one would make current_step ambiguous.
//   - A step is what its kind says: a skill step names a skill and a
//     session, a check step a command, a gate step an ask.
//   - Every step names the evidence that closes it. Evidence closes steps
//     and agents do not (docs/core-principles.md §8), so a step with no
//     evidence kind could never close.
//   - `revise` targets an earlier step. A revise supersedes every step from
//     the target through the failing one, so a forward target would move
//     current_step forward and skip work rather than redo it
//     (docs/domain-model.md §Revise).
//   - A gate that follows a write is preceded by a push. A gate releases
//     the lease and may wait for as long as a human takes; the worker that
//     resumes may be on another host and can only recreate the workspace
//     from a branch the forge has (docs/domain-model.md §"Gates and
//     pushes"). A step whose artifact evidence is read from a file it wrote
//     is not such a write: that file is on the node.
//   - An artifact source names a file in the checkout, on a step that
//     closes on `artifact`.
func Validate(w *millv1.Workflow) error {
	var errs []error
	index := map[string]int{}
	for position, step := range w.GetSteps() {
		if step.GetName() == "" {
			errs = append(errs, fmt.Errorf("step %d: name is required", position+1))
			continue
		}
		if _, duplicate := index[step.GetName()]; duplicate {
			errs = append(errs, fmt.Errorf("step %s: the name is already taken by an earlier step", step.GetName()))
			continue
		}
		index[step.GetName()] = position
	}
	for _, step := range w.GetSteps() {
		errs = append(errs, validateStep(step)...)
	}
	errs = append(errs, validateRevise(w, index)...)
	errs = append(errs, validateGates(w)...)
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("workflow %s: %w", w.GetName(), err)
	}
	return nil
}

func validateStep(step *millv1.Step) []error {
	var errs []error
	name := step.GetName()
	if step.GetEvidence() == millv1.EvidenceKind_EVIDENCE_KIND_UNSPECIFIED {
		errs = append(errs, fmt.Errorf("step %s: evidence is required; nothing else closes a step", name))
	}
	errs = append(errs, validateArtifact(step)...)
	switch step.GetKind() {
	case millv1.StepKind_STEP_KIND_SKILL:
		if step.GetSkill().GetName() == "" {
			errs = append(errs, fmt.Errorf("step %s: a skill step names a skill", name))
		}
		if step.GetSession() == "" {
			errs = append(errs, fmt.Errorf("step %s: a skill step names the session it runs in", name))
		}
	case millv1.StepKind_STEP_KIND_CHECK:
		if len(step.GetCheck().GetCommand()) == 0 {
			errs = append(errs, fmt.Errorf("step %s: a check step names a command", name))
		}
	case millv1.StepKind_STEP_KIND_GATE:
		if step.GetGate().GetAsk().GetTitle() == "" {
			errs = append(errs, fmt.Errorf("step %s: a gate step names what it asks", name))
		}
	default:
		errs = append(errs, fmt.Errorf("step %s: kind is required: skill, check, or gate", name))
	}
	return errs
}

// validateArtifact checks a step's artifact source: it says where `artifact`
// evidence is read from, so it means nothing on a step that closes on
// anything else, and the file it names is one in the checkout.
func validateArtifact(step *millv1.Step) []error {
	source := step.GetArtifact()
	if source == nil {
		return nil
	}
	name := step.GetName()
	var errs []error
	if step.GetEvidence() != millv1.EvidenceKind_EVIDENCE_KIND_ARTIFACT {
		errs = append(errs, fmt.Errorf("step %s: artifact names where artifact evidence is read from, and this step closes on %s",
			name, strings.ToLower(strings.TrimPrefix(step.GetEvidence().String(), "EVIDENCE_KIND_"))))
	}
	path := source.GetPath()
	switch {
	case path == "":
		errs = append(errs, fmt.Errorf("step %s: artifact names no path", name))
	case filepath.IsAbs(path):
		errs = append(errs, fmt.Errorf("step %s: artifact path %q is absolute; it is a file in the checkout", name, path))
	case slices.Contains(strings.Split(filepath.ToSlash(path), "/"), ".."):
		errs = append(errs, fmt.Errorf("step %s: artifact path %q climbs out of the checkout", name, path))
	}
	return errs
}

// validateRevise checks every on_failure target is a step earlier in the
// workflow, naming both steps so the error says what to move.
func validateRevise(w *millv1.Workflow, index map[string]int) []error {
	var errs []error
	for position, step := range w.GetSteps() {
		if step.GetOnFailure().GetKind() != millv1.OnFailureKind_ON_FAILURE_KIND_REVISE {
			continue
		}
		target := step.GetOnFailure().GetReviseTarget()
		at, known := index[target]
		switch {
		case !known:
			errs = append(errs, fmt.Errorf("step %s: revise targets %s, which is not a step in this workflow", step.GetName(), target))
		case at >= position:
			errs = append(errs, fmt.Errorf("step %s: revise targets %s, which is not earlier in the workflow", step.GetName(), target))
		}
	}
	return errs
}

// validateGates checks that no gate can strand a run's work on one host:
// once a step has written, a gate may only follow a step that put the
// branch on the forge, which is a step whose evidence is a URL.
func validateGates(w *millv1.Workflow) []error {
	var errs []error
	written, pushed := false, false
	for _, step := range w.GetSteps() {
		if step.GetKind() == millv1.StepKind_STEP_KIND_GATE {
			if written && !pushed {
				errs = append(errs, fmt.Errorf(
					"step %s: a gate after a write step must follow a step whose evidence is url, so the branch is on the forge before the run waits",
					step.GetName()))
			}
			continue
		}
		// A step whose evidence is a file it writes is not a write the
		// forge has to hold: the file's content is attached to the node as
		// the step's artifact, so a worker that recreates the workspace
		// elsewhere can put it back from the run's own evidence. This is
		// what lets `feature` gate a plan that lives in the checkout
		// (docs/domain-model.md §"Gates and pushes").
		if step.GetArtifact().GetPath() != "" &&
			step.GetEvidence() == millv1.EvidenceKind_EVIDENCE_KIND_ARTIFACT {
			continue
		}
		// A step that writes invalidates an earlier push: what is on the
		// forge is no longer what the workspace holds.
		if grants(step, millv1.Capability_CAPABILITY_WRITE) {
			written, pushed = true, false
		}
		if step.GetEvidence() == millv1.EvidenceKind_EVIDENCE_KIND_URL {
			pushed = true
		}
	}
	return errs
}

func grants(step *millv1.Step, capability millv1.Capability) bool {
	return slices.Contains(step.GetCapabilities(), capability)
}
