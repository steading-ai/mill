package scripted

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	harnessv1 "github.com/ivy/mill/gen/harness/v1"
	"github.com/ivy/mill/internal/harness"
	"github.com/ivy/mill/internal/harness/fake"
	"github.com/ivy/mill/internal/workspace"
	"github.com/ivy/mill/internal/workspace/socket"
)

// Adapter is a [harness.Harness] whose turns come from a script file named
// by each session's own `harness.scripted` options, resolved relative to
// the workflow directory it was built with. It keeps no state of its own
// beyond a per-session turn cursor and a cache of parsed script files: every
// call builds a [fake.Script] and delegates to a freshly constructed
// fake.Harness, so replay, terminals and Cancel semantics are exactly
// fake.Harness's.
type Adapter struct {
	workflowDir string

	mu      sync.Mutex
	scripts map[string]Script
	cursors map[string]int
}

var _ harness.Harness = (*Adapter)(nil)

// NewAdapter returns an adapter resolving every session's `script` option
// relative to workflowDir.
func NewAdapter(workflowDir string) *Adapter {
	return &Adapter{workflowDir: workflowDir}
}

// Start begins a session by replaying its script's next turn.
func (a *Adapter) Start(ctx context.Context, session harness.Session, prompt harness.Prompt, attachments []harness.Attachment) (*harness.Turn, error) {
	return a.play(ctx, "start", session, prompt, attachments)
}

// Resume continues a session by replaying its script's next turn.
func (a *Adapter) Resume(ctx context.Context, session harness.Session, prompt harness.Prompt, attachments []harness.Attachment) (*harness.Turn, error) {
	return a.play(ctx, "resume", session, prompt, attachments)
}

// Cancel is a no-op: no turn a scripted session replays ever holds, so
// nothing is ever running for Cancel to reach.
func (a *Adapter) Cancel(context.Context, harness.Session) error {
	return nil
}

func (a *Adapter) play(ctx context.Context, verb string, session harness.Session, prompt harness.Prompt, attachments []harness.Attachment) (*harness.Turn, error) {
	turn, err := a.next(session)
	if err != nil {
		return nil, err
	}
	fakeScript := a.build(turn, prompt)
	h := fake.New(fakeScript)
	if verb == "resume" {
		return h.Resume(ctx, session, prompt, attachments)
	}
	return h.Start(ctx, session, prompt, attachments)
}

// next resolves and loads (once, cached) the session's script, and returns
// the turn its own cursor now points at — clamped to the script's last turn
// once a session runs past what was written for it, matching fake.Harness's
// own tolerance for extra calls.
func (a *Adapter) next(session harness.Session) (Turn, error) {
	path, _ := session.Options["script"].(string)
	if strings.TrimSpace(path) == "" {
		return Turn{}, fmt.Errorf(
			"scripted: session %s: harness.scripted needs a \"script\" option naming a file relative to the workflow directory", session.ID)
	}
	resolved := filepath.Join(a.workflowDir, path)

	a.mu.Lock()
	defer a.mu.Unlock()
	script, cached := a.scripts[resolved]
	if !cached {
		loaded, err := Load(resolved)
		if err != nil {
			return Turn{}, err
		}
		if a.scripts == nil {
			a.scripts = map[string]Script{}
		}
		a.scripts[resolved] = loaded
		script = loaded
	}
	if len(script) == 0 {
		return Turn{}, fmt.Errorf("scripted: %s: no turns", resolved)
	}
	if a.cursors == nil {
		a.cursors = map[string]int{}
	}
	index := a.cursors[session.ID]
	if index >= len(script) {
		index = len(script) - 1
	}
	a.cursors[session.ID] = index + 1
	return script[index], nil
}

// build turns one script Turn into a fake.Script: its events verbatim, plus
// — for every tool_call the turn dispatches — a tool_result event this call
// appends right after it, whose content a single Do closure fills from the
// real workspace before fake.Harness sends any event, exactly as
// fake.Script documents Do running before the send loop.
func (a *Adapter) build(turn Turn, prompt harness.Prompt) fake.Script {
	events := make([]*harnessv1.Event, 0, len(turn.Events)+len(turn.Dispatches))
	type pending struct {
		result   *harnessv1.ToolResult
		dispatch Dispatch
	}
	var pendings []pending
	for i, event := range turn.Events {
		events = append(events, event)
		dispatch, ok := turn.Dispatches[i]
		if !ok {
			continue
		}
		result := &harnessv1.ToolResult{CallId: event.GetToolCall().GetCallId()}
		events = append(events, &harnessv1.Event{Body: &harnessv1.Event_ToolResult{ToolResult: result}})
		pendings = append(pendings, pending{result: result, dispatch: dispatch})
	}

	var do func(harness.Session)
	if len(pendings) > 0 {
		do = func(s harness.Session) {
			client := socket.Dial(s.Workspace.Socket, s.Workspace.Token)
			for _, p := range pendings {
				content, isError := dispatch(context.Background(), client, p.dispatch, prompt)
				p.result.Content = content
				p.result.IsError = isError
			}
		}
	}
	return fake.Script{Events: events, Terminal: turn.Terminal, Do: do}
}

// artifactPathPattern matches artifactBrief's literal
// "Write it to `<path>` in the checkout." (internal/run/artifact.go), the
// only place mill tells a session where it rendered a step's artifact path.
// It is the one thing a script cannot know ahead of a run, so a dispatch
// argument names it as the placeholder ${artifact_path} and this is
// substituted for the real path extracted from the prompt.
var artifactPathPattern = regexp.MustCompile("Write it to `([^`]+)` in the checkout")

// dispatch performs one real workspace verb call and renders its result as
// a tool_result's content and error flag, the same shape a session's own
// tool call would produce.
func dispatch(ctx context.Context, client *socket.Client, d Dispatch, prompt harness.Prompt) (content string, isError bool) {
	args := d.Arguments
	if match := artifactPathPattern.FindStringSubmatch(prompt.Text); match != nil {
		args = substitute(args, "${artifact_path}", match[1])
	}
	switch d.Verb {
	case socket.VerbFSWrite:
		path := argString(args, "path")
		if err := client.Write(ctx, path, []byte(argString(args, "content"))); err != nil {
			return err.Error(), true
		}
		return fmt.Sprintf("wrote %s", path), false
	case socket.VerbGitCommit:
		info, err := client.GitCommit(ctx, workspace.CommitRequest{
			Message:    argString(args, "message"),
			Paths:      argStrings(args, "paths"),
			AllowEmpty: argBool(args, "allow_empty"),
		})
		if err != nil {
			return err.Error(), true
		}
		return info.SHA, false
	default:
		return fmt.Sprintf("scripted: dispatch %q: not implemented", d.Verb), true
	}
}

// substitute returns args with every occurrence of old in a string-valued
// argument replaced by new.
func substitute(args map[string]any, old, new string) map[string]any {
	out := make(map[string]any, len(args))
	for k, v := range args {
		if s, ok := v.(string); ok {
			out[k] = strings.ReplaceAll(s, old, new)
			continue
		}
		out[k] = v
	}
	return out
}

func argString(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return s
}

func argStrings(args map[string]any, key string) []string {
	raw, ok := args[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func argBool(args map[string]any, key string) bool {
	b, _ := args[key].(bool)
	return b
}
