package run

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	harnessv1 "github.com/ivy/mill/gen/harness/v1"
	"github.com/ivy/mill/internal/check"
	"github.com/ivy/mill/internal/workspace/redact"
)

// The step log is what `mill logs` reads: one file per step run, written
// through the redactor as the session's events arrive
// (docs/architecture.md §Observability). Full tool output is never written;
// a summary line per tool result is, exactly as the workspace socket's own
// log does it.
//
// The file is named for the run, the step and the attempt, because an
// attempt is the unit the ledger closes and a re-run of a step must not
// overwrite what the attempt before it said.

// logSummaryLimit is how much of a text fragment or a tool result reaches
// the log. A transcript is not the ledger's business and a log an operator
// tails is not a place to put one.
const logSummaryLimit = 240

// StepLogPath is where one attempt's log lives under dir. The CLI builds the
// same path from the ledger's answer, so the writer and the reader agree
// without either storing it.
func StepLogPath(dir, runID, step string, attempt int64) string {
	return filepath.Join(dir, runID, fmt.Sprintf("%s.%d.log", step, attempt))
}

// openStepLog creates the file one attempt's events are written to. An empty
// dir means no log is kept, which is what a spec that does not care asks
// for; the returned writer is then nil and the sink discards.
func openStepLog(dir, runID, step string, attempt int64) (io.WriteCloser, error) {
	if dir == "" {
		return nil, nil
	}
	path := StepLogPath(dir, runID, step, attempt)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("step log: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return nil, fmt.Errorf("step log: %w", err)
	}
	return file, nil
}

// logPrompt writes the prompt an attempt was given at the head of its log,
// through the same redactor its events go through. It is written whole and
// not summarised: a prompt is what the mill decided to say, not what a model
// produced, and the questions asked of it afterwards — did the reviewer's
// findings reach the implementer, did the diff go in — are questions about
// its exact text.
func logPrompt(w io.Writer, redactor *redact.Redactor, now func() time.Time, prompt string) {
	if w == nil || prompt == "" {
		return
	}
	out := redactor.Writer(w)
	stamp := now().UTC().Format(time.RFC3339)
	_, _ = fmt.Fprintf(out, "%s prompt begin bytes=%d\n", stamp, len(prompt))
	_, _ = fmt.Fprintln(out, strings.TrimRight(prompt, "\n"))
	_, _ = fmt.Fprintf(out, "%s prompt end\n", stamp)
}

// logDiagnostics writes what the harness's own process said for itself when
// a session refused or crashed: the sidecar's standard error, whole, under a
// banner that says whose words these are. It goes through the redactor like
// everything else, because a crash dumps whatever was in scope.
func logDiagnostics(w io.Writer, redactor *redact.Redactor, now func() time.Time, diagnostics string) {
	if w == nil || strings.TrimSpace(diagnostics) == "" {
		return
	}
	out := redactor.Writer(w)
	stamp := now().UTC().Format(time.RFC3339)
	_, _ = fmt.Fprintf(out, "%s harness stderr begin bytes=%d\n", stamp, len(diagnostics))
	_, _ = fmt.Fprintln(out, strings.TrimRight(diagnostics, "\n"))
	_, _ = fmt.Fprintf(out, "%s harness stderr end\n", stamp)
}

// logCheck writes a check step's own log: the command's two streams, as the
// step run's attempt produced them.
//
// A check has no session and no events, so nothing else would write the file
// and `mill logs --step <check>` would answer "no log" for a step that
// certainly ran. Stdout is written when the contract kept it — a `none`
// contract drops it before it gets here — and stderr always, because stderr
// is where a check says why it failed.
//
// A failure to open or write the log is not a failure of the check: the
// check's result is a fact about the work, and losing the record of it must
// not turn a passing check into a failing step. It is dropped, as the log
// sink's own writes are.
func logCheck(dir, runID, step string, attempt int64, redactor *redact.Redactor, now func() time.Time, res check.Result) {
	file, err := openStepLog(dir, runID, step, attempt)
	if err != nil || file == nil {
		return
	}
	defer func() { _ = file.Close() }()

	out := redactor.Writer(file)
	stamp := now().UTC().Format(time.RFC3339)
	_, _ = fmt.Fprintf(out, "%s check %s exit status=%d\n", stamp, step, res.Exit.GetStatus())
	for _, stream := range []struct {
		name string
		body string
	}{{"stdout", res.Stdout}, {"stderr", res.Stderr}} {
		if strings.TrimSpace(stream.body) == "" {
			continue
		}
		_, _ = fmt.Fprintf(out, "%s check %s %s begin bytes=%d\n", stamp, step, stream.name, len(stream.body))
		_, _ = fmt.Fprintln(out, strings.TrimRight(stream.body, "\n"))
		_, _ = fmt.Fprintf(out, "%s check %s %s end\n", stamp, step, stream.name)
	}
	if res.Reason != "" {
		_, _ = fmt.Fprintf(out, "%s check %s failed: %s\n", stamp, step, res.Reason)
	}
}

// logSink renders a turn's events into the step log, through the redactor.
// The redactor is the workspace's own, so a value a session registered with
// the `redact` verb mid-step is scrubbed from every line written after it.
func logSink(w io.Writer, redactor *redact.Redactor, now func() time.Time) func(*harnessv1.Event) {
	if w == nil {
		return nil
	}
	out := redactor.Writer(w)
	return func(event *harnessv1.Event) {
		line := eventLine(event)
		if line == "" {
			return
		}
		_, _ = fmt.Fprintf(out, "%s %s\n", now().UTC().Format(time.RFC3339), line)
	}
}

// eventLine is one event as a line of the log: what happened, summarised.
func eventLine(event *harnessv1.Event) string {
	switch body := event.GetBody().(type) {
	case *harnessv1.Event_Text:
		return "text " + summarize(body.Text.GetText())
	case *harnessv1.Event_ToolCall:
		return fmt.Sprintf("tool %s call=%s", body.ToolCall.GetName(), body.ToolCall.GetCallId())
	case *harnessv1.Event_ToolResult:
		status := "ok"
		if body.ToolResult.GetIsError() {
			status = "error"
		}
		return fmt.Sprintf("result call=%s %s out=%s", body.ToolResult.GetCallId(), status, summarize(body.ToolResult.GetContent()))
	case *harnessv1.Event_Usage:
		models := make([]string, 0, len(body.Usage.GetByModel()))
		for model, counts := range body.Usage.GetByModel() {
			models = append(models, fmt.Sprintf("%s in=%d out=%d", model, counts.GetInput(), counts.GetOutput()))
		}
		return "usage " + strings.Join(models, " ")
	default:
		return ""
	}
}

// summarize flattens a fragment onto one line and truncates it, so the log
// stays a log.
func summarize(text string) string {
	flat := strings.Join(strings.Fields(text), " ")
	if len(flat) > logSummaryLimit {
		return flat[:logSummaryLimit] + "…"
	}
	return flat
}
