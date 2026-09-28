// Package scripted is a [harness.Harness] whose turns come from a file
// instead of a model: a workflow author writes what each call to Start or
// Resume emits, and every session that names this harness replays exactly
// that. It is reachable only from an acceptance run (see [Name]'s callers),
// because a step closing on canned events is a decision the acceptance
// suite is allowed to make about its own test workflows, not a way for an
// ordinary run to skip real work.
//
// It never reimplements the turn/goroutine/terminal machinery
// [internal/harness/fake] already has: every call builds one [fake.Script]
// and hands it to a freshly constructed fake.Harness.
package scripted

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	harnessv1 "github.com/steading-ai/mill/gen/harness/v1"
	"github.com/steading-ai/mill/internal/harness"
	"github.com/steading-ai/mill/internal/harness/fake"
	"github.com/steading-ai/mill/internal/workspace/socket"
)

// Name is what a step's `harness` table calls this adapter.
const Name = "scripted"

// dispatchVerbs are the workspace verbs a script may ask the adapter to
// call for real. Fixed rather than open: every verb here is one the adapter
// knows how to call and how to turn its response into a tool_result, and a
// script naming anything else is refused at load rather than failing mid-run.
var dispatchVerbs = map[string]bool{
	socket.VerbFSWrite:   true,
	socket.VerbGitCommit: true,
}

// Script is one session's whole replay: one [Turn] per call its
// [harness.Harness] makes, consumed in order.
type Script []Turn

// Turn is what one Start or Resume replays: the events it emits, in order,
// and how it ends.
type Turn struct {
	// Events are handed to fake.Script verbatim.
	Events []*harnessv1.Event
	// Terminal is what the turn ends with. Nil means a session that ran and
	// said nothing, matching fake.Script's own zero value.
	Terminal *harness.Terminal
	// Dispatches maps an index into Events — always a tool_call — to the
	// real verb the adapter calls before that call's tool_result is sent.
	// A script never writes the tool_result for a dispatching call itself:
	// the adapter builds it from the real response.
	Dispatches map[int]Dispatch
	// Delays maps an index into Events to how long the adapter waits,
	// honouring the step's context, before sending that event — a script's
	// `after` field, decoded. A missing entry sends the event with no
	// delay.
	Delays map[int]time.Duration
	// TerminalAfter is how long the adapter waits before ending the turn
	// once every event has been sent — a turn's `terminal.after`, decoded.
	// Zero ends the turn immediately.
	TerminalAfter time.Duration
}

// Dispatch is a real workspace verb a turn's tool_call asks the adapter to
// perform, and the request to call it with.
type Dispatch struct {
	Verb      string
	Arguments map[string]any
}

// Load reads and decodes a script file. A `.json` extension decodes strict
// JSON; anything else decodes YAML — the same rule
// internal/workflow already uses for workflow.yaml, so a workflow author
// picks whichever is easier to hand-write for one file.
func Load(path string) (Script, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("scripted: %w", err)
	}
	var turns []turnYAML
	if strings.EqualFold(filepath.Ext(path), ".json") {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&turns); err != nil {
			return nil, fmt.Errorf("scripted: decode %s: %w", path, err)
		}
	} else {
		decoder := yaml.NewDecoder(bytes.NewReader(raw))
		decoder.KnownFields(true)
		if err := decoder.Decode(&turns); err != nil {
			return nil, fmt.Errorf("scripted: decode %s: %w", path, err)
		}
	}
	script := make(Script, len(turns))
	for i, t := range turns {
		turn, err := t.toTurn()
		if err != nil {
			return nil, fmt.Errorf("scripted: %s: turn %d: %w", path, i, err)
		}
		script[i] = turn
	}
	return script, nil
}

// turnYAML is one turn's on-disk shape.
type turnYAML struct {
	Events   []eventYAML   `yaml:"events" json:"events"`
	Terminal *terminalYAML `yaml:"terminal,omitempty" json:"terminal,omitempty"`
}

// eventYAML is one event's on-disk shape. Kind selects which of the
// remaining fields the decode reads; the rest are ignored.
type eventYAML struct {
	Kind      string `yaml:"kind" json:"kind"`
	RequestID string `yaml:"request_id,omitempty" json:"request_id,omitempty"`
	// After is how long the adapter waits before sending this event, in Go
	// duration syntax. Empty means no wait.
	After string `yaml:"after,omitempty" json:"after,omitempty"`

	// text
	Text string `yaml:"text,omitempty" json:"text,omitempty"`

	// tool_call
	CallID    string         `yaml:"call_id,omitempty" json:"call_id,omitempty"`
	Name      string         `yaml:"name,omitempty" json:"name,omitempty"`
	Arguments map[string]any `yaml:"arguments,omitempty" json:"arguments,omitempty"`
	// Dispatch names a real workspace verb this tool_call performs. When
	// set, Arguments is the verb's request and no paired tool_result event
	// may appear in the file — see Dispatches.
	Dispatch string `yaml:"dispatch,omitempty" json:"dispatch,omitempty"`

	// tool_result. CallID above names the call this answers.
	Content string `yaml:"content,omitempty" json:"content,omitempty"`
	IsError bool   `yaml:"is_error,omitempty" json:"is_error,omitempty"`

	// usage, keyed by model name.
	Usage map[string]tokenCountsYAML `yaml:"usage,omitempty" json:"usage,omitempty"`

	// heartbeat
	Strike         int32    `yaml:"strike,omitempty" json:"strike,omitempty"`
	Strikes        int32    `yaml:"strikes,omitempty" json:"strikes,omitempty"`
	WaitingSeconds int64    `yaml:"waiting_seconds,omitempty" json:"waiting_seconds,omitempty"`
	Tasks          []string `yaml:"tasks,omitempty" json:"tasks,omitempty"`
}

type tokenCountsYAML struct {
	Input      int64 `yaml:"input,omitempty" json:"input,omitempty"`
	Output     int64 `yaml:"output,omitempty" json:"output,omitempty"`
	CacheRead  int64 `yaml:"cache_read,omitempty" json:"cache_read,omitempty"`
	CacheWrite int64 `yaml:"cache_write,omitempty" json:"cache_write,omitempty"`
}

// terminalYAML is a turn's terminal record. A present exit means the
// session never ran (fake.NeverRan); its absence means it ran and finished
// (fake.Terminal).
type terminalYAML struct {
	FinalMessage     string         `yaml:"final_message,omitempty" json:"final_message,omitempty"`
	StructuredOutput map[string]any `yaml:"structured_output,omitempty" json:"structured_output,omitempty"`
	Exit             *exitYAML      `yaml:"exit,omitempty" json:"exit,omitempty"`
	// After is how long the adapter waits, once every event has been sent,
	// before ending the turn — Go duration syntax, empty for no wait.
	After string `yaml:"after,omitempty" json:"after,omitempty"`
}

type exitYAML struct {
	Status int32  `yaml:"status" json:"status"`
	Signal int32  `yaml:"signal,omitempty" json:"signal,omitempty"`
	Reason string `yaml:"reason,omitempty" json:"reason,omitempty"`
}

func (t terminalYAML) toHarness() *harness.Terminal {
	out := &harness.Terminal{
		FinalMessage:     t.FinalMessage,
		StructuredOutput: t.StructuredOutput,
	}
	if t.Exit != nil {
		out.Exit = harness.ProcessExit{
			Status: t.Exit.Status,
			Signal: t.Exit.Signal,
			Reason: harness.ExitReason(t.Exit.Reason),
		}
	}
	return out
}

func (t turnYAML) toTurn() (Turn, error) {
	turn := Turn{}
	for i, e := range t.Events {
		event, dispatch, delay, err := e.toEvent()
		if err != nil {
			return Turn{}, fmt.Errorf("event %d: %w", i, err)
		}
		turn.Events = append(turn.Events, event)
		index := len(turn.Events) - 1
		if dispatch != nil {
			if turn.Dispatches == nil {
				turn.Dispatches = map[int]Dispatch{}
			}
			turn.Dispatches[index] = *dispatch
		}
		if delay > 0 {
			if turn.Delays == nil {
				turn.Delays = map[int]time.Duration{}
			}
			turn.Delays[index] = delay
		}
	}
	if t.Terminal != nil {
		turn.Terminal = t.Terminal.toHarness()
		if t.Terminal.After != "" {
			after, err := time.ParseDuration(t.Terminal.After)
			if err != nil {
				return Turn{}, fmt.Errorf("terminal: after %q: not a valid duration (Go duration syntax, e.g. \"5s\")", t.Terminal.After)
			}
			turn.TerminalAfter = after
		}
	}
	return turn, nil
}

// toEvent decodes one event, the [Dispatch] a dispatching tool_call's
// adapter performs for it, and how long the adapter waits before sending
// it.
func (e eventYAML) toEvent() (*harnessv1.Event, *Dispatch, time.Duration, error) {
	var delay time.Duration
	if e.After != "" {
		parsed, err := time.ParseDuration(e.After)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("after %q: not a valid duration (Go duration syntax, e.g. \"5s\")", e.After)
		}
		delay = parsed
	}
	switch e.Kind {
	case "text":
		return fake.Text(e.RequestID, e.Text), nil, delay, nil
	case "tool_call":
		event := fake.ToolCall(e.RequestID, e.CallID, e.Name, e.Arguments)
		if e.Dispatch == "" {
			return event, nil, delay, nil
		}
		if !dispatchVerbs[e.Dispatch] {
			return nil, nil, 0, fmt.Errorf("dispatch %q: not a verb this harness can call", e.Dispatch)
		}
		return event, &Dispatch{Verb: e.Dispatch, Arguments: e.Arguments}, delay, nil
	case "tool_result":
		return &harnessv1.Event{
			RequestId: e.RequestID,
			Body: &harnessv1.Event_ToolResult{
				ToolResult: &harnessv1.ToolResult{CallId: e.CallID, Content: e.Content, IsError: e.IsError},
			},
		}, nil, delay, nil
	case "usage":
		usage := make(harness.Usage, len(e.Usage))
		for model, counts := range e.Usage {
			usage[model] = harness.TokenCounts{
				Input: counts.Input, Output: counts.Output,
				CacheRead: counts.CacheRead, CacheWrite: counts.CacheWrite,
			}
		}
		return fake.UsageEvent(e.RequestID, usage), nil, delay, nil
	case "heartbeat":
		return fake.Heartbeat(e.RequestID, e.Strike, e.Strikes, e.WaitingSeconds, e.Tasks...), nil, delay, nil
	default:
		return nil, nil, 0, fmt.Errorf("event kind %q is not one of text, tool_call, tool_result, usage, heartbeat", e.Kind)
	}
}
