package run

import (
	"context"
	"fmt"
	"strings"

	harnessv1 "github.com/ivy/mill/gen/harness/v1"
	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/check"
	"github.com/ivy/mill/internal/workspace"
)

// A preparatory step readies the machine for the sessions: the toolchain
// install, `mise trust /work && mise install`. It is a deterministic step
// whose exit and captured output are the evidence, whichever way it exited,
// under the workflow's `timeout`; its output is recorded and never shown to
// a session, and its failure never fails the run — the run continues into
// the first session with the failure attached as a finding, because the
// circuit breaker exists for work that cannot be done and not for a typo an
// agent can fix in one session (docs/sandbox.md §"Preparatory steps").
//
// Three consequences shape the code here.
//
// The step always closes. A non-zero exit, or a budget spent, is recorded
// as the step's evidence and not as a failed attempt, so no retry is
// consumed, `on_failure` is never consulted, and the breaker cannot trip on
// it. The finding is derived from that evidence when the first session is
// prompted (findings below), not carried in the worker's head, so a worker
// that dies between the step and the session delivers it anyway
// (docs/ledger.md Invariant 10).
//
// The finding goes the way a reviewer's `revise` findings already go: as the
// feedback the prompt appends under the same heading (internal/run/walk.go,
// internal/run/execute.go's prompt). There is one delivery path, and
// provisioning's own findings — a derived build that fell back to base, an
// invalid `mill.toml` — take it too, in the same shape.
//
// A preparatory step runs only in a workspace that provisions a machine
// (workspace.Provisioned). The local backend is the operator's own host: a
// step written for the machine — `mise trust /work` — has nothing to ready
// there and would act on the host's own configuration instead, so the step
// closes on the record that it did not run, and that record is not a
// finding.

// findingLines is how much of a failed preparatory step's output the first
// session is given: the tail, where a tool says what went wrong.
const findingLines = 20

// runPreparatory runs the step's command through the workspace and records
// its exit and both streams as the result. A command that failed is not an
// error here: the result says how it ended, and the step closes on that.
func (r *Runner) runPreparatory(ctx context.Context, s *state, step *millv1.Step, sr *millv1.StepRun) (result, error) {
	if _, provisioned := s.workspace.(workspace.Provisioned); !provisioned {
		r.printf("  not run: this workspace provisions no machine to ready\n")
		return result{
			exit:   &harnessv1.ProcessExit{Status: -1, Reason: harnessv1.ExitReason_EXIT_REASON_REFUSED},
			output: notRunOutput,
		}, nil
	}
	res, err := check.Run(ctx, s.workspace, check.Request{
		Name:        step.GetName(),
		WorkflowDir: r.options.WorkflowDir,
		// The command alone is the step's; both streams are kept whole,
		// because the record of the run is the evidence.
		Check: &millv1.Check{
			Command: step.GetCheck().GetCommand(),
			Stdout:  millv1.StdoutShape_STDOUT_SHAPE_TEXT,
		},
	})
	if err != nil {
		return result{}, err
	}
	logCheck(r.options.LogDir, s.run.GetId(), step.GetName(), sr.GetAttempt(),
		r.options.Redactor, r.options.Now, res)
	out := result{
		exit:   res.Exit,
		stderr: strings.TrimRight(res.Stderr, "\n"),
		output: res.Stdout + res.Stderr,
	}
	if finding := preparatoryFinding(step, res.Exit, out.output); finding != "" {
		r.printf("  %s\n", firstLine(finding))
	}
	return out, nil
}

// notRunOutput is the evidence of a preparatory step in a workspace with no
// machine to ready.
const notRunOutput = "not run: this workspace provisions no machine to ready (docs/sandbox.md §\"Preparatory steps\")"

// preparatoryFinding is what a preparatory step's evidence owes the first
// session: nothing when the command exited zero or never ran because there
// was nothing to ready, and otherwise how it ended — its status, or the
// budget it spent — with the last lines of its output.
func preparatoryFinding(step *millv1.Step, exit *harnessv1.ProcessExit, output string) string {
	var ended string
	switch {
	case exit == nil:
		return ""
	case exit.GetStatus() == 0:
		return ""
	case exit.GetStatus() < 0 && exit.GetReason() == harnessv1.ExitReason_EXIT_REASON_TIMEOUT:
		ended = fmt.Sprintf("was stopped at its %s budget", step.GetTimeout().AsDuration())
	case exit.GetStatus() < 0:
		return ""
	default:
		ended = fmt.Sprintf("exited %d", exit.GetStatus())
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Preparatory step `%s` (`%s`) %s.", step.GetName(),
		strings.Join(step.GetCheck().GetCommand(), " "), ended)
	if tail := lastLines(output, findingLines); tail != "" {
		fmt.Fprintf(&b, " The last lines of its output:\n\n```\n%s\n```", tail)
	} else {
		b.WriteString(" It wrote nothing.")
	}
	return b.String()
}

// findings is everything owed to the run's first session before it is
// prompted: what provisioning found, then what each preparatory step's
// evidence says, in workflow order. It is read off the ledger and the
// workspace on every call rather than remembered, so a resumed run delivers
// the same findings the interrupted one would have.
func (r *Runner) findings(s *state, detail *millv1.InspectResponse) string {
	var found []string
	if provisioned, ok := s.workspace.(workspace.Provisioned); ok {
		for _, finding := range provisioned.Findings() {
			found = append(found, "Provisioning: "+finding)
		}
	}
	for _, step := range s.workflow.GetSteps() {
		if step.GetKind() != millv1.StepKind_STEP_KIND_PREPARATORY {
			continue
		}
		evidence := latestEvidence(detail, step.GetName())
		if evidence == nil {
			continue
		}
		if finding := preparatoryFinding(step, evidence.GetExit(), evidence.GetOutput()); finding != "" {
			found = append(found, finding)
		}
	}
	return strings.Join(found, "\n\n")
}

// latestEvidence is the evidence of the latest closed attempt at a step, or
// nil when no attempt has closed. A superseded attempt's evidence closes
// nothing and is not consulted.
func latestEvidence(detail *millv1.InspectResponse, step string) *millv1.Evidence {
	var latest *millv1.Evidence
	for _, d := range detail.GetSteps() {
		sr := d.GetStepRun()
		if sr.GetStepName() != step || sr.GetStatus() != millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED {
			continue
		}
		latest = d.GetEvidence()
	}
	return latest
}

// anySessionStarted reports whether any attempt the ledger holds has
// produced an event in a session: the first session has begun, and the
// findings owed to it have been delivered.
func anySessionStarted(detail *millv1.InspectResponse) bool {
	for _, d := range detail.GetSteps() {
		if d.GetStepRun().GetSessionStarted() {
			return true
		}
	}
	return false
}

// withFindings puts the run's findings before whatever feedback the step is
// already owed, so a session sent back by a check reads the check's words
// last, where the retry prompt puts them.
func withFindings(findings, feedback string) string {
	switch {
	case findings == "":
		return feedback
	case feedback == "":
		return findings
	default:
		return findings + "\n\n" + feedback
	}
}

// recordInstalls drains what the shim reported over the socket during the
// attempt into the run's provenance, one row per report. A workspace that
// takes no reports — the local backend — has nothing to drain.
func (r *Runner) recordInstalls(ctx context.Context, s *state, sr *millv1.StepRun) error {
	reporter, ok := s.workspace.(workspace.Reporter)
	if !ok {
		return nil
	}
	for _, install := range reporter.Installs() {
		if _, err := call(ctx, r.options.Store.RecordInstall, &millv1.RecordInstallRequest{
			LeaseId:   s.lease,
			RunId:     s.run.GetId(),
			StepRunId: sr.GetId(),
			Packages:  install.Packages,
		}); err != nil {
			return err
		}
		r.printf("  installed %s, not declared in mill.toml\n", strings.Join(install.Packages, ", "))
	}
	return nil
}

// lastLines is the tail of a text: its last n lines, trailing blank lines
// dropped.
func lastLines(text string, n int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// firstLine is a text's first line, for a progress line.
func firstLine(text string) string {
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		return text[:i]
	}
	return text
}
