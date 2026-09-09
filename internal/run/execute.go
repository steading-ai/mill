package run

import (
	"context"
	"crypto/sha1" //nolint:gosec // UUIDv5 is defined over SHA-1; this is a name, not a signature
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
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
	// feedback is what a failure hands the revise target as its prompt.
	feedback string
	// autoApproved records a gate the `auto` predicate closed.
	autoApproved bool
}

// execute runs one attempt by kind.
func (r *Runner) execute(ctx context.Context, s *state, step *millv1.Step, sr *millv1.StepRun, detail *millv1.InspectResponse, token *millv1.StepToken, feedback string) (result, error) {
	stepCtx := ctx
	if timeout := step.GetTimeout().AsDuration(); timeout > 0 {
		var cancel context.CancelFunc
		stepCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	switch step.GetKind() {
	case millv1.StepKind_STEP_KIND_SKILL:
		return r.runSkill(stepCtx, s, step, sr, token, feedback)
	case millv1.StepKind_STEP_KIND_CHECK:
		return r.runCheck(stepCtx, s, step)
	case millv1.StepKind_STEP_KIND_GATE:
		return r.runGate(stepCtx, s, step, sr, detail)
	default:
		return result{}, fmt.Errorf("run %s: step %s has no kind", s.run.GetId(), step.GetName())
	}
}

// runSkill drives a skill step as a session in a harness. The session is
// found by its deterministic id, so a step re-run after a crash resumes the
// same session and the model picks up its own prior context.
func (r *Runner) runSkill(ctx context.Context, s *state, step *millv1.Step, sr *millv1.StepRun, token *millv1.StepToken, feedback string) (result, error) {
	name, settings := harnessFor(step)
	adapter, err := r.options.Harnesses.Lookup(name)
	if err != nil {
		return result{}, err
	}
	prompt, err := r.prompt(ctx, s, step, feedback)
	if err != nil {
		return result{}, err
	}
	handle := s.workspace.Handle()

	// The step's own credential is registered with the redactor before the
	// session is handed it, so a bearer that finds its way into a tool's
	// output cannot reach the log (docs/architecture.md §Observability).
	s.workspace.Redact(token.GetToken())
	logFile, err := openStepLog(r.options.LogDir, s.run.GetId(), step.GetName(), sr.GetAttempt())
	if err != nil {
		return result{}, err
	}
	if logFile != nil {
		defer func() { _ = logFile.Close() }()
	}

	session := harness.Session{
		ID:        sr.GetSessionId(),
		Model:     settings.GetModel(),
		Effort:    settings.GetEffort(),
		Workspace: harness.Workspace{Socket: handle.Socket, Token: token.GetToken()},
		Cwd:       handle.Root,
		Events:    logSink(logFile, r.options.Redactor, r.options.Now),
	}
	policy := harness.Policy{
		Allow: settings.GetToolPolicy().GetAllow(),
		Deny:  settings.GetToolPolicy().GetDeny(),
	}

	begin := adapter.Start
	if sr.GetAttempt() > 1 || s.resumed[sr.GetSessionId()] {
		begin = adapter.Resume
	}
	turn, err := begin(ctx, session, prompt, nil, policy)
	if err != nil {
		return result{}, err
	}
	s.markResumed(sr.GetSessionId())

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

// observe is what the mill itself saw after a skill step, which is the only
// thing offered as a claim. Nothing the session said is consulted: a session
// that reports a commit it did not make reports it into a transcript nothing
// reads (docs/domain-model.md §Evidence).
func (r *Runner) observe(ctx context.Context, s *state, step *millv1.Step, sr *millv1.StepRun, terminal *harness.Terminal) (string, error) {
	switch step.GetEvidence() {
	case millv1.EvidenceKind_EVIDENCE_KIND_COMMIT:
		// The head of the run's branch, read through the workspace. The
		// verifier still proves the object exists and is on the branch.
		commits, err := s.workspace.Git().Log(ctx, workspace.LogRequest{Limit: 1})
		if err != nil {
			return "", err
		}
		if len(commits) == 0 {
			return "", nil
		}
		return commits[0].SHA, nil

	case millv1.EvidenceKind_EVIDENCE_KIND_URL:
		pr, err := r.pullRequest(ctx, s)
		if err != nil {
			return "", err
		}
		return pr.URL, nil

	case millv1.EvidenceKind_EVIDENCE_KIND_ARTIFACT:
		return r.attachArtifact(ctx, s, step, sr, terminal.FinalMessage)

	default:
		return "", nil
	}
}

// runCheck runs a check step through the workspace's chokepoint.
func (r *Runner) runCheck(ctx context.Context, s *state, step *millv1.Step) (result, error) {
	res, err := check.Run(ctx, s.workspace, check.Request{
		Name:        step.GetName(),
		Check:       step.GetCheck(),
		WorkflowDir: r.options.WorkflowDir,
	})
	if err != nil {
		return result{}, err
	}
	out := result{exit: res.Exit}
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
// and its inputs as attributed data, then a check's feedback when this
// attempt is the revise for one.
func (r *Runner) prompt(ctx context.Context, s *state, step *millv1.Step, feedback string) (harness.Prompt, error) {
	skill, err := call(ctx, r.options.Store.GetSkill, &millv1.GetSkillRequest{
		Name:   step.GetSkill().GetName(),
		Digest: step.GetSkill().GetDigest(),
	})
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
	text := workflow.Prompt(skill.GetSkill(), node)
	if feedback != "" {
		// A revise's feedback is the mill's own words about a check or a
		// verdict, so it belongs with the instructions rather than in a
		// data block.
		text += "\n\n" + feedbackHeading + "\n\n" + feedback
	}
	return harness.Prompt{Text: text, OutputSchema: step.GetOutputSchema()}, nil
}

// feedbackHeading introduces what the previous attempt failed on.
const feedbackHeading = "## What the last attempt failed on\n\nThis is the mill's own report of the check or review that sent this step back. Fix it."

// pullRequest is the run branch's pull request, opened if the forge has
// none. It is the mill's own read of the forge, not the session's: a `url`
// step closes on what the forge returned when asked for the pull request by
// branch, never on a URL a session typed.
func (r *Runner) pullRequest(ctx context.Context, s *state) (forge.PullRequest, error) {
	client, err := r.options.Forges(ctx)
	if err != nil {
		return forge.PullRequest{}, err
	}
	return client.OpenPR(ctx, forge.OpenPRRequest{
		Branch:      s.run.GetBranch(),
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
