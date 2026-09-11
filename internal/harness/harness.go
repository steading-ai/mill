// Package harness is the seam between mill and whatever runs a skill step as
// a headless agent session.
//
// Mill does not implement an agent runtime. It drives one through the
// [Harness] interface, which docs/architecture.md §"Skill steps: the harness
// adapter" defines: Start, Resume, Cancel, and a [Turn] that streams events
// and ends with exactly one [Terminal] record.
//
// Two things about that contract are load-bearing. A [ProcessExit] whose
// Status is -1 means the session never ran and Reason says why, which is what
// keeps "never ran" apart from "ran and failed" for retry policy. And [Usage]
// is part of the return value, not an optional event: the adapter is the only
// component that sees the harness's numbers, so an adapter that drops them
// has left the ledger and dagger with nothing to record.
package harness

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	harnessv1 "github.com/ivy/mill/gen/harness/v1"
)

// Workspace is the entire environment a session is given: a unix socket and
// a step token. Everything the session does to the world goes through that
// socket, gated by the step's capability set.
type Workspace struct {
	// Socket is the path to the workspace's unix socket.
	Socket string
	// Token is the step token, the session's only credential. It is scoped
	// to one step run and revoked when that step run leaves `running`.
	Token string
	// Env is what the workspace requires the session's own processes to run
	// under: the run's commit identity and the mill's signing key
	// (docs/architecture.md §"The workspace"), and the proxy and the model
	// gateway the session reaches out through, with the step token as the
	// gateway's bearer (docs/sandbox.md §Network, §Credentials). An adapter
	// sets it last, with the socket and the token, so nothing a step asked
	// for can shadow it — a session's `git commit` is the workspace's commit
	// whether the session meant it to be or not, and its way out is the
	// workspace's route.
	Env map[string]string
}

// Sink receives a turn's events as they arrive, in the order the harness
// produced them. The caller supplies it because the events belong to the step
// run's log — one file per step run, through the redactor, per
// docs/architecture.md §Observability — and not to the adapter. A nil Sink
// discards them.
//
// Events carry the wire's own types: nothing an event says is evidence, so
// there is no domain shape for them to be translated into. Only the terminal
// record becomes something the ledger stores.
type Sink func(*harnessv1.Event)

// Send delivers one event, tolerating the caller that wanted none.
func (s Sink) Send(event *harnessv1.Event) {
	if s == nil {
		return
	}
	s(event)
}

// Session is the session a call runs in, and everything about it that is not
// the prompt or its attachments.
//
// Options travel here because they are per-turn: a step may resume a session
// under different options than started it, so they are re-sent with every
// call rather than fixed when the session was created.
type Session struct {
	// ID is the deterministic session id: UUIDv5 of the run's context id,
	// generation, and the step's session name. Resume finds the transcript
	// by it, and a restart's new generation yields ids the harness has never
	// seen.
	ID string
	// Options is the step's options document for the harness this session
	// runs in: its entry in the step's `harness` table, as the workflow
	// wrote it.
	//
	// Mill reads nothing inside it and the adapter passes it through. What a
	// key means, and whether the harness has one by that name at all, is the
	// harness's to say — it validates the document and refuses what it does
	// not know rather than starting a session under settings it guessed at
	// (docs/domain-model.md §Step).
	Options map[string]any
	// Workspace is the socket and token the session is handed.
	Workspace Workspace
	// Cwd is the working directory inside the workspace's checkout.
	Cwd string
	// Timeout is the step's `timeout`: the wall clock this turn runs under,
	// and so the longest a command the session runs in the foreground may
	// take. An adapter hands it to the harness as the session's default
	// shell timeout, so a session waits for a test suite in the turn that
	// started it rather than having it backgrounded underneath it at the
	// harness's own default — the wait the heartbeat then has to watch.
	// Every step declares one (internal/workflow, Validate), and an adapter
	// refuses a session without one rather than guessing a budget.
	Timeout time.Duration
	// Events receives this turn's events as they arrive.
	Events Sink
}

// Prompt is what the session is asked to do.
type Prompt struct {
	// Text is the rendered prompt: the skill body, plus the node's body and
	// inputs.
	Text string
	// OutputSchema is the JSON Schema the session's final output must
	// validate against, when the step declares one. Empty when the step
	// wants no structured output.
	OutputSchema string
}

// Attachment is one piece of context handed to the session alongside the
// prompt: a prior step's artifact, or a predecessor node's result. Source
// names the principal making the claim, because predecessor content is
// rendered as attributed data and never as instructions.
type Attachment struct {
	Name      string
	Source    string
	MediaType string
	Content   string
}

// Harness runs a skill step as a session. docs/architecture.md defines this
// interface; the adapter, not the workflow, translates a skill into whatever
// the harness wants.
type Harness interface {
	// Start begins a session that has never run.
	Start(ctx context.Context, session Session, prompt Prompt, attachments []Attachment) (*Turn, error)
	// Resume continues an existing session with a new prompt, so a revise
	// returns to the implementing session and the model picks up its own
	// prior context.
	Resume(ctx context.Context, session Session, prompt Prompt, attachments []Attachment) (*Turn, error)
	// Cancel stops a running session: interrupt, a grace window, kill.
	// Cancelling a session that is not running is not an error.
	Cancel(ctx context.Context, session Session) error
}

// ExitReason says why a session ended when the exit status alone does not. It
// is the discriminator that keeps "never ran" apart from "ran and failed".
type ExitReason string

const (
	// ReasonNone is the zero value: the process ran and its status is the
	// whole story.
	ReasonNone ExitReason = ""
	// ReasonRefused means the harness declined to start the session at all.
	ReasonRefused ExitReason = "refused"
	// ReasonPolicy means the options the step asked for could not be
	// applied, so the session was never started under them.
	ReasonPolicy ExitReason = "policy"
	// ReasonTimeout means the step's wall clock elapsed and the worker
	// stopped the session.
	ReasonTimeout ExitReason = "timeout"
	// ReasonCancelled means a human or the worker cancelled the run.
	ReasonCancelled ExitReason = "cancelled"
	// ReasonStalled means the session ended a turn with a background task
	// that then went silent through every heartbeat, and the harness's
	// breaker killed it. The session ran, so the attempt failed rather than
	// never started, and the last working turn's words are its final
	// message (docs/domain-model.md §Session).
	ReasonStalled ExitReason = "stalled"
)

// reasons is the mapping both ways between the wire's enum and the string the
// ledger stores.
var reasons = map[harnessv1.ExitReason]ExitReason{
	harnessv1.ExitReason_EXIT_REASON_UNSPECIFIED: ReasonNone,
	harnessv1.ExitReason_EXIT_REASON_REFUSED:     ReasonRefused,
	harnessv1.ExitReason_EXIT_REASON_POLICY:      ReasonPolicy,
	harnessv1.ExitReason_EXIT_REASON_TIMEOUT:     ReasonTimeout,
	harnessv1.ExitReason_EXIT_REASON_CANCELLED:   ReasonCancelled,
	harnessv1.ExitReason_EXIT_REASON_STALLED:     ReasonStalled,
}

// ReasonFrom translates the wire's enum into the reason the ledger stores.
func ReasonFrom(reason harnessv1.ExitReason) ExitReason {
	return reasons[reason]
}

// Proto is the reason as the wire spells it.
func (r ExitReason) Proto() harnessv1.ExitReason {
	for wire, reason := range reasons {
		if reason == r {
			return wire
		}
	}
	return harnessv1.ExitReason_EXIT_REASON_UNSPECIFIED
}

// ProcessExit is how a session ended. Borrowed wholesale from
// buildkite-agent, including the convention that a negative status means the
// process never ran.
type ProcessExit struct {
	// Status is the exit status, or -1 when the session never ran.
	Status int32
	// Signal is the signal that killed the process, or 0 if none did.
	Signal int32
	// Reason says why the session ended when the status does not. Set
	// whenever Status is -1.
	Reason ExitReason
}

// NeverRan reports whether the session never reached the point of producing a
// status of its own, which is what retry policy branches on.
func (e ProcessExit) NeverRan() bool { return e.Status < 0 }

// TokenCounts is one model's token usage. Counts, never currency: a rate
// table is applied by whoever wants money.
type TokenCounts struct {
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
}

// Usage is what a turn or a whole session cost, keyed by the harness's own
// model name. It is a map because one assistant turn bills an auxiliary model
// alongside the one the step asked for, and mill posts one dagger usage
// record per (lease, model).
type Usage map[string]TokenCounts

// Models are the models billed, sorted, so a caller can iterate a usage map
// in a stable order.
func (u Usage) Models() []string {
	models := make([]string, 0, len(u))
	for model := range u {
		models = append(models, model)
	}
	sort.Strings(models)
	return models
}

// UsageFrom translates the wire's usage into the ledger's shape. A nil
// message is no usage rather than an error: a session that never ran billed
// nothing.
func UsageFrom(usage *harnessv1.Usage) Usage {
	if len(usage.GetByModel()) == 0 {
		return nil
	}
	out := make(Usage, len(usage.GetByModel()))
	for model, counts := range usage.GetByModel() {
		out[model] = TokenCounts{
			Input:      counts.GetInput(),
			Output:     counts.GetOutput(),
			CacheRead:  counts.GetCacheRead(),
			CacheWrite: counts.GetCacheWrite(),
		}
	}
	return out
}

// Proto is the usage as the wire spells it.
func (u Usage) Proto() *harnessv1.Usage {
	if len(u) == 0 {
		return nil
	}
	byModel := make(map[string]*harnessv1.TokenCounts, len(u))
	for model, counts := range u {
		byModel[model] = &harnessv1.TokenCounts{
			Input:      counts.Input,
			Output:     counts.Output,
			CacheRead:  counts.CacheRead,
			CacheWrite: counts.CacheWrite,
		}
	}
	return &harnessv1.Usage{ByModel: byModel}
}

// Terminal is how a turn ended and what it produced. Exactly one follows each
// Start or Resume, and it is the only thing the worker turns into evidence.
type Terminal struct {
	// RequestID correlates the record with the request that produced it.
	RequestID string
	// FinalMessage is the session's last assistant message. When the session
	// never ran it is what the adapter could say about why.
	FinalMessage string
	// StructuredOutput is the final output parsed against the step's schema,
	// when the step declared one. Nil otherwise, and nil when parsing
	// failed — Exit says so.
	StructuredOutput map[string]any
	// Diagnostics is what the harness's own process wrote to its standard
	// error, when the session refused or crashed. It is the harness's, not
	// the session's: a sidecar that died before it could write a terminal
	// record left its stack trace here and nowhere else, and the step's log
	// is where an operator goes looking for it
	// (docs/architecture.md §"Failure and stop"). Empty for a session that
	// ended cleanly.
	Diagnostics string
	// Usage is what the whole session cost, per model. The harness's own
	// total, not a running sum of the usage events.
	Usage Usage
	// Exit is how the session ended.
	Exit ProcessExit
}

// TerminalFrom translates a wire terminal record into the ledger's shape.
func TerminalFrom(terminal *harnessv1.Terminal) *Terminal {
	if terminal == nil {
		return nil
	}
	out := &Terminal{
		RequestID:    terminal.GetRequestId(),
		FinalMessage: terminal.GetFinalMessage(),
		Usage:        UsageFrom(terminal.GetUsage()),
		Exit: ProcessExit{
			Status: terminal.GetExit().GetStatus(),
			Signal: terminal.GetExit().GetSignal(),
			Reason: ReasonFrom(terminal.GetExit().GetReason()),
		},
	}
	if structured := terminal.GetStructuredOutput(); structured != nil {
		out.StructuredOutput = structured.AsMap()
	}
	return out
}

// Environment is an adapter's record of what it gave the process it spawned
// of the mill's own environment. It is a record, not a setting: the decision
// is the adapter's and is already made by the time a turn carries it, and
// the ledger keeps it so a reviewer can see what a workflow asked for and
// what was taken away (docs/architecture.md §"Skill steps: the harness
// adapter").
type Environment struct {
	// Passthrough are the variables the step asked to have carried from the
	// mill's environment into its session's process.
	Passthrough []string
	// Stripped are the deny-listed variables that were present in the
	// mill's environment and removed.
	Stripped []string
}

// Turn is one Start or Resume in flight. Its events reach the session's sink
// as they arrive; it ends with exactly one terminal record, which is what
// Wait returns.
type Turn struct {
	// Environment is what the adapter gave the spawned process, set before
	// the turn is returned and never written again.
	Environment Environment

	done     chan struct{}
	once     sync.Once
	terminal *Terminal
	err      error
}

// NewTurn is how an adapter creates the turn it is about to fill.
func NewTurn() *Turn {
	return &Turn{done: make(chan struct{})}
}

// Finish ends the turn with its terminal record, or with the error that
// stopped the adapter from producing one. Only the first call counts, so a
// cancellation racing a terminal line cannot rewrite what the session said.
func (t *Turn) Finish(terminal *Terminal, err error) {
	t.once.Do(func() {
		t.terminal, t.err = terminal, err
		close(t.done)
	})
}

// Done is closed when the turn has ended.
func (t *Turn) Done() <-chan struct{} { return t.done }

// Wait blocks until the turn ends and returns its terminal record. A context
// that is cancelled first returns its error and leaves the turn running: use
// [Harness.Cancel] to stop the session itself.
func (t *Turn) Wait(ctx context.Context) (*Terminal, error) {
	select {
	case <-t.done:
		return t.terminal, t.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// ErrUnknownHarness is returned when a step names a harness nothing
// implements.
var ErrUnknownHarness = errors.New("harness: unknown harness")

// Registry maps a harness name to its implementation. A step's `harness`
// table is keyed by these names and the adapter reads its own entry.
//
// There is one entry in O0. This is a lookup table, not plugin discovery.
type Registry struct {
	mu sync.RWMutex
	by map[string]Harness
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{by: map[string]Harness{}}
}

// Register adds an implementation under a name. Registering a name twice is
// an error: two adapters answering to one name would make which one a step
// got depend on start order.
func (r *Registry) Register(name string, implementation Harness) error {
	if name == "" {
		return errors.New("harness: a harness needs a name")
	}
	if implementation == nil {
		return fmt.Errorf("harness: %q has no implementation", name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.by == nil {
		r.by = map[string]Harness{}
	}
	if _, taken := r.by[name]; taken {
		return fmt.Errorf("harness: %q is already registered", name)
	}
	r.by[name] = implementation
	return nil
}

// Lookup returns the implementation registered under a name.
func (r *Registry) Lookup(name string) (Harness, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	implementation, found := r.by[name]
	if !found {
		return nil, fmt.Errorf("%w: %q", ErrUnknownHarness, name)
	}
	return implementation, nil
}

// Names are the registered harness names, sorted.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.by))
	for name := range r.by {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
