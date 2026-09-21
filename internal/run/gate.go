package run

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"text/template"

	daggerv1 "github.com/ivy/mill/gen/dagger/v1"
	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/dagger"
	"github.com/ivy/mill/internal/evidence"
	"github.com/ivy/mill/internal/notify"
)

// The gate branch of the loop: docs/architecture.md §"Gate steps" and
// docs/domain-model.md §Gate, in that order.
//
// A gate step evaluates its `auto` predicate over the evidence prior steps
// closed on and the run's tier ([evaluate]). If it holds, the ledger records
// an auto-approval and nothing reaches dagger at all. If it does not, the
// gate is filed in the three writes whose order is the whole of Invariant
// 12, and the run stops until somebody answers — except in single-run mode,
// where somebody is watching and [Runner.askNow] asks them.

// runGate evaluates a gate step's auto predicate and either records an
// auto-approval or files the gate.
func (r *Runner) runGate(ctx context.Context, s *state, step *millv1.Step, sr *millv1.StepRun, detail *millv1.InspectResponse) (result, error) {
	predicate := step.GetGate().GetAuto()
	env := newPredicateEnv(s.run, detail)
	approved, err := evaluate(predicate, env)
	// Why a name resolved to nothing, said out loud. A predicate that could
	// not be answered is false, and a human about to be asked a question
	// deserves to know it was the mill that could not answer rather than the
	// workflow that decided not to.
	for _, reason := range env.why() {
		r.options.Log.Info("gate predicate", "step", step.GetName(), "auto", predicate, "reason", reason)
		r.printf("  gate %s: %s\n", step.GetName(), reason)
	}
	if err != nil {
		// A predicate that does not parse is a workflow bug, and the safe
		// direction to be wrong in is a human: the gate is filed and the
		// reason is on the record.
		r.options.Log.Warn("gate predicate", "step", step.GetName(), "auto", predicate, "error", err)
		r.printf("  gate %s: %v; asking a human\n", step.GetName(), err)
		approved = false
	}
	if !approved {
		return result{}, r.fileGate(ctx, s, step, sr, detail)
	}
	r.printf("  gate %s auto-approved by %s\n", step.GetName(), predicate)
	out := result{autoApproved: true}
	claim, err := r.approve(ctx, s, step)
	if err != nil {
		return out, err
	}
	out.claim = claim
	return out, nil
}

// approve is what an approved gate authorizes. A gate is a decision, and the
// workflow says what the decision is for: quick-fix's merge gate declares
// `status` evidence, which is the forge's word on the branch it just agreed
// to merge, so approving it merges. A gate declaring `gate` evidence is a
// decision about nothing but itself and authorizes no act.
func (r *Runner) approve(ctx context.Context, s *state, step *millv1.Step) (string, error) {
	if step.GetEvidence() != millv1.EvidenceKind_EVIDENCE_KIND_STATUS {
		return "", nil
	}
	f := s.repo.Forge
	pr, err := f.PR(ctx, s.run.GetBranch())
	if err != nil {
		return "", err
	}
	if !pr.Merged() {
		if pr, err = f.Merge(ctx, pr); err != nil {
			return "", err
		}
	}
	sha, err := f.MergeCommit(ctx, pr)
	if err != nil {
		return "", err
	}
	s.mergeCommit = sha
	r.printf("  merged as %s\n", short(sha))
	return pr.HeadSHA, nil
}

// fileGate is the three writes, in the one order that survives a crash at
// any point: the patch that creates the gate and its blocking edge, the
// store's record of the gate id, then the release. A crash between the last
// two leaves a gate the store already knows about and will not file twice
// (Invariant 12); a crash between the first two is what [findFiledGate]
// exists for.
func (r *Runner) fileGate(ctx context.Context, s *state, step *millv1.Step, sr *millv1.StepRun, detail *millv1.InspectResponse) error {
	err := r.file(ctx, s, step, sr, detail)
	switch {
	case err == nil:
		return errReleased
	case errors.Is(err, errAnswered), errors.Is(err, errReleased):
		return err
	default:
		// Filing is the mill's own bookkeeping, not the step's work: a
		// patch dagger refused or a store write that failed is not a gate
		// a human declined, and parking the run on it would ask somebody to
		// answer a question that was never put to them.
		return fmt.Errorf("%w: filing the %s gate: %w", errAborted, step.GetName(), err)
	}
}

// errAborted ends a run without parking it. The circuit breaker is for work
// that failed; this is for the mill failing to keep its own record, which a
// human answers by looking at the worker, not at the node.
var errAborted = errors.New("run: aborted")

// file is fileGate's three writes, so that everything they can go wrong with
// is one error to classify.
func (r *Runner) file(ctx context.Context, s *state, step *millv1.Step, sr *millv1.StepRun, detail *millv1.InspectResponse) error {
	gate, err := r.findFiledGate(ctx, s, step)
	if err != nil {
		return err
	}
	if gate == nil {
		ask, err := r.renderAsk(ctx, s, step, detail)
		if err != nil {
			return err
		}
		gate, err = r.options.Dagger.CreateGate(ctx,
			dagger.RequestID(s.run.GetId(), step.GetName(), sr.GetAttempt()), s.lease,
			dagger.Gate{
				Title:  ask.title,
				Body:   ask.body,
				Blocks: s.node.GetReference(),
				Labels: dagger.GateLabels(s.run.GetId(), step.GetName()),
			})
		if err != nil {
			return err
		}
		if err := s.activity.Record(&millv1.Activity{
			Source: millv1.ActivitySource_ACTIVITY_SOURCE_MILL,
			Body: &millv1.Activity_GateFiled{GateFiled: &millv1.GateFiled{
				Kind:    string(noticeKind(step)),
				NodeId:  gate.GetId(),
				NodeRef: gate.GetReference(),
				Title:   ask.title,
			}},
		}); err != nil {
			return err
		}
		if err := s.activity.Close(); err != nil {
			return err
		}
		detachActivitySink(s)
		s.activity = nil
		// Only for a gate this attempt filed. A gate found already filed is
		// one somebody has already been told about, and telling them again
		// is the second message a crash recovery must not send.
		r.notified(ctx, s, sr.GetId(), notify.Notice{
			Kind:  noticeKind(step),
			Gate:  gate.GetReference(),
			Title: ask.title,
			Body:  ask.body,
		})
	}
	// The attempt stays open across the wait; its credential does not. The
	// token is revoked before the store forgets which lease fences this
	// run, because a revocation is a store write like any other.
	if s.token != "" {
		if _, err := call(ctx, r.options.Store.RevokeStepToken, &millv1.RevokeStepTokenRequest{
			LeaseId:     s.lease,
			StepTokenId: s.token,
		}); err != nil {
			return err
		}
	}
	if _, err := call(ctx, r.options.Store.MarkWaiting, &millv1.MarkWaitingRequest{
		LeaseId:    s.lease,
		RunId:      s.run.GetId(),
		GateNodeId: gate.GetId(),
		StepRunId:  sr.GetId(),
	}); err != nil {
		return err
	}
	// Single-run mode: there is a person watching, so the ask goes to them
	// rather than to nobody. The lease is not released and this attempt is
	// not closed; the next pass of the loop is the ordinary resume through
	// a gate, reading the answer they just wrote.
	if r.options.Answers != nil {
		return r.askNow(ctx, s, gate)
	}
	if err := r.options.Dagger.Release(ctx, s.lease, daggerv1.ReleaseReason_RELEASE_REASON_GATE, "gate "+gate.GetReference()); err != nil {
		return err
	}
	r.printf("  gate %s filed as %s; waiting on a human\n", step.GetName(), gate.GetReference())
	return nil
}

// askNow puts a gate to the person running `mill run` and writes their
// answer to dagger as theirs.
//
// The gate is filed and recorded first, in the same writes and the same
// order a worker uses, so a process killed while the question is on screen
// leaves exactly what a worker's crash leaves. What single-run mode skips is
// the release: the lease is still held, so the run reads the answer instead
// of waiting to be claimed again.
//
// That matters beyond convenience. Dagger's rule is that a dropped gate
// never satisfies the edge out of it, so a node whose gate was denied is
// never ready again — a run that had released its lease could not be claimed
// back to record the denial (dagger's docs/domain-model.md §Gates).
func (r *Runner) askNow(ctx context.Context, s *state, gate *daggerv1.Node) error {
	approved, err := r.options.Answers.Answer(ctx, Ask{
		NodeRef: s.node.GetReference(),
		GateRef: gate.GetReference(),
		Title:   gate.GetTitle(),
		Body:    gate.GetBody(),
	})
	if err != nil {
		return err
	}
	answer := "Denied at the terminal, under `mill run`, by the person the gate was put to."
	if approved {
		answer = "Approved at the terminal, under `mill run`, by the person the gate was put to."
	}
	// As the person, never as the mill: no lease, and a request id that
	// does not begin with mill's prefix (docs/domain-model.md §Principal).
	if err := r.options.Dagger.AnswerGate(ctx,
		dagger.UserRequestIDPrefix+gate.GetId(), gate.GetReference(), approved,
		&daggerv1.Result{Body: answer}); err != nil {
		return err
	}
	// MarkWaiting took the lease off the run, because for a worker that is
	// where the run stops. This one has not stopped: the same lease, which
	// dagger still holds, goes back on so that the writes which follow are
	// fenced by it again.
	leased, err := call(ctx, r.options.Store.SetLease, &millv1.SetLeaseRequest{
		LeaseId:   s.lease,
		RunId:     s.run.GetId(),
		WorkerRef: r.options.WorkerRef,
	})
	if err != nil {
		return err
	}
	s.run = leased.GetRun()
	r.printf("  gate %s answered at the terminal: %s\n", gate.GetReference(), decision(approved))
	return errAnswered
}

// errAnswered ends a gate step's attempt without closing or failing it: the
// gate has an answer, and the loop's next pass reads it on the same branch a
// worker takes when it is claimed again.
var errAnswered = errors.New("run: the gate was answered")

// findFiledGate is the answer to the one window the write order cannot
// close. `ApplyPatch` comes before the store's record of the gate id, so a
// process that dies between them leaves a gate dagger has and the ledger
// does not; a rerun that trusted the ledger alone would file a second one
// for the same step. Dagger's graph is the source of truth for that single
// check, and mill's own labels — the run and the step — say which gate is
// this step's rather than some other child a human filed.
//
// A gate found this way is recorded and waited on exactly as one just filed
// is, whether or not it has already been answered. Making the answered case
// its own path would buy one claim cycle in a crash recovery and cost a
// second way for a gate to close; the run is marked waiting, the lease goes
// back, and the ordinary resume branch reads the answer on the next claim.
//
// Nothing else in the loop reads dagger to decide what to write. This does,
// and the reason is written into docs/ledger.md §Resume beside it.
func (r *Runner) findFiledGate(ctx context.Context, s *state, step *millv1.Step) (*daggerv1.Node, error) {
	gates, err := r.options.Dagger.Gates(ctx, s.node.GetReference(),
		dagger.GateLabels(s.run.GetId(), step.GetName())...)
	if err != nil {
		return nil, err
	}
	if len(gates) == 0 {
		return nil, nil
	}
	// The newest, in the vanishing case of more than one: a gate filed by a
	// generation that restart has already retired shares no run id, so the
	// labels have narrowed this to this run's own attempts at this step.
	filed := gates[len(gates)-1]
	r.printf("  gate %s was already filed as %s; not filing another\n", step.GetName(), filed.GetReference())
	return filed, nil
}

// gateByID is the gate the ledger recorded, read back from dagger.
//
// It is a search rather than a lookup because dagger resolves references and
// not ids — `mill#7` is a reference, the ULID beside it is not something the
// API can be asked for — and what the ledger holds, correctly, is the id.
// Mill's own labels narrow the node's children to this run's gates, and the
// id picks the one out.
func (r *Runner) gateByID(ctx context.Context, s *state, id string) (*daggerv1.Node, error) {
	gates, err := r.options.Dagger.Gates(ctx, s.node.GetReference(),
		dagger.GateLabel, dagger.RunLabel(s.run.GetId()))
	if err != nil {
		return nil, err
	}
	for _, gate := range gates {
		if gate.GetId() == id {
			return gate, nil
		}
	}
	return nil, fmt.Errorf("run %s: gate %s is not a gate on %s", s.run.GetId(), id, s.node.GetReference())
}

// resumeGate is the branch at the top of the loop: a run whose gate is
// already filed reads it rather than filing another. A gate that is done
// closes the step with `gate` evidence; a gate a human disposed of instead
// of completing is a denial, and fails the run with the step's deny_result.
func (r *Runner) resumeGate(ctx context.Context, s *state, step *millv1.Step, detail *millv1.InspectResponse) error {
	gate, err := r.gateByID(ctx, s, s.run.GetGateNodeId())
	if err != nil {
		return err
	}
	sr := latestStepRun(detail, step.GetName())
	if sr == nil {
		return fmt.Errorf("run %s: gate %s is filed but has no attempt", s.run.GetId(), step.GetName())
	}
	switch gate.GetStatus() {
	case daggerv1.Status_STATUS_DONE:
		if err := r.reopenActivity(s, step, sr); err != nil {
			return err
		}
		verified, err := evidence.Verify(ctx, millv1.EvidenceKind_EVIDENCE_KIND_GATE, evidence.Step{
			Name:       step.GetName(),
			Definition: step,
			Workflow:   s.workflow,
			Claim:      gate.GetId(),
			Gates:      gates{runner: r, state: s},
			Now:        r.options.Now,
		})
		if err != nil {
			return err
		}
		// The attempt that filed the gate is the attempt that closes on it:
		// the step has one step run, not two, because waiting for a human
		// is not a second try at anything.
		return r.closeStep(ctx, s, sr, step, verified, result{claim: gate.GetId()})

	case daggerv1.Status_STATUS_OPEN:
		// Nobody has answered yet. A node blocked by an open gate is not in
		// the ready set, so this is a `mill run` aimed at it by hand; give
		// the lease back and leave the run exactly as it was.
		if err := r.options.Dagger.Release(ctx, s.lease,
			daggerv1.ReleaseReason_RELEASE_REASON_GATE, "gate "+gate.GetReference()); err != nil {
			return err
		}
		r.printf("  gate %s is still open; %s waits\n", gate.GetReference(), s.node.GetReference())
		return errReleased

	default:
		if err := r.reopenActivity(s, step, sr); err != nil {
			return err
		}
		return r.denyGate(ctx, s, step, sr, gate)
	}
}

// denyGate ends a run whose gate a human disposed of rather than completing.
// The node is completed with the step's deny_result, so whoever asked for
// the work is told why it stopped, and only then is the run failed: dagger
// first, store second, as everywhere else.
func (r *Runner) denyGate(ctx context.Context, s *state, step *millv1.Step, sr *millv1.StepRun, gate *daggerv1.Node) error {
	body := step.GetGate().GetDenyResult()
	if body == "" {
		body = fmt.Sprintf("The %s gate was not approved.", step.GetName())
	}
	if err := r.options.Dagger.Complete(ctx,
		dagger.RequestID(s.run.GetId(), step.GetName(), denialAttempt), s.lease,
		s.node.GetReference(), &daggerv1.Result{Body: body}); err != nil {
		return err
	}
	if err := r.endActivity(ctx, s, step, sr, millv1.StepRunStatus_STEP_RUN_STATUS_FAILED, millv1.EvidenceKind_EVIDENCE_KIND_GATE, "gate denied"); err != nil {
		return err
	}
	if _, err := call(ctx, r.options.Store.Fail, &millv1.FailRequest{
		LeaseId: s.lease,
		RunId:   s.run.GetId(),
		Reason:  fmt.Sprintf("gate %s was denied at step %s", gate.GetReference(), step.GetName()),
	}); err != nil {
		return err
	}
	r.printf("  gate %s was denied; %s stops here\n", step.GetName(), s.node.GetReference())
	return errReleased
}

// denialAttempt distinguishes the patch that reports a denial from the patch
// that would have completed the node with the work's own result. They are
// two different writes and must never share an idempotency key.
const denialAttempt = 0

// ask is a gate's rendered title and body.
type ask struct {
	title string
	body  string
}

// renderAsk fills the gate's templates from the run. The fields are the ones
// docs/domain-model.md §Gate names: which node, which step, and the
// artifacts the decision is about.
func (r *Runner) renderAsk(ctx context.Context, s *state, step *millv1.Step, detail *millv1.InspectResponse) (ask, error) {
	data := r.templateData(s, step)
	funcs := r.askFuncs(ctx, s, detail)
	title, err := renderWith(step.GetGate().GetAsk().GetTitle(), data, funcs)
	if err != nil {
		return ask{}, err
	}
	body, err := renderWith(step.GetGate().GetAsk().GetBody(), data, funcs)
	if err != nil {
		return ask{}, err
	}
	return ask{title: title, body: body}, nil
}

// askFuncs are the two functions a gate's ask may call, and the reason the
// ask is more than the run's own facts.
//
// A gate is answered by reading, and what is being read is regularly a step's
// artifact: `feature`'s plan gate asks a person to approve a plan, and a
// question that does not carry the plan is a question that cannot be answered
// where it is asked. The artifact is on the node in dagger, so `artifact`
// fetches it and `head` cuts it to the length a notification can carry.
//
// Neither ever fails the ask. An artifact that is not there yields a line
// saying so and the gate is filed regardless: a person who has to open dagger
// to read the plan is a worse outcome than a run that stops, and stopping the
// run is what an error here would do (docs/domain-model.md §Gate).
func (r *Runner) askFuncs(ctx context.Context, s *state, detail *millv1.InspectResponse) template.FuncMap {
	return template.FuncMap{
		"artifact": func(step string) string { return r.artifactOf(ctx, s, detail, step) },
		"head":     head,
	}
}

// artifactOf is the text of a step's artifact evidence, read from dagger.
// The ledger holds the digest of what was attached, not the bytes, so the
// node is where the bytes are.
func (r *Runner) artifactOf(ctx context.Context, s *state, detail *millv1.InspectResponse, step string) string {
	digest := ""
	for _, d := range detail.GetSteps() {
		if d.GetStepRun().GetStepName() == step &&
			d.GetEvidence().GetKind() == millv1.EvidenceKind_EVIDENCE_KIND_ARTIFACT {
			digest = d.GetEvidence().GetArtifact()
		}
	}
	if digest == "" {
		return fmt.Sprintf("(step %s has attached no artifact to %s)", step, s.node.GetReference())
	}
	held, err := r.options.Dagger.Artifacts(ctx, s.node.GetReference())
	if err != nil {
		r.options.Log.Warn("gate ask", "artifact", step, "error", err)
		return fmt.Sprintf("(the artifact of %s could not be read from %s: %v)", step, s.node.GetReference(), err)
	}
	for _, artifact := range held {
		if artifact.GetMetadata().GetFields()["digest"].GetStringValue() != digest {
			continue
		}
		var b strings.Builder
		for _, part := range artifact.GetParts() {
			b.WriteString(part.GetText().GetText())
		}
		return b.String()
	}
	return fmt.Sprintf("(the artifact %s of step %s is not on %s)", short(digest), step, s.node.GetReference())
}

// head is the first n lines of a text, with a line saying what was left out.
// A gate's body travels: it is read in dagger, and carried to a person by
// whatever the deployment configured (internal/notify). Quoting a whole plan
// there is quoting it into a chat message.
func head(n int, text string) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if n <= 0 || len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[:n], "\n") + fmt.Sprintf("\n\n… %d more lines; the whole of it is the artifact on this node.", len(lines)-n)
}

// render expands one gate template.
func render(text string, data map[string]any) (string, error) {
	return renderWith(text, data, nil)
}

// renderWith expands a template that may call the ask's own functions.
func renderWith(text string, data map[string]any, funcs template.FuncMap) (string, error) {
	t, err := template.New("ask").Option("missingkey=zero").Funcs(funcs).Parse(text)
	if err != nil {
		return "", fmt.Errorf("gate ask: %w", err)
	}
	var b strings.Builder
	if err := t.Execute(&b, data); err != nil {
		return "", fmt.Errorf("gate ask: %w", err)
	}
	return b.String(), nil
}

// gates adapts the run's own gate lookup to the `gate` verifier's seam. The
// verifier is handed an id and asks for that node; finding it among the
// node's children is [Runner.gateByID]'s business and not the verifier's.
type gates struct {
	runner *Runner
	state  *state
}

func (g gates) Node(ctx context.Context, id string) (evidence.GateNode, error) {
	node, err := g.runner.gateByID(ctx, g.state, id)
	if err != nil {
		return evidence.GateNode{}, err
	}
	gate := evidence.GateNode{ID: node.GetId(), Status: daggerStatus(node.GetStatus())}
	if gate.Status != "done" {
		return gate, nil
	}
	// The gate's result is the evidence. A node message carries none, so
	// the one node dagger is asked about twice is the one whose answer the
	// ledger is about to record.
	result, err := g.runner.options.Dagger.Result(ctx, node.GetReference())
	if err != nil {
		return evidence.GateNode{}, err
	}
	gate.Result = result.GetBody()
	return gate, nil
}

// daggerStatus is a node status as the `gate` verifier reads it.
func daggerStatus(status daggerv1.Status) string {
	switch status {
	case daggerv1.Status_STATUS_OPEN:
		return "open"
	case daggerv1.Status_STATUS_DONE:
		return "done"
	case daggerv1.Status_STATUS_DROPPED:
		return "dropped"
	default:
		return ""
	}
}

// latestStepRun is the newest attempt at a step in an inspect response.
func latestStepRun(detail *millv1.InspectResponse, name string) *millv1.StepRun {
	var latest *millv1.StepRun
	for _, d := range detail.GetSteps() {
		sr := d.GetStepRun()
		if sr.GetStepName() != name {
			continue
		}
		if latest == nil || sr.GetAttempt() > latest.GetAttempt() {
			latest = sr
		}
	}
	return latest
}

// decision renders an answer for the run's output.
func decision(approved bool) string {
	if approved {
		return "approved"
	}
	return "denied"
}
