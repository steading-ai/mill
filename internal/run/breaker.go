package run

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"

	harnessv1 "github.com/steading-ai/mill/gen/harness/v1"
	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/harness"
	"github.com/steading-ai/mill/internal/notify"
	"github.com/steading-ai/mill/internal/skift"
)

// maxCommentBytes mirrors skift's internal/domain.MaxCommentBytes at the
// commit mill's go.mod pins (internal/run/limits_test.go catches drift):
// not importable from here, since internal/domain is skift's own and not
// vendored, and not necessarily what a checkout's working tree currently
// has, since skift's own ceiling has moved since mill's pin.
const maxCommentBytes = 64 << 10

// maxBodyBytes mirrors skift's internal/domain.MaxBodyBytes, on the same
// terms as maxCommentBytes above.
const maxBodyBytes = 256 << 10

// The circuit breaker: docs/architecture.md §"Failure and stop".
//
// A step whose retries are exhausted does not fail the run and does not try
// again. It posts what it has, comments every round of findings on the node,
// files a gate asking a human what to do, records the park, and releases the
// lease with reason `budget`. Asking a human costs the attempts it took to
// get there, and the human's two answers are `resume` — complete the gate,
// and the run continues at the parked step with its attempts reset — or
// `mill restart`.

// park trips the breaker.
func (r *Runner) park(ctx context.Context, s *state, step *millv1.Step, sr *millv1.StepRun, _ *millv1.InspectResponse, cause error) error {
	rounds := s.findings[step.GetName()]
	if len(rounds) == 0 {
		rounds = []string{cause.Error()}
	}
	artifacts := r.attachRounds(ctx, s, step, sr, rounds)
	findings := roundsOf(s.node.GetReference(), step.GetName(), sr.GetAttempt(), rounds, artifacts)
	if err := r.commentFindings(ctx, s, step, sr, findings); err != nil {
		return err
	}
	gate, err := s.skift.CreateGate(ctx,
		skift.RequestID(s.run.GetId(), step.GetName()+"-park", sr.GetAttempt()), s.leaseID(),
		skift.Gate{
			Title:  fmt.Sprintf("%s: %s stopped after %d attempts", s.node.GetReference(), step.GetName(), sr.GetAttempt()),
			Body:   parkBody(s, step, findings),
			Blocks: s.node.GetReference(),
			Labels: skift.GateLabels(s.run.GetId(), step.GetName()+"-park"),
		})
	if err != nil {
		return err
	}
	r.notified(ctx, s, sr.GetId(), notify.Notice{
		Kind:  notify.KindPark,
		Gate:  gate.GetReference(),
		Title: gate.GetTitle(),
		Body:  gate.GetBody(),
	})
	if _, err := call(ctx, r.options.Store.Park, &millv1.ParkRequest{
		LeaseId:    s.leaseID(),
		RunId:      s.run.GetId(),
		GateNodeId: gate.GetId(),
		Reason:     millv1.ParkedReason_PARKED_REASON_RETRIES,
	}); err != nil {
		return err
	}
	if err := r.options.Skift.Release(ctx, s.leaseID(), skiftv1.ReleaseReason_RELEASE_REASON_BUDGET, "retries exhausted on "+step.GetName()); err != nil {
		return err
	}

	r.printf("  parked on %s; %s asks a human what to do\n", step.GetName(), gate.GetReference())
	return errReleased
}

// attachRounds attaches every round of findings to the node as its own
// artifact, before any of it is trimmed for a comment or a gate body: the
// artifact is what a cut round's marker points to, and it is there whether
// or not anything ends up cut. A round that could not be attached is logged
// and skipped rather than aborting the park — the same rule as the comment
// below: a presentation error must not be the reason nobody is asked.
func (r *Runner) attachRounds(ctx context.Context, s *state, step *millv1.Step, sr *millv1.StepRun, rounds []string) []string {
	names := make([]string, len(rounds))
	for i, round := range rounds {
		name := fmt.Sprintf("%s-%s-round-%d", s.run.GetId(), step.GetName(), i+1)
		names[i] = name
		requestID := skift.RequestID(s.run.GetId(), fmt.Sprintf("%s-findings-round-%d", step.GetName(), i+1), sr.GetAttempt())
		description := fmt.Sprintf("%s round %d of %d, in full", step.GetName(), i+1, len(rounds))
		if err := s.skift.AttachText(ctx, requestID, s.leaseID(), s.node.GetReference(),
			name, description, round, map[string]any{
				"run":  s.run.GetId(),
				"step": step.GetName(),
			}); err != nil {
			r.printf("  round %d of %s's findings could not be attached to %s: %s\n", i+1, step.GetName(), s.node.GetReference(), err)
		}
	}
	return names
}

// commentFindings posts the findings comment for a park, surviving a
// refusal that is not a fence: the gate that follows is what asks the
// human, and a comment's own presentation error — too large, or any other
// refusal skift gives it — must not be the reason a run retries forever
// instead of parking. A fenced write is the one exception: it means the run
// is already being abandoned elsewhere, and posting a substitute under a
// lease that is no longer current would be meaningless.
func (r *Runner) commentFindings(ctx context.Context, s *state, step *millv1.Step, sr *millv1.StepRun, findings string) error {
	requestID := skift.RequestID(s.run.GetId(), step.GetName()+"-findings", sr.GetAttempt())
	err := s.skift.Comment(ctx, requestID, s.leaseID(), s.node.GetReference(), findings)
	if err == nil {
		return nil
	}
	if errors.Is(err, skift.ErrFenced) {
		return err
	}
	r.printf("  the findings comment on %s was refused: %s\n", s.node.GetReference(), err)
	fallback := fmt.Sprintf("The findings comment was refused: %s. See the artifacts attached to this node.", err)
	if err := s.skift.Comment(ctx, requestID, s.leaseID(), s.node.GetReference(), fallback); err != nil {
		if errors.Is(err, skift.ErrFenced) {
			return err
		}
		r.printf("  the fallback comment on %s was also refused: %s\n", s.node.GetReference(), err)
	}
	return nil
}

// roundsOf is every round of findings in one comment, oldest first, each
// trimmed to fit inside maxCommentBytes. A round's budget comes from
// splitBudget, so one oversized round cannot starve the others; a round
// that must be cut keeps its tail — the useful part of a build or test
// report is last — and ends with a line naming how much was dropped, the
// artifact that holds the round in full, and the `mill logs` command that
// reads it back. One comment rather than one per round, because what a
// human is being asked to read is a sequence and reading it should not mean
// scrolling through a node's whole history to reassemble it.
func roundsOf(ref, step string, attempt int64, rounds []string, artifacts []string) string {
	trimmed := make([]string, len(rounds))
	sizes := make([]int, len(rounds))
	for i, round := range rounds {
		trimmed[i] = strings.TrimSpace(round)
		sizes[i] = len(trimmed[i])
	}

	header := fmt.Sprintf("`%s` failed %d times. Every round, oldest first:\n", step, len(rounds))
	roundHeaders := make([]string, len(rounds))
	overhead := len(header)
	for i := range rounds {
		roundHeaders[i] = fmt.Sprintf("\n### Round %d\n\n", i+1)
		overhead += len(roundHeaders[i]) + 1 // the newline after every round's content
	}
	budgets := splitBudget(sizes, maxCommentBytes-overhead)

	var b strings.Builder
	b.WriteString(header)
	for i, content := range trimmed {
		b.WriteString(roundHeaders[i])
		if len(content) <= budgets[i] {
			b.WriteString(content)
			b.WriteString("\n")
			continue
		}
		name := roundArtifact(artifacts, i)
		// The round furthest from the parking attempt is first; sr's own
		// attempt is the last round's. This is a best-effort pointer for a
		// human, not exact bookkeeping: s.findings carries no attempt
		// number per round, and an aborted or upstream-refused attempt
		// between two recorded rounds would throw the count off.
		roundAttempt := attempt - int64(len(rounds)-1-i)
		worst := cutMarker(len(content), ref, step, roundAttempt, name)
		reserve := len(worst) + 2 // the blank line before the marker
		shown, dropped := trimTail(content, budgets[i]-reserve)
		fmt.Fprintf(&b, "%s\n\n%s\n", shown, cutMarker(dropped, ref, step, roundAttempt, name))
	}
	return b.String()
}

// roundArtifact is the artifact name a round's cut marker cites, or a
// generic pointer if attachRounds could not name one.
func roundArtifact(names []string, i int) string {
	if i < len(names) && names[i] != "" {
		return names[i]
	}
	return "the artifact attached to this node"
}

// cutMarker is the line a trimmed round ends with.
func cutMarker(dropped int, ref, step string, attempt int64, artifact string) string {
	return fmt.Sprintf(
		"_%d bytes cut; the round is attached in full as `%s` — `mill logs %s --step %s --attempt %d`._",
		dropped, artifact, ref, step, attempt)
}

// parkBody is what the park gate asks.
//
// The failing step's rounds — already sized to fit a comment, so always
// small enough for the body too — then every attempt of every step in the
// span, including the attempts of steps that went on to close, trimmed to
// whatever room is left if it does not fit. The cause of a park is
// regularly not the step that parked: a `commit` session that said it could
// not proceed leaves a clean-tree check to fail four rounds later, and a
// human reading only the check's stderr cannot see why. The whole span is
// here so that reading the gate is enough.
func parkBody(s *state, step *millv1.Step, findings string) string {
	header := fmt.Sprintf("Run %s of %s stopped at step %s.\n\n", s.run.GetId(), s.node.GetReference(), step.GetName())
	instructions := fmt.Sprintf(
		"Complete this node to resume the run at that step with its attempts reset. "+
			"Drop it, or run `mill restart %s`, to start over.", s.node.GetReference())
	story := spanStory(s.span)
	budget := maxBodyBytes - len(header) - len(findings) - len(instructions) - 2 // the newlines joining the three
	if budget < 0 {
		budget = 0
	}
	if len(story) > budget {
		story, _ = trimTail(story, budget)
	}
	return header + findings + "\n" + story + "\n" + instructions
}

// trimTail keeps the tail of text within budget bytes — the useful part of
// a build or test report is last — and reports how many bytes were
// dropped. The cut lands on a rune boundary so a multi-byte character at
// the edge is never split.
func trimTail(text string, budget int) (shown string, dropped int) {
	if budget <= 0 {
		return "", len(text)
	}
	if len(text) <= budget {
		return text, 0
	}
	dropped = len(text) - budget
	for dropped < len(text) && !utf8.RuneStart(text[dropped]) {
		dropped++
	}
	return text[dropped:], dropped
}

// splitBudget divides budget across sizes by water-filling: an entry under
// its current equal share keeps its whole size, and what it does not use is
// redistributed to the rest, repeating until every remaining entry is over
// its share. What is left is then split evenly among them. Nothing already
// under budget is trimmed to reach this — sizes that already fit in budget
// come back unchanged — and no single oversized entry can starve the
// others down to nothing while a smaller neighbor's unused share goes to
// waste.
func splitBudget(sizes []int, budget int) []int {
	if budget < 0 {
		budget = 0
	}
	result := make([]int, len(sizes))
	total := 0
	for _, size := range sizes {
		total += size
	}
	if total <= budget {
		copy(result, sizes)
		return result
	}

	open := make([]int, len(sizes))
	for i := range open {
		open[i] = i
	}
	remaining := budget
	for len(open) > 0 {
		share := remaining / len(open)
		var next []int
		settled := false
		for _, i := range open {
			if sizes[i] <= share {
				result[i] = sizes[i]
				remaining -= sizes[i]
				settled = true
			} else {
				next = append(next, i)
			}
		}
		if !settled {
			base := remaining / len(open)
			extra := remaining % len(open)
			for k, i := range open {
				result[i] = base
				if k < extra {
					result[i]++
				}
			}
			return result
		}
		open = next
	}
	return result
}

// attempt is one attempt in the span the park gate reports: what the ledger
// holds, plus the last thing the attempt said for itself.
type attempt struct {
	// step is the step the attempt was at.
	step string
	// number is the attempt number the ledger gave it.
	number int64
	// exit is how the attempt's process ended, rendered.
	exit string
	// words is the session's final message, or the check's stderr.
	words string
	// failed says whether the attempt failed. A closed attempt is in the
	// span too: a step that closed on evidence and still left the run
	// unable to continue is exactly the case a human is being asked about.
	failed bool
}

// recordAttempt appends one attempt to the span.
func (s *state) recordAttempt(step *millv1.Step, sr *millv1.StepRun, exit *harnessv1.ProcessExit, words string, failed bool) {
	s.span = append(s.span, attempt{
		step:   step.GetName(),
		number: sr.GetAttempt(),
		exit:   exitOf(exit),
		words:  words,
		failed: failed,
	})
}

// spanTail is how much of an attempt's last words the gate carries. Enough
// to see what a session or a script was saying when it stopped, and not the
// whole transcript: the log has that, and a gate nobody reads helps nobody.
const spanTail = 20

// spanStory is every attempt in the span, oldest first.
func spanStory(span []attempt) string {
	if len(span) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n## Every attempt since a human was last asked\n")
	for _, a := range span {
		outcome := "closed"
		if a.failed {
			outcome = "failed"
		}
		fmt.Fprintf(&b, "\n### %s attempt %d — %s, %s\n\n", a.step, a.number, outcome, a.exit)
		words := tail(a.words, spanTail)
		if words == "" {
			fmt.Fprintf(&b, "It said nothing.\n")
			continue
		}
		fmt.Fprintf(&b, "```\n%s\n```\n", words)
	}
	return b.String()
}

// tail is the last n lines of a message, trimmed.
func tail(message string, n int) string {
	message = strings.Trim(message, "\n")
	if message == "" {
		return ""
	}
	lines := strings.Split(message, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// exitOf renders how a process ended for a human reading the gate.
func exitOf(exit *harnessv1.ProcessExit) string {
	if exit == nil {
		return "no process ran"
	}
	return exitText(harness.ProcessExit{
		Status: exit.GetStatus(),
		Signal: exit.GetSignal(),
		Reason: harness.ReasonFrom(exit.GetReason()),
	})
}

// restoreRetried recovers, from the ledger, the attempt each step's retries
// are measured from. A human who answered a park gate paid for a fresh set
// of attempts at the parked step, and the worker that resumed the run
// recorded that in its own head; a worker that takes the run over later —
// after a restart, a crash, a lease that expired — must not charge those
// attempts again (Invariant 10: nothing is carried in the worker's head).
//
// The audit trail says it: the `resume` event that answered a `park`, and
// the attempt that failed into that park, which is the last one the human
// paid for. Only the parked step's counter moves, as resumePark moves it.
func (r *Runner) restoreRetried(ctx context.Context, s *state) error {
	history, err := call(ctx, r.options.Store.History, &millv1.HistoryRequest{Id: s.run.GetId()})
	if err != nil {
		return err
	}
	var failed, parkedOn string
	paid := map[string]bool{}
	for _, event := range history.GetEvents() {
		switch event.GetOp() {
		case "fail_step_run", "abort_step_run":
			failed = event.GetStepRunId()
		case "park":
			parkedOn = failed
		case "resume":
			if parkedOn != "" {
				paid[parkedOn] = true
			}
		}
	}
	if len(paid) == 0 {
		return nil
	}
	detail, err := call(ctx, r.options.Store.Inspect, &millv1.InspectRequest{Id: s.run.GetId()})
	if err != nil {
		return err
	}
	for _, d := range detail.GetSteps() {
		sr := d.GetStepRun()
		if paid[sr.GetId()] && sr.GetAttempt() > s.retried[sr.GetStepName()] {
			s.retried[sr.GetStepName()] = sr.GetAttempt()
		}
	}
	return nil
}

// resumePark is the other half of the breaker: a claim that took over a
// parked run reads the gate the breaker filed before it runs anything.
//
// A gate a human completed is the price paid, so the parked step's attempts
// so far no longer count against its retries and the run continues where it
// stopped. Anything else — an unanswered gate, or one disposed of rather
// than completed — leaves the run parked and gives the lease back: the
// human's other answer is `mill restart`, which is a different command and
// not something a worker decides for itself.
func (r *Runner) resumePark(ctx context.Context, s *state) error {
	if !s.parked {
		return nil
	}
	gate, err := r.gateByID(ctx, s, s.run.GetGateNodeId())
	if err != nil {
		return err
	}
	if gate.GetStatus() != skiftv1.Status_STATUS_DONE {
		if err := r.options.Skift.Release(ctx, s.leaseID(),
			skiftv1.ReleaseReason_RELEASE_REASON_GATE, "park gate "+gate.GetReference()); err != nil {
			return err
		}
		r.printf("park gate %s is unanswered; %s stays parked\n", gate.GetReference(), s.node.GetReference())
		return errReleased
	}

	detail, err := call(ctx, r.options.Store.Inspect, &millv1.InspectRequest{Id: s.run.GetId()})
	if err != nil {
		return err
	}
	// The parked step's attempts, and no other step's: the human paid for
	// this step to try again, not for the run to forget what the steps
	// before it spent. The span the next park would report starts here.
	step := detail.GetSummary().GetCurrentStep()
	if sr := latestStepRun(detail, step); sr != nil {
		s.retried[step] = sr.GetAttempt()
	}
	s.span = nil
	if _, err := call(ctx, r.options.Store.Resume, &millv1.ResumeRequest{
		LeaseId: s.leaseID(),
		RunId:   s.run.GetId(),
	}); err != nil {
		return err
	}
	s.parked = false
	r.printf("park gate %s was answered; resuming at %s with its attempts reset\n", gate.GetReference(), step)
	return nil
}
