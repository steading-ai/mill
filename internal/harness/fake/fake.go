// Package fake is a scripted [harness.Harness] for specs.
//
// It replays a fixed sequence of events and a terminal record, records the
// calls it was made with, and answers Cancel. Nothing about it talks to a
// process or a model, so a spec that is about the worker's own logic — step
// ordering, evidence, usage reaching the ledger — can be written without a
// sidecar, a network, or a wall clock.
package fake

import (
	"context"
	"fmt"
	"sync"

	"google.golang.org/protobuf/types/known/structpb"

	harnessv1 "github.com/ivy/mill/gen/harness/v1"
	"github.com/ivy/mill/internal/harness"
)

// Name is what a step's `harness` table calls this adapter in a spec.
const Name = "fake"

// Script is one turn's worth of replay: the events the session emits, in
// order, and the terminal record it ends with.
type Script struct {
	// Events reach the caller's sink before the turn ends, in this order.
	Events []*harnessv1.Event
	// Terminal is what the turn ends with. Nil means a session that ran and
	// said nothing.
	Terminal *harness.Terminal
	// Err is returned by Start or Resume instead of a turn, for the spec
	// that needs the harness itself to fail.
	Err error
	// Hold keeps the turn in flight until the session is cancelled or the
	// caller's context ends, so a spec can exercise the cancellation path.
	Hold bool
	// Cancelled is the terminal record a held turn ends with once it is
	// cancelled. Nil means a session that never ran, with reason
	// `cancelled`.
	Cancelled *harness.Terminal
}

// Call is one thing a spec asked the harness to do.
type Call struct {
	// Verb is "start", "resume", or "cancel".
	Verb        string
	Session     harness.Session
	Prompt      harness.Prompt
	Attachments []harness.Attachment
	Policy      harness.Policy
}

// Harness replays scripts in order. The last script is reused once they run
// out, so a spec that cares about one turn does not have to enumerate the
// rest; a spec that cares about the sequence gives one script per turn.
type Harness struct {
	mu        sync.Mutex
	scripts   []Script
	next      int
	calls     []Call
	cancelled map[string]chan struct{}
}

var _ harness.Harness = (*Harness)(nil)

// New returns a harness that replays the given scripts. With no script it
// replays an empty session that ran and finished.
func New(scripts ...Script) *Harness {
	if len(scripts) == 0 {
		scripts = []Script{{Terminal: Terminal("", nil, nil)}}
	}
	return &Harness{scripts: scripts, cancelled: map[string]chan struct{}{}}
}

// Start begins a session by replaying the next script.
func (h *Harness) Start(ctx context.Context, session harness.Session, prompt harness.Prompt, attachments []harness.Attachment, policy harness.Policy) (*harness.Turn, error) {
	return h.play(ctx, "start", session, prompt, attachments, policy)
}

// Resume continues a session by replaying the next script.
func (h *Harness) Resume(ctx context.Context, session harness.Session, prompt harness.Prompt, attachments []harness.Attachment, policy harness.Policy) (*harness.Turn, error) {
	return h.play(ctx, "resume", session, prompt, attachments, policy)
}

// Cancel ends a held turn. Cancelling a session that is not running is not
// an error, exactly as it is not for the real adapter.
func (h *Harness) Cancel(_ context.Context, session harness.Session) error {
	h.mu.Lock()
	h.calls = append(h.calls, Call{Verb: "cancel", Session: session})
	stop, running := h.cancelled[session.ID]
	delete(h.cancelled, session.ID)
	h.mu.Unlock()

	if running {
		close(stop)
	}
	return nil
}

func (h *Harness) play(ctx context.Context, verb string, session harness.Session, prompt harness.Prompt, attachments []harness.Attachment, policy harness.Policy) (*harness.Turn, error) {
	h.mu.Lock()
	h.calls = append(h.calls, Call{Verb: verb, Session: session, Prompt: prompt, Attachments: attachments, Policy: policy})
	script := h.scripts[min(h.next, len(h.scripts)-1)]
	h.next++
	stop := make(chan struct{})
	if script.Hold {
		h.cancelled[session.ID] = stop
	}
	h.mu.Unlock()

	if script.Err != nil {
		return nil, script.Err
	}

	turn := harness.NewTurn()
	go func() {
		for _, event := range script.Events {
			session.Events.Send(event)
		}
		if !script.Hold {
			turn.Finish(script.Terminal, nil)
			return
		}
		select {
		case <-stop:
		case <-ctx.Done():
		}
		cancelled := script.Cancelled
		if cancelled == nil {
			cancelled = NeverRan(harness.ReasonCancelled, "the session was cancelled")
		}
		turn.Finish(cancelled, nil)
	}()
	return turn, nil
}

// Calls are the calls the harness has been made, in order.
func (h *Harness) Calls() []Call {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Call{}, h.calls...)
}

// Terminal builds the record of a session that ran and finished: its final
// message, its structured output when the step declared a schema, and what
// it billed, per model.
func Terminal(final string, output map[string]any, usage harness.Usage) *harness.Terminal {
	return &harness.Terminal{
		FinalMessage:     final,
		StructuredOutput: output,
		Usage:            usage,
	}
}

// NeverRan builds the record of a session that never ran: status -1, and the
// reason why.
func NeverRan(reason harness.ExitReason, message string) *harness.Terminal {
	return &harness.Terminal{
		FinalMessage: message,
		Exit:         harness.ProcessExit{Status: -1, Reason: reason},
	}
}

// Text is an assistant-said event, for scripting a turn's stream.
func Text(requestID, text string) *harnessv1.Event {
	return &harnessv1.Event{RequestId: requestID, Body: &harnessv1.Event_Text{Text: &harnessv1.Text{Text: text}}}
}

// ToolCall is a session asking to use a tool.
func ToolCall(requestID, callID, name string, arguments map[string]any) *harnessv1.Event {
	fields, err := structpb.NewStruct(arguments)
	if err != nil {
		panic(fmt.Sprintf("fake: tool call arguments are not JSON: %v", err))
	}
	return &harnessv1.Event{RequestId: requestID, Body: &harnessv1.Event_ToolCall{
		ToolCall: &harnessv1.ToolCall{CallId: callID, Name: name, Arguments: fields},
	}}
}

// UsageEvent is what one assistant turn billed, which the worker writes to
// the ledger per turn.
func UsageEvent(requestID string, usage harness.Usage) *harnessv1.Event {
	return &harnessv1.Event{RequestId: requestID, Body: &harnessv1.Event_Usage{Usage: usage.Proto()}}
}
