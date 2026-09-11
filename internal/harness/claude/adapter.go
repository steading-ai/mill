// Package claude drives the TypeScript sidecar over the Claude Agent
// SDK: the first implementation of [harness.Harness].
//
// The sidecar is a child process spawned per step and spoken to over
// newline-delimited protojson on stdio — a lifecycle that comes for free,
// since a child dies with its parent and needs no port. See
// docs/architecture.md §"Skill steps: the harness adapter" and sidecar/README.md.
//
// Every process goes out through [workspace.Execer], which is why this
// package does not import os/exec: the workspace is the single chokepoint
// where cwd, uid and environment are decided, and the forbidigo rule in
// .golangci.yml makes that checkable rather than aspirational.
package claude

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"syscall"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	harnessv1 "github.com/ivy/mill/gen/harness/v1"
	"github.com/ivy/mill/internal/harness"
	"github.com/ivy/mill/internal/workspace"
)

const (
	// Name is what a step's `harness` table calls this adapter.
	Name = "claude"

	// DefaultCommand and DefaultEntrypoint are what the sidecar is spawned
	// as. sidecar/main.ts exists so this path is literal and stable.
	DefaultCommand    = "node"
	DefaultEntrypoint = "sidecar/dist/main.js"

	// DefaultGrace is how long a session may take to stop cleanly after the
	// interrupt before it is killed. docs/architecture.md §"Failure and
	// stop" names the window — interrupt, grace, kill — without naming a
	// number; this is the sidecar's own default, so the two ends agree when
	// neither is configured.
	DefaultGrace = 5 * time.Second

	// socketVar and tokenVar are the session's entire grant: a socket to
	// speak to and a bearer to present on it. They are set on every sidecar
	// whatever the environment mode, and they are set last, so nothing a
	// step asked for can shadow them.
	socketVar = "WORKSPACE_SOCKET"
	tokenVar  = "STEP_TOKEN"

	// maxLine bounds one wire frame. A tool result is truncated by the
	// sidecar long before this.
	maxLine = 8 << 20

	// stderrLimit is how much of the sidecar's standard error is kept. A
	// sidecar that crashes writes its stack trace here and nowhere else, so
	// the whole stream is kept rather than a line of it; the bound is what
	// stops a sidecar that loops on a warning from growing the worker's
	// memory without limit.
	stderrLimit = 64 << 10

	// messageLines is how many of stderr's last lines a failure with no
	// terminal record carries as its message. One line of a Node crash is
	// always `Node.js v24.x`; twenty is the stack that preceded it.
	messageLines = 20
)

// Option configures an [Adapter].
type Option func(*Adapter)

// WithExecer sets where processes are spawned. The default is the local
// workspace.
func WithExecer(execer workspace.Execer) Option {
	return func(a *Adapter) { a.execer = execer }
}

// WithSandbox marks the execer a sandbox that supplies its own base
// environment — the machine backend, whose [workspace.Execer] enters a
// systemd-nspawn machine where `agent`'s HOME, PATH and the rest are the
// machine's, not the host's (docs/sandbox.md §Exec, §"Session identity").
//
// A sandboxed sidecar is handed only the session's own grant — its socket
// and step token — the workspace's egress and identity, and whatever the
// step's `env` carved out; the host floor is never copied in, because the
// host's HOME and PATH name nothing inside a machine. Without it a session
// would run with `HOME=/home/<operator>` and a PATH of host directories the
// machine does not have, and `~/.claude` would resolve to a home that is not
// there.
func WithSandbox() Option {
	return func(a *Adapter) { a.sandbox = true }
}

// WithEntrypoint sets the command the sidecar is spawned as.
func WithEntrypoint(command string, args ...string) Option {
	return func(a *Adapter) { a.command, a.args = command, args }
}

// WithGrace sets how long a cancelled session may take to stop cleanly
// before it is killed.
func WithGrace(grace time.Duration) Option {
	return func(a *Adapter) { a.grace = grace }
}

// WithEnvMode sets where a sidecar's environment comes from:
// [EnvInherit], the default, or [EnvStrict].
func WithEnvMode(mode EnvMode) Option {
	return func(a *Adapter) { a.envMode = mode }
}

// Adapter spawns one sidecar per turn and speaks harness.v1 to it.
type Adapter struct {
	execer  workspace.Execer
	command string
	args    []string
	grace   time.Duration
	envMode EnvMode
	// sandbox is set when the execer enters a sandbox that supplies its own
	// base environment, so the host floor is never copied in (WithSandbox).
	sandbox bool

	mu   sync.Mutex
	live map[string]*flight
}

// New returns an adapter that spawns the sidecar through the local
// workspace.
func New(options ...Option) *Adapter {
	adapter := &Adapter{
		execer:  workspace.Spawner{},
		command: DefaultCommand,
		args:    []string{DefaultEntrypoint},
		grace:   DefaultGrace,
		envMode: DefaultEnvMode,
		live:    map[string]*flight{},
	}
	for _, option := range options {
		option(adapter)
	}
	return adapter
}

// Start begins a session that has never run.
func (a *Adapter) Start(ctx context.Context, session harness.Session, prompt harness.Prompt, attachments []harness.Attachment) (*harness.Turn, error) {
	settings, err := wireSettings(session)
	if err != nil {
		return nil, err
	}
	timeout, err := wireTimeout(session)
	if err != nil {
		return nil, err
	}
	return a.begin(ctx, session, &harnessv1.Request{Id: requestID(), Body: &harnessv1.Request_Start{Start: &harnessv1.Start{
		SessionId:    session.ID,
		Prompt:       prompt.Text,
		Attachments:  wireAttachments(attachments),
		Settings:     settings,
		Workspace:    wireWorkspace(session),
		OutputSchema: prompt.OutputSchema,
		Cwd:          session.Cwd,
		TimeoutMs:    timeout,
		SystemPrompt: systemPrompt(session),
	}}})
}

// Resume continues an existing session with a new prompt.
func (a *Adapter) Resume(ctx context.Context, session harness.Session, prompt harness.Prompt, attachments []harness.Attachment) (*harness.Turn, error) {
	settings, err := wireSettings(session)
	if err != nil {
		return nil, err
	}
	timeout, err := wireTimeout(session)
	if err != nil {
		return nil, err
	}
	return a.begin(ctx, session, &harnessv1.Request{Id: requestID(), Body: &harnessv1.Request_Resume{Resume: &harnessv1.Resume{
		SessionId:    session.ID,
		Prompt:       prompt.Text,
		Attachments:  wireAttachments(attachments),
		Settings:     settings,
		Workspace:    wireWorkspace(session),
		OutputSchema: prompt.OutputSchema,
		Cwd:          session.Cwd,
		TimeoutMs:    timeout,
		SystemPrompt: systemPrompt(session),
	}}})
}

// Cancel stops a running session: SIGTERM to its process group, the grace
// window, then SIGKILL. It returns once the group is gone, so a caller that
// has been told the session stopped can rely on there being no orphan.
// Cancelling a session that is not running is not an error.
func (a *Adapter) Cancel(ctx context.Context, session harness.Session) error {
	running := a.lookup(session.ID)
	if running == nil {
		return nil
	}
	running.stop(harness.ReasonCancelled)
	select {
	case <-running.exited:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *Adapter) begin(ctx context.Context, session harness.Session, request *harnessv1.Request) (*harness.Turn, error) {
	line, err := encodeRequest(request)
	if err != nil {
		return nil, err
	}

	// The whole of the child's environment, decided by the mode and refused
	// before anything is spawned when the step asked for something it may
	// not have (environment.go).
	env, decided, err := a.environment(session)
	if err != nil {
		return nil, err
	}
	process, err := a.execer.Exec(ctx, workspace.Command{
		Path:     a.command,
		Args:     a.args,
		Dir:      session.Cwd,
		Env:      env,
		ClearEnv: true,
	})
	if err != nil {
		return nil, fmt.Errorf("claude: spawn sidecar: %w", err)
	}

	running := &flight{
		adapter:   a,
		id:        session.ID,
		requestID: request.GetId(),
		process:   process,
		sink:      session.Events,
		grace:     a.grace,
		timeout:   session.Timeout,
		turn:      harness.NewTurn(),
		exited:    make(chan struct{}),
	}
	running.turn.Environment = decided
	if err := a.track(running); err != nil {
		running.kill()
		return nil, err
	}

	// The pipe stays open for the life of the step: a worker holds it, and a
	// sidecar that read EOF as a reason to stop would never see a signal.
	//
	// A write that fails is a sidecar that has already gone — it refused
	// before it read its request — and not a spawn that did not happen. Once
	// a process exists the turn owns it, so the failure is noted and the
	// pump resolves it into a terminal record like any other refusal, rather
	// than leaving the caller with an error and no record of the step.
	if _, err := process.Stdin.Write(line); err != nil {
		running.note(fmt.Sprintf("the sidecar did not read its request: %v", err))
	}

	go running.pump(ctx)
	return running.turn, nil
}

func (a *Adapter) track(running *flight) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, busy := a.live[running.id]; busy {
		return fmt.Errorf("claude: session %q is already running", running.id)
	}
	a.live[running.id] = running
	return nil
}

func (a *Adapter) forget(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.live, id)
}

func (a *Adapter) lookup(id string) *flight {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.live[id]
}

// flight is one sidecar in flight: the process, the turn it fills, and the
// state the stop path and the terminal record are resolved from.
type flight struct {
	adapter   *Adapter
	id        string
	requestID string
	process   *workspace.Process
	sink      harness.Sink
	grace     time.Duration
	// timeout is the step's wall clock, after which the session is stopped
	// with ReasonTimeout. Zero only for a Probe, which starts no session.
	timeout time.Duration
	turn    *harness.Turn

	exited   chan struct{}
	stopOnce sync.Once

	// stderr is everything the sidecar wrote to its standard error, bounded
	// to the last [stderrLimit] bytes. It is written by one goroutine and
	// read once the process has exited, and holds its own lock.
	stderr tail

	mu       sync.Mutex
	reason   harness.ExitReason
	terminal *harness.Terminal
	streamed harness.Usage
	lastErr  string
}

// pump reads the sidecar's stdout until it ends, streams the events to the
// caller's sink as they arrive, and finishes the turn with the terminal
// record — synthesising one when the sidecar produced none.
func (s *flight) pump(ctx context.Context) {
	defer s.adapter.forget(s.id)

	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		s.readStderr()
	}()

	if s.timeout > 0 {
		expired := time.AfterFunc(s.timeout, func() { s.stop(harness.ReasonTimeout) })
		defer expired.Stop()
	}

	// A cancelled context is the worker dropping the run; the session stops
	// with it rather than outliving what asked for it.
	watching := make(chan struct{})
	defer close(watching)
	go func() {
		select {
		case <-ctx.Done():
			s.stop(harness.ReasonCancelled)
		case <-watching:
		}
	}()

	scanner := bufio.NewScanner(s.process.Stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLine)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		response, err := decodeResponse(line)
		if err != nil {
			// A line the wire cannot parse is not evidence and is not fatal:
			// it is noted so a synthesised refusal can say what arrived.
			s.note(err.Error())
			continue
		}
		s.deliver(response)
	}

	<-stderrDone
	_ = s.process.Stdin.Close()
	result := s.process.Wait()
	close(s.exited)

	s.turn.Finish(s.resolve(result), nil)
}

func (s *flight) deliver(response *harnessv1.Response) {
	switch {
	case response.GetEvent() != nil:
		if usage := response.GetEvent().GetUsage(); usage != nil {
			s.accumulate(harness.UsageFrom(usage))
		}
		s.sink.Send(response.GetEvent())
	case response.GetTerminal() != nil:
		s.mu.Lock()
		s.terminal = harness.TerminalFrom(response.GetTerminal())
		s.mu.Unlock()
	case response.GetFailure() != nil:
		// The sidecar could not handle the request at all. It carries no
		// exit of its own, so it becomes the message of a refusal.
		s.note(response.GetFailure().GetMessage())
	}
}

// readStderr keeps everything the sidecar wrote to its standard error, less
// whatever the bound dropped off the front. It is what a refusal has to say
// for itself when the process died before writing a terminal record, and it
// is the crash's stack trace when the crash was a crash.
func (s *flight) readStderr() {
	_, _ = io.Copy(&s.stderr, s.process.Stderr)
}

// accumulate sums what a turn billed. The terminal record's own total is
// authoritative and this is never added to it; it is what the adapter has to
// report when the session was killed before it could write one, so a
// cancelled step still costs what it cost.
func (s *flight) accumulate(usage harness.Usage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.streamed == nil {
		s.streamed = harness.Usage{}
	}
	for model, counts := range usage {
		running := s.streamed[model]
		running.Input += counts.Input
		running.Output += counts.Output
		running.CacheRead += counts.CacheRead
		running.CacheWrite += counts.CacheWrite
		s.streamed[model] = running
	}
}

func (s *flight) note(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastErr = line
}

// resolve is the terminal record the turn ends with: the sidecar's own, or
// one the adapter writes when the sidecar produced none.
//
// A stop the adapter started wins over the reason in the record. The sidecar
// reports every stop as `cancelled` — a signal carries nothing that says
// which — and the adapter is the only end that knows a timeout from a
// cancellation.
func (s *flight) resolve(result workspace.Result) *harness.Terminal {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.terminal != nil {
		if s.reason != harness.ReasonNone && s.terminal.Exit.NeverRan() {
			s.terminal.Exit.Reason = s.reason
		}
		// A session that ended cleanly has nothing to explain, and its
		// stderr is the sidecar's own chatter. A refusal has everything to
		// explain, so what the sidecar wrote goes with the record.
		if s.terminal.Exit.NeverRan() || s.terminal.Exit.Status != 0 {
			s.terminal.Diagnostics = s.stderr.String()
		}
		return s.terminal
	}

	reason := s.reason
	message := words(s.stderr.lines(messageLines), s.lastErr)
	if reason == harness.ReasonNone {
		reason = harness.ReasonRefused
		if message == "" {
			message = fmt.Sprintf("the sidecar exited with status %d and wrote no terminal record", result.Status)
		}
	} else {
		stopped := fmt.Sprintf("the session was stopped (%s) and wrote no terminal record", reason)
		if message != "" {
			stopped += ": " + message
		}
		message = stopped
	}

	return &harness.Terminal{
		RequestID:    s.requestID,
		FinalMessage: message,
		Diagnostics:  s.stderr.String(),
		Usage:        s.streamed,
		Exit: harness.ProcessExit{
			Status: -1,
			Signal: result.Signal,
			Reason: reason,
		},
	}
}

// words is what a failure with no terminal record says for itself: the tail
// of the sidecar's standard error, or — when it wrote none — the adapter's
// own note about what happened on the wire.
//
// Stderr wins because it is the cause and the note is usually the symptom. A
// sidecar that throws on startup dies before reading its request, so the
// note is "the sidecar did not read its request: broken pipe" while the
// stack trace that explains it is on stderr. Nothing is lost either way: the
// whole stream reaches the step's log.
func words(stderr, note string) string {
	if stderr != "" {
		return stderr
	}
	return note
}

// tail is an [io.Writer] keeping the last [stderrLimit] bytes written to it.
// It is what "the sidecar's whole stderr, bounded" means: a stream longer
// than the bound loses its beginning, because a crash's last words are at
// the end.
type tail struct {
	mu   sync.Mutex
	buf  []byte
	over bool
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > stderrLimit {
		t.buf = t.buf[len(t.buf)-stderrLimit:]
		t.over = true
	}
	return len(p), nil
}

// String is what was kept, with a marker when the bound dropped anything, so
// a reader is not left thinking a stack trace began where it was cut.
func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.buf) == 0 {
		return ""
	}
	if t.over {
		return fmt.Sprintf("[…the first bytes of stderr were dropped; the last %d are kept]\n%s",
			stderrLimit, t.buf)
	}
	return string(t.buf)
}

// lines is the last n non-empty lines kept, newest last: the message a
// terminal-less failure carries.
func (t *tail) lines(n int) string {
	t.mu.Lock()
	kept := string(t.buf)
	t.mu.Unlock()

	var out []string
	for _, line := range strings.Split(kept, "\n") {
		if line = strings.TrimRight(line, "\r"); strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return strings.Join(out, "\n")
}

// stop is interrupt, grace, kill. It returns as soon as the interrupt is
// sent; the escalation runs on its own and s.exited is what says the group
// is gone.
func (s *flight) stop(reason harness.ExitReason) {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.reason = reason
		s.mu.Unlock()

		_ = s.process.Signal(syscall.SIGTERM)
		go func() {
			select {
			case <-s.exited:
			case <-time.After(s.grace):
				_ = s.process.Signal(syscall.SIGKILL)
			}
		}()
	})
}

// kill ends a sidecar that never reached its pump, so a failure to hand it
// the request leaves nothing behind.
func (s *flight) kill() {
	_ = s.process.Signal(syscall.SIGKILL)
	_ = s.process.Stdin.Close()
	_, _ = io.Copy(io.Discard, s.process.Stdout)
	_, _ = io.Copy(io.Discard, s.process.Stderr)
	s.process.Wait()
}

func wireWorkspace(session harness.Session) *harnessv1.Workspace {
	return &harnessv1.Workspace{Socket: session.Workspace.Socket, Token: session.Workspace.Token}
}

// wireSettings carries the step's options document to the sidecar as it
// stands. The adapter reads no key of it: which keys the sidecar has, and
// what each one does, is the sidecar's own schema, and a key this side does
// not recognise is one the sidecar refuses rather than one the adapter drops
// on the way (docs/architecture.md §"Skill steps: the harness adapter").
//
// The one failure here is a document that has no JSON form at all, which the
// loader has already refused: a Struct is what the wire can carry.
func wireSettings(session harness.Session) (*harnessv1.Settings, error) {
	if len(session.Options) == 0 {
		return nil, nil
	}
	options, err := structpb.NewStruct(session.Options)
	if err != nil {
		return nil, fmt.Errorf("claude: the step's options are not a JSON document: %w", err)
	}
	return &harnessv1.Settings{Options: options}, nil
}

// wireTimeout carries the step's budget to the sidecar as the session's
// default shell timeout, in milliseconds. A step without one is refused
// before anything is spawned: the budget is the workflow's to declare
// (internal/workflow, Validate), and an adapter that filled one in would be
// deciding how long a session may wait in the foreground, which is the
// decision that lost sessions to poll loops (docs/sandbox.md §Preparatory
// steps). The field is an int32, which holds a budget of over twenty days;
// one past that is not a step.
func wireTimeout(session harness.Session) (int32, error) {
	if session.Timeout <= 0 {
		return 0, fmt.Errorf("claude: session %s has no timeout; every step declares one", session.ID)
	}
	ms := session.Timeout.Milliseconds()
	if ms > math.MaxInt32 {
		return 0, fmt.Errorf("claude: session %s: a timeout of %s is longer than the wire carries", session.ID, session.Timeout)
	}
	return int32(ms), nil
}

func wireAttachments(attachments []harness.Attachment) []*harnessv1.Attachment {
	if len(attachments) == 0 {
		return nil
	}
	wire := make([]*harnessv1.Attachment, 0, len(attachments))
	for _, attachment := range attachments {
		wire = append(wire, &harnessv1.Attachment{
			Name:      attachment.Name,
			Source:    attachment.Source,
			MediaType: attachment.MediaType,
			Content:   attachment.Content,
		})
	}
	return wire
}

// requestID correlates one request with the events and terminal record it
// produces. The mill- prefix is the same marker its lease-less dagger writes
// carry, so a frame in a log says who sent it.
func requestID() string {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		// crypto/rand does not fail on the platforms mill runs on; a
		// correlation id is not worth a panic if it ever does.
		return "mill-" + fmt.Sprint(time.Now().UnixNano())
	}
	return "mill-" + hex.EncodeToString(raw)
}

// Probe starts the sidecar, asks it for a no-op, and reports what it
// answered. `mill doctor` uses it: a sidecar that starts, parses one frame of
// newline-delimited protojson and writes one back is a sidecar a step can be
// given, and nothing short of running it proves that.
//
// The no-op is a `cancel` request. Cancellation reaches a running sidecar as
// SIGTERM, so a cancel on stdin is a frame the sidecar understands and
// refuses by contract — it never starts a session, never reaches the Claude
// API, and never bills anything. Its refusal is the answer this probe wants.
func (a *Adapter) Probe(ctx context.Context) (*harness.Terminal, error) {
	line, err := encodeRequest(&harnessv1.Request{
		Id:   requestID(),
		Body: &harnessv1.Request_Cancel{Cancel: &harnessv1.Cancel{}},
	})
	if err != nil {
		return nil, err
	}
	env, _, err := a.environment(harness.Session{})
	if err != nil {
		return nil, err
	}
	process, err := a.execer.Exec(ctx, workspace.Command{Path: a.command, Args: a.args, Env: env, ClearEnv: true})
	if err != nil {
		return nil, fmt.Errorf("claude: spawn sidecar: %w", err)
	}
	defer func() {
		_ = process.Signal(syscall.SIGKILL)
		_, _ = io.Copy(io.Discard, process.Stderr)
		process.Wait()
	}()

	if _, err := process.Stdin.Write(line); err != nil {
		return nil, fmt.Errorf("claude: the sidecar did not read its request: %w", err)
	}
	_ = process.Stdin.Close()

	reader := bufio.NewReaderSize(process.Stdout, maxLine)
	for {
		raw, err := reader.ReadBytes('\n')
		if len(bytes.TrimSpace(raw)) > 0 {
			response, decodeErr := decodeResponse(bytes.TrimSpace(raw))
			if decodeErr != nil {
				return nil, decodeErr
			}
			if terminal := response.GetTerminal(); terminal != nil {
				return harness.TerminalFrom(terminal), nil
			}
			if failure := response.GetFailure(); failure != nil {
				return nil, fmt.Errorf("claude: the sidecar refused: %s", failure.GetMessage())
			}
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, errors.New("claude: the sidecar answered nothing before it exited")
			}
			return nil, fmt.Errorf("claude: read the sidecar: %w", err)
		}
	}
}
