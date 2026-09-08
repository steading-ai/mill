package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/run"
)

// The tools `mill mcp` offers. Each is one CLI command with its flags as
// arguments, described for the model that will call it.
//
// The readers take `view` and size their answer with it, as dagger's do:
// refs for identifiers, summary for what an operator reads at a glance, full
// for everything the ledger holds. A session that starts with refs and asks
// for full only where it needs to is a session that has not filled its
// context with a backlog.

// viewIdiom is what the views mean, stated once.
const viewIdiom = `view sizes the answer: refs is identifiers alone, summary the fields worth
reading at a glance (the default), full everything the ledger holds.`

// runIdiom is how a run is named, stated once.
const runIdiom = `A run is named by its node reference, such as mill#18, or by the run id
status prints.`

func stringProp(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func boolProp(description string) map[string]any {
	return map[string]any{"type": "boolean", "description": description}
}

func viewProp() map[string]any {
	return map[string]any{"type": "string", "enum": views, "description": viewIdiom}
}

func objectSchema(properties map[string]any, required ...string) map[string]any {
	schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// decodeArgs parses a tool's arguments strictly, so a misspelt argument is
// reported rather than silently ignored.
func decodeArgs(raw json.RawMessage, into any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return fmt.Errorf("arguments: %w", err)
	}
	return nil
}

// wire renders a proto message the way the service spells it, so a field an
// agent reads here is the field the contract names.
func wire(message proto.Message) (any, error) {
	raw, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(message)
	if err != nil {
		return nil, fmt.Errorf("encode response: %w", err)
	}
	return json.RawMessage(raw), nil
}

// toolset builds the tools over this server's options.
func (s *mcpServer) toolset() []*mcpTool {
	return []*mcpTool{
		{
			name: "status",
			description: `List every live run: the step it is on, how long it has been going, what
it has cost, and the gate a waiting run is blocked on. Start here.
` + viewIdiom,
			schema: objectSchema(map[string]any{
				"project":  stringProp("Only runs on nodes in this dagger project."),
				"statuses": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Only runs in these states: selecting, running, waiting, parked, done, failed, abandoned. Empty means every state that has not ended."},
				"view":     viewProp(),
			}),
			call: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var args struct {
					Project  string   `json:"project"`
					Statuses []string `json:"statuses"`
					View     string   `json:"view"`
				}
				if err := decodeArgs(raw, &args); err != nil {
					return nil, err
				}
				v, err := parseViewName(args.View)
				if err != nil {
					return nil, err
				}
				statuses, err := runStatuses(args.Statuses)
				if err != nil {
					return nil, err
				}
				res, err := storeClient(s.opts.socket).Status(ctx, connect.NewRequest(&millv1.StatusRequest{
					Statuses: statuses, Project: args.Project,
				}))
				if err != nil {
					return nil, err
				}
				if v == viewFull {
					return wire(res.Msg)
				}
				out := make([]map[string]any, 0, len(res.Msg.GetRuns()))
				for _, summary := range res.Msg.GetRuns() {
					out = append(out, summaryFields(summary, v))
				}
				return map[string]any{"runs": out}, nil
			},
		},
		{
			name: "inspect",
			description: `Show one run in full: the workflow version it follows and every attempt at
every step, in order, with the evidence that closed it, how its process
ended, and what it cost. ` + runIdiom + `
` + viewIdiom,
			schema: objectSchema(map[string]any{
				"run":  stringProp("The run, by node reference or run id."),
				"view": viewProp(),
			}, "run"),
			call: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var args struct {
					Run  string `json:"run"`
					View string `json:"view"`
				}
				if err := decodeArgs(raw, &args); err != nil {
					return nil, err
				}
				if args.Run == "" {
					return nil, fmt.Errorf("run is required")
				}
				v, err := parseViewName(args.View)
				if err != nil {
					return nil, err
				}
				res, err := storeClient(s.opts.socket).Inspect(ctx, connect.NewRequest(target(args.Run)))
				if err != nil {
					return nil, err
				}
				if v == viewFull {
					return wire(res.Msg)
				}
				return inspectFields(res.Msg, v), nil
			},
		},
		{
			name: "explain",
			description: `Say what step a run is on, why that step is open, what evidence would close
it, how many attempts it has used against its retries, and what it is
waiting on. A waiting or parked run is blocked on a dagger gate: the gate's
reference and title are named, and only a person can answer it. ` + runIdiom + `

This is the tool to reach for when asked why a run is stuck. It returns no
transcript content; the ledger holds none.`,
			schema: objectSchema(map[string]any{
				"run": stringProp("The run, by node reference or run id."),
			}, "run"),
			call: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var args struct {
					Run string `json:"run"`
				}
				if err := decodeArgs(raw, &args); err != nil {
					return nil, err
				}
				if args.Run == "" {
					return nil, fmt.Errorf("run is required")
				}
				return explain(ctx, storeClient(s.opts.socket), daggerFor(s.opts), args.Run)
			},
		},
		{
			name: "logs",
			description: `Read the log of one attempt at one step: the session's events, one summary
line each, as the worker wrote them through the redactor. Redaction happened
at write time and full tool output was never written. ` + runIdiom,
			schema: objectSchema(map[string]any{
				"run":  stringProp("The run, by node reference or run id."),
				"step": stringProp("The step to read. Defaults to the step the run is on."),
				"tail": map[string]any{"type": "integer", "description": "Only the last N lines. Zero means the whole log."},
			}, "run"),
			call: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var args struct {
					Run  string `json:"run"`
					Step string `json:"step"`
					Tail int    `json:"tail"`
				}
				if err := decodeArgs(raw, &args); err != nil {
					return nil, err
				}
				if args.Run == "" {
					return nil, fmt.Errorf("run is required")
				}
				return s.logs(ctx, args.Run, args.Step, args.Tail)
			},
		},
		{
			name: "history",
			description: `Show every event recorded against a run, oldest first: what changed, when,
and under which worker. The audit trail, and the record of a run that has
already ended. ` + runIdiom,
			schema: objectSchema(map[string]any{
				"run": stringProp("The run, by node reference or run id."),
			}, "run"),
			call: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var args struct {
					Run string `json:"run"`
				}
				if err := decodeArgs(raw, &args); err != nil {
					return nil, err
				}
				if args.Run == "" {
					return nil, fmt.Errorf("run is required")
				}
				asked := target(args.Run)
				res, err := storeClient(s.opts.socket).History(ctx, connect.NewRequest(&millv1.HistoryRequest{
					Id: asked.GetId(), NodeRef: asked.GetNodeRef(),
				}))
				if err != nil {
					return nil, err
				}
				return wire(res.Msg)
			},
		},
		{
			name: "cost",
			description: `Roll a window of usage up four ways: by run, by workflow version, by step
name, and by model. Counts, never currency: the ledger records tokens and a
rate table is applied by whoever wants money.`,
			schema: objectSchema(map[string]any{
				"since":   stringProp("Only usage recorded within this window, such as 24h or 7d. Empty means everything."),
				"project": stringProp("Only runs on nodes in this dagger project."),
			}),
			call: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var args struct {
					Since   string `json:"since"`
					Project string `json:"project"`
				}
				if err := decodeArgs(raw, &args); err != nil {
					return nil, err
				}
				request := &millv1.CostRequest{Project: args.Project}
				if args.Since != "" {
					window, err := parseWindow(args.Since)
					if err != nil {
						return nil, err
					}
					request.Since = timestamp(time.Now().Add(-window))
				}
				res, err := storeClient(s.opts.socket).Cost(ctx, connect.NewRequest(request))
				if err != nil {
					return nil, err
				}
				return wire(res.Msg)
			},
		},
		{
			name: "workflow_list",
			description: `List the workflow versions the store holds: what a run on a project would
be able to follow.`,
			schema: objectSchema(map[string]any{
				"name":         stringProp("Only this workflow."),
				"project":      stringProp("Only this project's scope."),
				"all_versions": boolProp("Every version, not just the newest of each name."),
			}),
			call: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var args struct {
					Name        string `json:"name"`
					Project     string `json:"project"`
					AllVersions bool   `json:"all_versions"`
				}
				if err := decodeArgs(raw, &args); err != nil {
					return nil, err
				}
				res, err := storeClient(s.opts.socket).ListWorkflows(ctx, connect.NewRequest(&millv1.ListWorkflowsRequest{
					Name: args.Name, Project: args.Project, AllVersions: args.AllVersions,
				}))
				if err != nil {
					return nil, err
				}
				return wire(res.Msg)
			},
		},
		{
			name: "workflow_show",
			description: `Print one workflow version as a run would walk it: every step in order, its
kind, what evidence closes it, the capabilities its workspace handle grants,
and what a failure does. This is the definition; inspect shows what a run
made of it.
` + viewIdiom,
			schema: objectSchema(map[string]any{
				"name":    stringProp("The workflow's name, such as quick-fix."),
				"project": stringProp("Resolve in this project's scope."),
				"version": stringProp("A specific version, rather than the newest."),
				"view":    viewProp(),
			}, "name"),
			call: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var args struct {
					Name    string `json:"name"`
					Project string `json:"project"`
					Version string `json:"version"`
					View    string `json:"view"`
				}
				if err := decodeArgs(raw, &args); err != nil {
					return nil, err
				}
				if args.Name == "" {
					return nil, fmt.Errorf("name is required")
				}
				v, err := parseViewName(args.View)
				if err != nil {
					return nil, err
				}
				res, err := storeClient(s.opts.socket).GetWorkflow(ctx, connect.NewRequest(&millv1.GetWorkflowRequest{
					Name: args.Name, Project: args.Project, Version: args.Version,
				}))
				if err != nil {
					return nil, err
				}
				if res.Msg.GetWorkflow() == nil {
					return nil, fmt.Errorf("no workflow named %q is stored", args.Name)
				}
				if v == viewFull {
					return wire(res.Msg.GetWorkflow())
				}
				return workflowFields(res.Msg.GetWorkflow(), v), nil
			},
		},
		{
			name: "doctor",
			description: `Probe everything a run depends on and report each as pass or fail with one
line of reason: the service's socket, the database schema, dagger, the
harness sidecar, the commit signing key, and the forge credential.`,
			schema: objectSchema(map[string]any{}),
			call: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var args struct{}
				if err := decodeArgs(raw, &args); err != nil {
					return nil, err
				}
				return map[string]any{"probes": doctor(ctx, s.doctorOptions())}, nil
			},
		},
		{
			name: "cancel",
			description: `Ask the worker holding a run to stop the step it is on. This sets a flag;
the worker acts on it at its next heartbeat, records the attempt aborted
with reason "cancelled", and releases its lease as preempted.

The run is not failed and not abandoned: it keeps its workflow and its
branch, and the next claim resumes at that step. This is the only tool here
that changes anything, and whether it is allowed is decided by the unix
account this server runs as. ` + runIdiom,
			schema: objectSchema(map[string]any{
				"run":    stringProp("The run, by node reference or run id."),
				"reason": stringProp("Why, recorded on the run's audit trail."),
			}, "run"),
			call: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var args struct {
					Run    string `json:"run"`
					Reason string `json:"reason"`
				}
				if err := decodeArgs(raw, &args); err != nil {
					return nil, err
				}
				if args.Run == "" {
					return nil, fmt.Errorf("run is required")
				}
				store := storeClient(s.opts.socket)
				detail, err := store.Inspect(ctx, connect.NewRequest(target(args.Run)))
				if err != nil {
					return nil, err
				}
				res, err := store.Cancel(ctx, connect.NewRequest(&millv1.CancelRequest{
					RunId: detail.Msg.GetSummary().GetRun().GetId(), Reason: args.Reason,
				}))
				if err != nil {
					return nil, err
				}
				return wire(res.Msg)
			},
		},
	}
}

// logs reads a step run's log for a tool call.
func (s *mcpServer) logs(ctx context.Context, reference, step string, tail int) (any, error) {
	detail, err := storeClient(s.opts.socket).Inspect(ctx, connect.NewRequest(target(reference)))
	if err != nil {
		return nil, err
	}
	if step == "" {
		step = detail.Msg.GetSummary().GetCurrentStep()
	}
	if step == "" {
		return nil, fmt.Errorf("the run is on no step; name one")
	}
	attempt := latestAttempt(detail.Msg.GetSteps(), step)
	if attempt == nil {
		return nil, fmt.Errorf("no attempt at step %q has been made", step)
	}
	path := run.StepLogPath(s.opts.logDir, detail.Msg.GetSummary().GetRun().GetId(), step, attempt.GetStepRun().GetAttempt())
	raw, err := os.ReadFile(path) //nolint:gosec // the path is built from the ledger's own ids
	if err != nil {
		return nil, fmt.Errorf("no log for %s attempt %d: %w", step, attempt.GetStepRun().GetAttempt(), err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if tail > 0 && len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}
	return map[string]any{
		"run":     detail.Msg.GetSummary().GetRun().GetId(),
		"step":    step,
		"attempt": attempt.GetStepRun().GetAttempt(),
		"status":  stepRunStatusName(attempt.GetStepRun().GetStatus()),
		"lines":   lines,
	}, nil
}

// doctorOptions is what the server's flags mean to the doctor tool.
func (s *mcpServer) doctorOptions() doctorOptions {
	return doctorOptions{
		socket:       s.opts.socket,
		daggerSocket: s.opts.daggerSocket,
		project:      s.opts.project,
		sidecar:      envOr(sidecarEnv, "sidecar/dist/main.js"),
		workspaceDir: defaultWorkspaceDir(),
		vcsURL:       os.Getenv(vcsURLEnv),
		identity:     defaultIdentity(),
		name:         defaultForgeUser,
	}
}

// summaryFields is one run as status renders it, sized by view.
func summaryFields(summary *millv1.RunSummary, v view) map[string]any {
	runRow := summary.GetRun()
	if v == viewRefs {
		return map[string]any{"node": runRow.GetNodeRef(), "run": runRow.GetId()}
	}
	in, out, _, _ := sum(summary.GetTotals())
	fields := map[string]any{
		"node":          runRow.GetNodeRef(),
		"run":           runRow.GetId(),
		"status":        runStatusName(runRow.GetStatus()),
		"step":          summary.GetCurrentStep(),
		"attempt":       summary.GetCurrentAttempt(),
		"elapsed":       elapsed(runRow),
		"input_tokens":  in,
		"output_tokens": out,
	}
	if runRow.GetGateNodeId() != "" {
		fields["gate"] = runRow.GetGateNodeId()
	}
	return fields
}

// inspectFields is a run as inspect renders it, sized by view.
func inspectFields(detail *millv1.InspectResponse, v view) map[string]any {
	steps := make([]map[string]any, 0, len(detail.GetSteps()))
	for _, step := range detail.GetSteps() {
		sr := step.GetStepRun()
		row := map[string]any{
			"step":    sr.GetStepName(),
			"attempt": sr.GetAttempt(),
			"status":  stepRunStatusName(sr.GetStatus()),
		}
		if v != viewRefs {
			row["kind"] = stepKindName(sr.GetKind())
			row["exit"] = exitText(sr.GetExit())
			row["evidence_kind"] = evidenceKindName(step.GetEvidence().GetKind())
			row["evidence"] = evidenceValue(step.GetEvidence())
		}
		steps = append(steps, row)
	}
	fields := summaryFields(detail.GetSummary(), v)
	if workflow := detail.GetWorkflow(); workflow != nil {
		fields["workflow"] = workflow.GetName() + "@" + short(workflow.GetVersion())
	}
	fields["steps"] = steps
	return fields
}

// workflowFields is a definition as workflow_show renders it, sized by view.
func workflowFields(definition *millv1.Workflow, v view) map[string]any {
	steps := make([]map[string]any, 0, len(definition.GetSteps()))
	for _, step := range definition.GetSteps() {
		row := map[string]any{"step": step.GetName(), "kind": stepKindName(step.GetKind())}
		if v != viewRefs {
			row["evidence"] = evidenceKindName(step.GetEvidence())
			row["capabilities"] = capabilityNames(step.GetCapabilities())
			row["retries"] = step.GetRetries()
			row["on_failure"] = onFailureText(step.GetOnFailure())
			row["session"] = step.GetSession()
		}
		steps = append(steps, row)
	}
	return map[string]any{
		"name":    definition.GetName(),
		"scope":   scopeText(definition),
		"version": definition.GetVersion(),
		"steps":   steps,
	}
}
