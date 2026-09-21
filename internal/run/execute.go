package run

import (
	"context"
	"crypto/sha1" //nolint:gosec // UUIDv5 is defined over SHA-1; this is a name, not a signature
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	harnessv1 "github.com/ivy/mill/gen/harness/v1"
	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/check"
	"github.com/ivy/mill/internal/evidence"
	"github.com/ivy/mill/internal/forge"
	"github.com/ivy/mill/internal/harness"
	"github.com/ivy/mill/internal/workflow"
	"github.com/ivy/mill/internal/workspace"
)

// result is what executing one attempt produced, before anything is
// verified. Nothing in it is evidence: the claim is the fact the mill
// observed and is asking to have verified, and the session's words are here
// only for the two verifiers that read structured output.
type result struct {
	// claim is what the mill observed: a SHA git returned, a URL the forge
	// returned, an artifact digest dagger accepted, a gate node's id.
	claim string
	// session is what the session produced.
	session evidence.Session
	// exit is how a process ended, for check and skill steps.
	exit *harnessv1.ProcessExit
	// usage is what the step billed, by model.
	usage harness.Usage
	// feedback is what a failure hands the next attempt as its prompt.
	feedback string
	// stderr is what a check step's command wrote to its diagnostics
	// stream, kept whether or not the definition hands it back as
	// feedback: the breaker's gate carries it either way.
	stderr string
	// judgment says the failure is a reviewer's `revise` verdict rather
	// than the step's own failure: the step did its job, and what it
	// produced implicates another step's work (docs/domain-model.md
	// §Verdict).
	judgment bool
	// autoApproved records a gate the `auto` predicate closed.
	autoApproved bool
	// output is what a preparatory step's command wrote, both streams,
	// which its evidence carries whole: the record of the step's run is
	// the fact the step closes on (docs/sandbox.md §"Preparatory steps").
	output string
}

// execute runs one attempt by kind.
func (r *Runner) execute(ctx context.Context, s *state, step *millv1.Step, sr *millv1.StepRun, detail *millv1.InspectResponse, token *millv1.StepToken, feedback string) (result, error) {
	stepCtx, cancel := stepContext(ctx, step)
	defer cancel()
	switch step.GetKind() {
	case millv1.StepKind_STEP_KIND_SKILL:
		made, err := r.commitAlreadyMade(stepCtx, s, step, sr)
		if err != nil {
			return result{}, err
		}
		if made != "" {
			r.printf("  the commit the interrupted attempt made is on the branch\n")
			return result{claim: made}, nil
		}
		return r.runSkill(stepCtx, s, step, sr, detail, token, feedback, "")
	case millv1.StepKind_STEP_KIND_CHECK:
		return r.runCheck(stepCtx, s, step, sr)
	case millv1.StepKind_STEP_KIND_GATE:
		return r.runGate(stepCtx, s, step, sr, detail)
	case millv1.StepKind_STEP_KIND_PREPARATORY:
		return r.runPreparatory(stepCtx, s, step, sr)
	default:
		return result{}, fmt.Errorf("run %s: step %s has no kind", s.run.GetId(), step.GetName())
	}
}

// stepContext is the step's own context: the run's, bounded by the step's
// timeout when it declares one. The cancel is the caller's to run.
func stepContext(ctx context.Context, step *millv1.Step) (context.Context, context.CancelFunc) {
	if timeout := step.GetTimeout().AsDuration(); timeout > 0 {
		return context.WithTimeout(ctx, timeout)
	}
	return context.WithCancel(ctx)
}

// runSkill drives a skill step as a session in a harness. The session is
// found by its deterministic id, so a step re-run after a crash resumes the
// same session and the model picks up its own prior context.
//
// brief is what the mill has to say to this step's session beyond its skill
// and the node: the assessment's list of the workflows in scope is the one
// caller that has any (internal/run/select.go). Every other step passes
// nothing.
func (r *Runner) runSkill(ctx context.Context, s *state, step *millv1.Step, sr *millv1.StepRun, detail *millv1.InspectResponse, token *millv1.StepToken, feedback, brief string) (result, error) {
	name, settings := harnessFor(step)
	adapter, err := s.harnesses.Lookup(name)
	if err != nil {
		return result{}, err
	}
	prompt, err := r.prompt(ctx, s, step, detail, feedback, brief)
	if err != nil {
		return result{}, err
	}
	handle := s.workspace.Handle()

	// Where a session's sidecar connects and works. For a sidecar that runs
	// inside a machine these are the machine-side paths — the socket's
	// in-machine mount and the checkout at `/work` — not the host paths the
	// worker serves and keeps the checkout at (docs/sandbox.md §Naming,
	// §"Session identity"). A workspace whose sessions run on the host does
	// not implement this and the handle's own socket and root stand.
	sessionSocket, sessionCwd := handle.Socket, handle.Root
	if inMachine, ok := s.workspace.(inMachineSession); ok {
		sessionSocket, sessionCwd = inMachine.SessionSocket(), inMachine.SessionCwd()
	}

	// The step's own credential is registered with the redactor before the
	// session is handed it, so a bearer that finds its way into a tool's
	// output cannot reach the log (docs/architecture.md §Observability).
	s.workspace.Redact(token.GetToken())

	session := harness.Session{
		ID: sr.GetSessionId(),
		// The step's options document, as the workflow wrote it. Nothing
		// here reads a key of it: the harness owns its own schema and
		// refuses what it does not know, so a knob mill has never heard of
		// still reaches the harness that has (docs/domain-model.md §Step).
		Options: settings.GetOptions().AsMap(),
		Workspace: harness.Workspace{
			Socket: sessionSocket,
			Token:  token.GetToken(),
			Env:    s.workspace.Environment(),
		},
		Cwd: sessionCwd,
		// The step's budget, which is also how long the session may wait
		// in the foreground for anything it runs.
		Timeout: step.GetTimeout().AsDuration(),
		Events:  r.sessionEvents(ctx, s, sr),
	}

	// The prompt heads the attempt's activity record. What a session was
	// asked is the first thing that happened to it, and reading it back off
	// the ledger is how a revise is shown to have carried a reviewer's
	// findings to the implementer without anyone reading the session
	// (docs/architecture.md §Observability).
	if prompt.Text != "" {
		r.recordActivity(s, sr, &millv1.Activity{
			Source: millv1.ActivitySource_ACTIVITY_SOURCE_MILL,
			Body:   &millv1.Activity_Prompt{Prompt: &millv1.Prompt{Text: prompt.Text}},
		})
	}

	// Start or resume is the ledger's answer, not the attempt number's: a
	// session is resumable once the harness has produced an event in it, and
	// an attempt that ended before it did left no conversation for a resume
	// to find (docs/ledger.md §Resume, the session-started rule).
	begin := adapter.Start
	if sessionStarted(detail, sr.GetSessionId()) {
		begin = adapter.Resume
	}
	turn, err := begin(ctx, session, prompt, nil)
	if err != nil {
		return result{}, err
	}

	// What the adapter gave the session's process, on the attempt, before
	// the session's own outcome is known: a step that asked for a variable
	// and a deny list that took one away are both decisions a reviewer
	// reads off the ledger (docs/architecture.md §"Skill steps: the harness
	// adapter").
	if err := r.recordEnvironment(ctx, s, sr, turn.Environment); err != nil {
		return result{}, err
	}

	terminal, err := turn.Wait(ctx)
	if err != nil {
		// The session was stopped: cancel it so nothing outlives the step,
		// then report what it billed before it went.
		//
		// The cancellation runs under the process's context rather than the
		// step's, which is already dead, because the adapter does not
		// return until the session's process group is gone and that is the
		// point of calling it. A worker killed rather than stopped — a
		// second signal inside its grace window — is the one thing that
		// cuts the wait short.
		_ = adapter.Cancel(s.processContext(), session)
		return result{}, err
	}

	// What the harness's own process had to say when it refused or crashed.
	// A sidecar that died before writing a terminal record left its stack
	// trace on its standard error, and this is the only place it is kept.
	if strings.TrimSpace(terminal.Diagnostics) != "" {
		r.recordActivity(s, sr, &millv1.Activity{
			Source: millv1.ActivitySource_ACTIVITY_SOURCE_MILL,
			Body:   &millv1.Activity_HarnessStderr{HarnessStderr: &millv1.HarnessStderr{Text: terminal.Diagnostics}},
		})
	}

	out := result{
		session: evidence.Session{
			FinalMessage: terminal.FinalMessage,
			Output:       terminal.StructuredOutput,
		},
		exit:  processExit(terminal.Exit),
		usage: terminal.Usage,
	}
	if terminal.Exit.Status != 0 {
		out.feedback = terminal.FinalMessage
		return out, fmt.Errorf("step %s: the session ended %s", step.GetName(), exitText(terminal.Exit))
	}
	out.claim, err = r.observe(ctx, s, step, sr, terminal)
	if err != nil {
		return out, err
	}
	return out, nil
}

// sessionEvents is the sink the harness reports a turn's events to: the
// ledger's record that the session has started, written once when the first
// event arrives, and then the attempt's activity record.
//
// The write is at the first event rather than at the end of the turn because
// that is when the fact becomes true. A worker killed mid-turn has left a
// conversation behind, and the attempt that follows it must resume that
// conversation rather than start a second one; a worker that dies before the
// first event has left nothing, and starting is the only thing that can
// work. Which of the two happened is not derivable from the attempt number,
// from the log directory, or from the harness's own transcript files: it is
// a fact the ledger holds (Invariant 10).
func (r *Runner) sessionEvents(ctx context.Context, s *state, sr *millv1.StepRun) harness.Sink {
	record := func(event *harnessv1.Event) { r.recordActivity(s, sr, activityFromEvent(event)) }
	if sr.GetSessionId() == "" {
		return record
	}
	var once sync.Once
	return func(event *harnessv1.Event) {
		if beat := event.GetHeartbeat(); beat != nil {
			r.recordHeartbeat(ctx, s, sr, beat)
		}
		once.Do(func() {
			// Not the step's context: a session that spoke and was then
			// cancelled has still spoken, and the ledger has to say so
			// before the next attempt asks how to begin.
			if _, err := call(context.WithoutCancel(ctx), r.options.Store.MarkSessionStarted, &millv1.MarkSessionStartedRequest{
				LeaseId:   s.lease,
				StepRunId: sr.GetId(),
			}); err != nil {
				r.options.Log.Warn("recording that the session started",
					"run", s.run.GetId(), "step_run", sr.GetId(), "error", err)
			}
		})
		record(event)
	}
}

// activityFromEvent translates one harness event into the activity body it
// carries. harness.v1.Event carries no block_id, delta or parent_call_id yet
// (deferred to activity-fidelity), so a Text event always arrives as one
// complete fragment: Final true, flushing at once rather than holding back.
func activityFromEvent(event *harnessv1.Event) *millv1.Activity {
	a := &millv1.Activity{Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS}
	switch body := event.GetBody().(type) {
	case *harnessv1.Event_Text:
		a.Body = &millv1.Activity_Text{Text: &millv1.Text{Text: body.Text.GetText(), Final: true}}
	case *harnessv1.Event_ToolCall:
		a.Body = &millv1.Activity_ToolCall{ToolCall: &millv1.ToolCall{
			CallId:    body.ToolCall.GetCallId(),
			Name:      body.ToolCall.GetName(),
			Arguments: body.ToolCall.GetArguments(),
		}}
	case *harnessv1.Event_ToolResult:
		a.Body = &millv1.Activity_ToolResult{ToolResult: &millv1.ToolResult{
			CallId:  body.ToolResult.GetCallId(),
			Content: body.ToolResult.GetContent(),
			IsError: body.ToolResult.GetIsError(),
		}}
	case *harnessv1.Event_Usage:
		a.Body = &millv1.Activity_Usage{Usage: body.Usage}
	case *harnessv1.Event_Heartbeat:
		a.Body = &millv1.Activity_Heartbeat{Heartbeat: body.Heartbeat}
	}
	return a
}

// recordActivity writes one record to the attempt's activity. A dropped
// record is not a failure of the step whose work it describes, so a write
// error is logged and nothing more — the same tolerance checkSink and
// verbSink below apply to their own records.
func (r *Runner) recordActivity(s *state, sr *millv1.StepRun, a *millv1.Activity) {
	if err := s.activity.Record(a); err != nil {
		r.options.Log.Warn("recording activity", "run", s.run.GetId(), "step_run", sr.GetId(), "error", err)
	}
}

// checkSink is the run package's [check.Sink]: every call becomes one
// activity record on the attempt's writer, source check, turn 0 — a check
// has no session and no turns of its own (docs/activity.md §Turns). It is
// used for both a check step (execute.go) and a preparatory step's own
// command (preparatory.go), the two callers of check.Run.
//
// A dropped record here is not a failure of the check: losing a mid-stream
// record is not a failure of the work it describes, the same tolerance
// recordActivity documents above.
type checkSink struct {
	activity *activityWriter
}

func (s checkSink) Started(command []string, contract *millv1.Check) {
	_ = s.activity.Record(&millv1.Activity{
		Source: millv1.ActivitySource_ACTIVITY_SOURCE_CHECK,
		Body: &millv1.Activity_CheckStarted{CheckStarted: &millv1.CheckStarted{
			Command:  command,
			Contract: contract,
		}},
	})
}

func (s checkSink) Output(stream string, data []byte) {
	_ = s.activity.Record(&millv1.Activity{
		Source: millv1.ActivitySource_ACTIVITY_SOURCE_CHECK,
		Body:   &millv1.Activity_CheckOutput{CheckOutput: &millv1.Output{Stream: stream, Data: data}},
	})
}

// verbSink is the run package's [workspace.Sink]: every call becomes one
// activity record on the attempt's writer, source workspace. It is set on
// the workspace for the length of an attempt (attachActivitySink) so that a
// verb the session or a checkout step reaches through the workspace — a git
// command chief among them — lands on the same ledger a check's output does
// (docs/activity.md §Verbs).
//
// A dropped record here is not a failure of the verb: losing a mid-stream
// record is not a failure of the work it describes, the same tolerance
// recordActivity documents above.
type verbSink struct {
	activity *activityWriter
}

func (s verbSink) Started(verb string, args map[string]string) {
	_ = s.activity.Record(&millv1.Activity{
		Source: millv1.ActivitySource_ACTIVITY_SOURCE_WORKSPACE,
		Body:   &millv1.Activity_VerbStarted{VerbStarted: &millv1.VerbStarted{Name: verb, Arguments: args}},
	})
}

func (s verbSink) Output(verb, stream string, data []byte) {
	_ = s.activity.Record(&millv1.Activity{
		Source: millv1.ActivitySource_ACTIVITY_SOURCE_WORKSPACE,
		Body:   &millv1.Activity_VerbOutput{VerbOutput: &millv1.Output{Stream: stream, Data: data}},
	})
}

// Ended flushes both streams' trailing partial lines before it records the
// verb's own terminal result, the same order runCheck keeps by hand for a
// check's output and result: the tail belongs to the record it completes,
// not the one that follows it.
func (s verbSink) Ended(verb string, err error) {
	_ = s.activity.EndVerbOutput("stdout")
	_ = s.activity.EndVerbOutput("stderr")
	var errText string
	if err != nil {
		errText = err.Error()
	}
	_ = s.activity.Record(&millv1.Activity{
		Source: millv1.ActivitySource_ACTIVITY_SOURCE_WORKSPACE,
		Body:   &millv1.Activity_Verb{Verb: &millv1.Verb{Name: verb, Error: errText}},
	})
}

var _ workspace.Sink = verbSink{}

// attachActivitySink points a workspace that records verbs (workspace.
// ActivitySink) at the attempt's own activity writer, so a verb the loop
// reaches through s.workspace during this attempt — a git command chief
// among them — lands on this attempt's ledger and not the next one's. A
// workspace with no verbs to record, the local backend's sessions among
// them, does not implement the interface and the call is a no-op.
func attachActivitySink(s *state) {
	if sink, ok := s.workspace.(workspace.ActivitySink); ok {
		sink.SetActivitySink(verbSink{activity: s.activity})
	}
}

// detachActivitySink clears the sink attachActivitySink installed, so a verb
// reached after the attempt's activity writer has closed — through a
// workspace the run loop keeps open across attempts — is never asked to
// write to it.
func detachActivitySink(s *state) {
	if sink, ok := s.workspace.(workspace.ActivitySink); ok {
		sink.SetActivitySink(nil)
	}
}

// openActivity starts step's activity writer for the attempt sr just began,
// attaches it as the workspace's verb sink, and records the attempt's
// opening frame.
func (r *Runner) openActivity(s *state, step *millv1.Step, sr *millv1.StepRun) error {
	activity, err := newActivityWriter(r.options.LogDir, s.run.GetId(), step.GetName(), sr.GetAttempt(), r.options.Redactor, r.options.Now, r.options.ActivityCeiling)
	if err != nil {
		return err
	}
	s.activity = activity
	attachActivitySink(s)
	return s.activity.Record(attemptStartedActivity(step, sr))
}

// reopenActivity reattaches step's already-written activity record for sr's
// attempt, for a run resuming a filed gate rather than starting a new
// attempt.
func (r *Runner) reopenActivity(s *state, step *millv1.Step, sr *millv1.StepRun) error {
	activity, err := reopenActivityWriter(r.options.LogDir, s.run.GetId(), step.GetName(), sr.GetAttempt(), r.options.Redactor, r.options.Now, r.options.ActivityCeiling)
	if err != nil {
		return err
	}
	s.activity = activity
	attachActivitySink(s)
	return nil
}

// abortActivity closes the attempt's activity record without recording an
// outcome — the attempt is being abandoned, not ended — ignoring a close
// error the way every abort call site already did.
func abortActivity(s *state) {
	detachActivitySink(s)
	_ = s.activity.Close()
	s.activity = nil
}

// endActivity records the attempt's outcome and closes its activity record.
// On error the attempt's sink stays attached and s.activity stays set,
// matching every end call site's existing behavior of returning before
// detaching.
func endActivity(s *state, status millv1.StepRunStatus, kind millv1.EvidenceKind, reason string) error {
	if err := s.activity.End(status, kind, reason); err != nil {
		return err
	}
	detachActivitySink(s)
	s.activity = nil
	return nil
}

// recordHeartbeat writes one heartbeat to the run's history: the harness
// asked a session that had ended its turn on a task that then went quiet
// whether the task was still needed (docs/domain-model.md §Session). It is a
// row of the audit trail and nothing else, so a write that fails is logged
// and the session goes on: losing the record of the ask must not fail the
// step the ask was about.
func (r *Runner) recordHeartbeat(ctx context.Context, s *state, sr *millv1.StepRun, beat *harnessv1.Heartbeat) {
	// Not the step's context: the last heartbeat before a breaker trips is
	// written while the step is being stopped, and it is the one that
	// explains the exit.
	if _, err := call(context.WithoutCancel(ctx), r.options.Store.RecordHeartbeat, &millv1.RecordHeartbeatRequest{
		LeaseId:        s.lease,
		RunId:          s.run.GetId(),
		StepRunId:      sr.GetId(),
		Strike:         beat.GetStrike(),
		Strikes:        beat.GetStrikes(),
		WaitingSeconds: beat.GetWaitingSeconds(),
		Tasks:          beat.GetTasks(),
	}); err != nil {
		r.options.Log.Warn("recording a session heartbeat",
			"run", s.run.GetId(), "step_run", sr.GetId(), "error", err)
	}
}

// sessionStarted reports whether any attempt the ledger holds has produced
// an event in a session. It is what says a session exists to be resumed:
// every attempt at every step is considered, because a session outlives the
// step that opened it and a workflow may run several steps in one.
func sessionStarted(detail *millv1.InspectResponse, sessionID string) bool {
	if sessionID == "" {
		return false
	}
	for _, d := range detail.GetSteps() {
		if d.GetStepRun().GetSessionId() == sessionID && d.GetStepRun().GetSessionStarted() {
			return true
		}
	}
	return false
}

// recordEnvironment writes what the harness gave the session's process. An
// adapter that decided nothing — a harness with no environment of its own to
// speak of — writes nothing.
func (r *Runner) recordEnvironment(ctx context.Context, s *state, sr *millv1.StepRun, env harness.Environment) error {
	if len(env.Passthrough) == 0 && len(env.Stripped) == 0 {
		return nil
	}
	_, err := call(ctx, r.options.Store.RecordStepEnvironment, &millv1.RecordStepEnvironmentRequest{
		LeaseId:        s.lease,
		StepRunId:      sr.GetId(),
		EnvPassthrough: env.Passthrough,
		EnvStripped:    env.Stripped,
	})
	return err
}

// observe is what the mill itself saw after a skill step, which is the only
// thing offered as a claim. Nothing the session said is consulted: a session
// that reports a commit it did not make reports it into a transcript nothing
// reads (docs/domain-model.md §Evidence).
func (r *Runner) observe(ctx context.Context, s *state, step *millv1.Step, sr *millv1.StepRun, terminal *harness.Terminal) (string, error) {
	switch step.GetEvidence() {
	case millv1.EvidenceKind_EVIDENCE_KIND_COMMIT:
		// The head of the run's branch after the session, read through the
		// workspace, offered whether or not it moved. Whether an unmoved
		// head is still the step's work — a revise that changed nothing, as
		// against a session that simply made no commit — is not this
		// function's to judge: it is what the verifier's seams (a clean
		// tree, the step's earliest base) are for.
		return r.head(ctx, s)

	case millv1.EvidenceKind_EVIDENCE_KIND_URL:
		pr, err := r.pullRequest(ctx, s)
		if err != nil {
			return "", err
		}
		return pr.URL, nil

	case millv1.EvidenceKind_EVIDENCE_KIND_ARTIFACT:
		// The session's last word, or the file the step names instead
		// (internal/run/artifact.go).
		body, err := r.artifactBody(ctx, s, step, terminal.FinalMessage)
		if err != nil {
			return "", err
		}
		return r.attachArtifact(ctx, s, step, sr, body)

	default:
		return "", nil
	}
}

// head is the run branch's current head, read through the workspace. It is
// empty for a checkout with no history at all.
func (r *Runner) head(ctx context.Context, s *state) (string, error) {
	if s.workspace == nil {
		return "", nil
	}
	commits, err := s.workspace.Git().Log(ctx, workspace.LogRequest{Limit: 1})
	if err != nil {
		return "", err
	}
	if len(commits) == 0 {
		return "", nil
	}
	return commits[0].SHA, nil
}

// baseCommit is the head the attempt's work starts from, recorded on the
// attempt before anything runs. It is what makes `commit` evidence a commit
// the step made, and what a rerun of a superseded span reads to know what
// that span produced.
//
// It is the branch head, except for an attempt that continues an interrupted
// one: a worker killed between the session's commit and the write that
// closes the step left its work on the branch, and an attempt that took the
// new head as its base would call that commit "already there" and ask for
// another. Inheriting the base of the aborted attempt keeps the span the
// same one, so the commit that was made still closes the step.
func (r *Runner) baseCommit(ctx context.Context, s *state, step *millv1.Step, detail *millv1.InspectResponse) (string, error) {
	if carried := abortedBase(detail, step.GetName()); carried != "" {
		return carried, nil
	}
	return r.head(ctx, s)
}

// abortedBase is the base of the latest attempt at a step, when that attempt
// was aborted. A failed or superseded attempt is not continued — its work
// was judged and sent back — so only an abort carries its base forward.
func abortedBase(detail *millv1.InspectResponse, step string) string {
	var latest *millv1.StepRun
	for _, d := range detail.GetSteps() {
		if sr := d.GetStepRun(); sr.GetStepName() == step {
			latest = sr
		}
	}
	if latest.GetStatus() != millv1.StepRunStatus_STEP_RUN_STATUS_ABORTED {
		return ""
	}
	return latest.GetBaseCommit()
}

// commitAlreadyMade is the commit an interrupted attempt at this step left on
// the branch, or empty when there is none.
//
// A `commit` step whose base is behind the branch head has its evidence
// already: the commit is on the branch, and the mill observed it. Running the
// session again would ask a model to redo work that is done, and the ledger's
// base plus the branch are enough to see that it is (docs/ledger.md §Resume).
// The claim still goes to the verifier like any other, so a head that moved
// under somebody else's identity closes nothing.
func (r *Runner) commitAlreadyMade(ctx context.Context, s *state, step *millv1.Step, sr *millv1.StepRun) (string, error) {
	if step.GetEvidence() != millv1.EvidenceKind_EVIDENCE_KIND_COMMIT || sr.GetBaseCommit() == "" {
		return "", nil
	}
	head, err := r.head(ctx, s)
	if err != nil {
		return "", err
	}
	if head == sr.GetBaseCommit() {
		return "", nil
	}
	return head, nil
}

// runCheck runs a check step through the workspace's chokepoint, and writes
// what the command said to the attempt's activity record.
//
// A check is a step run like any other, so `mill logs <run> --step
// clean-tree` has to answer with something: what the command wrote is the
// whole of what a check produces, and an operator asked to explain a failed
// check has nowhere else to read it.
func (r *Runner) runCheck(ctx context.Context, s *state, step *millv1.Step, sr *millv1.StepRun) (result, error) {
	res, err := check.Run(ctx, s.workspace, check.Request{
		Name:        step.GetName(),
		Check:       step.GetCheck(),
		WorkflowDir: r.options.WorkflowDir,
		Sink:        checkSink{activity: s.activity},
	})
	if err != nil {
		return result{}, err
	}
	_ = s.activity.EndCheckOutput("stdout")
	_ = s.activity.EndCheckOutput("stderr")
	r.recordActivity(s, sr, &millv1.Activity{
		Source: millv1.ActivitySource_ACTIVITY_SOURCE_CHECK,
		Body: &millv1.Activity_Check{Check: &millv1.CheckResult{
			Exit:   res.Exit,
			Reason: res.Reason,
		}},
	})
	out := result{exit: res.Exit, stderr: strings.TrimRight(res.Stderr, "\n")}
	if !res.Passed {
		out.feedback = check.Feedback(step.GetCheck(), res)
		return out, fmt.Errorf("step %s: %s", step.GetName(), res.Reason)
	}
	return out, nil
}

// harnessFor picks the harness a skill step runs in. A step's harness table
// is keyed by adapter name; O0 has one adapter, so the table has one entry
// and the first is the answer. A step with no table takes the adapter's own
// defaults under the registry's default name.
func harnessFor(step *millv1.Step) (string, *millv1.HarnessSettings) {
	for name, settings := range step.GetHarness() {
		return name, settings
	}
	return DefaultHarness, nil
}

// DefaultHarness is the adapter a step names none for.
const DefaultHarness = "claude"

// exitText renders a process exit for an error message.
func exitText(exit harness.ProcessExit) string {
	if exit.NeverRan() {
		return "without running: " + string(exit.Reason)
	}
	if exit.Reason != "" {
		return fmt.Sprintf("with status %d: %s", exit.Status, exit.Reason)
	}
	return "with status " + strconv.Itoa(int(exit.Status))
}

// SessionID is the deterministic id of one session in one run: UUIDv5 over
// the run's context id, its generation and the session's name, in the URL
// namespace.
//
// Deterministic is the whole point. A step interrupted and re-run finds the
// same session, so the model picks up its own prior context. The generation
// is in the name because a restart must not: `mill restart` opens the next
// generation precisely so that every session in it is one the harness has
// never seen, and resuming the abandoned run's transcripts would be the one
// thing restart exists to avoid (docs/domain-model.md §Session, §Restart).
func SessionID(contextID string, generation int64, name string) string {
	// RFC 9562 §5.5: the UUIDv5 name is the namespace's bytes followed by
	// the name's. The URL namespace is used because a context id and a
	// session name together are a name, not a DNS label.
	namespace := [16]byte{
		0x6b, 0xa7, 0xb8, 0x11, 0x9d, 0xad, 0x11, 0xd1,
		0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8,
	}
	key := contextID + "/" + strconv.FormatInt(generation, 10) + "/" + name
	sum := sha1.Sum(append(namespace[:], []byte(key)...)) //nolint:gosec // see above
	var id [16]byte
	copy(id[:], sum[:16])
	id[6] = (id[6] & 0x0f) | 0x50
	id[8] = (id[8] & 0x3f) | 0x80
	hexed := hex.EncodeToString(id[:])
	return hexed[0:8] + "-" + hexed[8:12] + "-" + hexed[12:16] + "-" + hexed[16:20] + "-" + hexed[20:32]
}

// processExit is a terminal's exit in the shape the ledger stores.
func processExit(exit harness.ProcessExit) *harnessv1.ProcessExit {
	return &harnessv1.ProcessExit{
		Status: exit.Status,
		Signal: exit.Signal,
		Reason: exit.Reason.Proto(),
	}
}

// digest is the content hash an artifact is named by.
func digest(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// prompt renders the step's prompt: the skill body verbatim, then the node
// and its inputs as attributed data, then whatever the mill itself has to
// tell this step's session, and last a check's feedback when this attempt is
// the revise for one.
func (r *Runner) prompt(ctx context.Context, s *state, step *millv1.Step, detail *millv1.InspectResponse, feedback, brief string) (harness.Prompt, error) {
	skill, err := r.skillFor(ctx, step)
	if err != nil {
		return harness.Prompt{}, err
	}
	inputs, err := r.options.Dagger.Inputs(ctx, s.node.GetReference())
	if err != nil {
		return harness.Prompt{}, err
	}
	node := workflow.Node{
		Ref:    s.node.GetReference(),
		Title:  s.node.GetTitle(),
		Body:   s.node.GetBody(),
		Author: s.node.GetAssignee(),
	}
	for _, input := range inputs {
		node.Inputs = append(node.Inputs, workflow.Input{
			Ref:    input.GetNode().GetReference(),
			Title:  input.GetNode().GetTitle(),
			Body:   input.GetResult().GetBody(),
			Author: input.GetNode().GetAssignee(),
		})
	}
	text := workflow.Prompt(skill, node)
	if brief != "" {
		text += "\n\n" + brief
	}
	artifact, err := r.artifactBrief(s, step)
	if err != nil {
		return harness.Prompt{}, err
	}
	if artifact != "" {
		text += "\n\n" + artifact
	}
	// What a reviewer is judging, and where (internal/run/verify.go).
	review, err := r.reviewBrief(ctx, s, step, detail)
	if err != nil {
		return harness.Prompt{}, err
	}
	if review != "" {
		text += "\n\n" + review
	}
	// What a reviewer that already judged this work once is told about
	// the revision it is looking at now (internal/run/verify.go).
	revision, err := r.revisionBrief(s, step, detail)
	if err != nil {
		return harness.Prompt{}, err
	}
	if revision != "" {
		text += "\n\n" + revision
	}
	if feedback != "" {
		// A revise's feedback is the mill's own words about a check or a
		// verdict, so it belongs with the instructions rather than in a
		// data block.
		text += "\n\n" + feedbackHeading + "\n\n" + feedback
	}
	return harness.Prompt{Text: text, OutputSchema: outputSchema(step)}, nil
}

// skillFor is the prompt a step runs: the skill the step pins, read from the
// store, unless a test seam replaced it. An override is loaded from a
// directory rather than the store, so what it runs is never a skill anything
// pushed; it is refused outside the acceptance suite before a node is
// claimed (internal/cli/run.go, docs/runbook.md).
func (r *Runner) skillFor(ctx context.Context, step *millv1.Step) (*millv1.Skill, error) {
	if skill, ok := r.options.SkillOverrides[step.GetName()]; ok {
		return skill, nil
	}
	res, err := call(ctx, r.options.Store.GetSkill, &millv1.GetSkillRequest{
		Name:   step.GetSkill().GetName(),
		Digest: step.GetSkill().GetDigest(),
	})
	if err != nil {
		return nil, err
	}
	return res.GetSkill(), nil
}

// outputSchema is the shape the session's structured output must take. A
// step says it for itself, except a `verdict` step: a verdict is mill's own
// record and not the workflow's — parsed, never read (docs/domain-model.md
// §Verdict) — so the schema the verifier will hold the output to is the
// schema the session is given, and a workflow cannot ask a reviewer for a
// shape the mill would then refuse.
func outputSchema(step *millv1.Step) string {
	if step.GetEvidence() == millv1.EvidenceKind_EVIDENCE_KIND_VERDICT && step.GetOutputSchema() == "" {
		return evidence.VerdictSchema
	}
	return step.GetOutputSchema()
}

// feedbackHeading introduces what the previous attempt failed on: the check
// or review that sent this step back, or — before the first session — what
// readying the machine found (internal/run/preparatory.go). One heading,
// because there is one path.
const feedbackHeading = "## What the last attempt failed on\n\nThis is the mill's own report of what failed before this attempt: the check or review that sent this step back, or a preparatory step that readied the machine. Fix it."

// pullRequest is the run branch's pull request, opened if the forge has
// none. It is the mill's own read of the forge, not the session's: a `url`
// step closes on what the forge returned when asked for the pull request by
// branch, never on a URL a session typed.
//
// The head the checkout is at travels with the request, so nothing is opened
// until the forge shows the branch there: the session's push and this call
// are two requests to a forge that indexes the first one in its own time
// (mill#46).
func (r *Runner) pullRequest(ctx context.Context, s *state) (forge.PullRequest, error) {
	head, err := r.head(ctx, s)
	if err != nil {
		return forge.PullRequest{}, err
	}
	return s.repo.Forge.OpenPR(ctx, forge.OpenPRRequest{
		Branch:      s.run.GetBranch(),
		Head:        head,
		Title:       s.node.GetTitle(),
		Description: s.node.GetBody(),
	})
}

// verifyStep timing: how long the current step has been waiting for a fact
// that has not settled, which the `status` verifier compares against the
// workflow's cap.
func elapsed(detail *millv1.InspectResponse, step string, now time.Time) time.Duration {
	var first time.Time
	for _, d := range detail.GetSteps() {
		if d.GetStepRun().GetStepName() != step {
			continue
		}
		started := d.GetStepRun().GetStartedAt().AsTime()
		if first.IsZero() || started.Before(first) {
			first = started
		}
	}
	if first.IsZero() {
		return 0
	}
	return now.Sub(first)
}
