package cli

import (
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	harnessv1 "github.com/ivy/mill/gen/harness/v1"
	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/gen/mill/v1/millv1connect"
	"github.com/ivy/mill/internal/dagger"
)

// The read tools. Every one is a thin call on the Connect client and a
// renderer over its answer: docs/prd.md R-52, and docs/ledger.md §"Queries
// the CLI needs" for what each is meant to answer.
//
// Two rules hold across all of them. They return ledger data and never
// transcript content — a session's words are not the ledger's business, and
// the one place they surface is `mill logs`, which reads a file the redactor
// already wrote. And each is one client with two framings: the functions
// below build a value, the CLI renders it as text and `mill mcp` renders the
// same value as JSON, so an agent and an operator are looking at the same
// fields.

// view is how much of an answer a caller wants, spelled as dagger's views
// are so an agent that knows one knows the other.
type view string

const (
	// viewRefs is identifiers alone: what to ask about next.
	viewRefs view = "refs"
	// viewSummary is the fields an operator reads at a glance. The default.
	viewSummary view = "summary"
	// viewFull is everything the ledger holds about the thing.
	viewFull view = "full"
)

// views is the vocabulary, for a flag's help and a tool's schema.
var views = []string{string(viewRefs), string(viewSummary), string(viewFull)}

// parseViewName reads a view name, defaulting an empty one to summary.
func parseViewName(text string) (view, error) {
	switch v := view(text); v {
	case "":
		return viewSummary, nil
	case viewRefs, viewSummary, viewFull:
		return v, nil
	default:
		return "", fmt.Errorf("view: %q is not one of %s", text, strings.Join(views, ", "))
	}
}

// readOptions are what every read tool needs: where the service is, and how
// much of the answer to render.
type readOptions struct {
	socket       string
	daggerSocket string
	project      string
	view         string
	statuses     []string
	since        string
	ended        bool
	cursor       string
	limit        int32
	until        string
	bucket       string
	step         string
	follow       bool
	workspaceDir string
	reason       string
	// config is the configuration file `status` reads the worker's claim
	// filter from, for its header line. No other read tool uses it.
	config string
}

// target names the run a tool was asked about. A reference carrying a `#` is
// a dagger node's display key; anything else is the run's own id, which is
// what `mill status` prints.
//
// A node reference resolves to that node's most recent run in the ledger —
// the highest generation, which is the run a restart left in place — so
// `mill logs mill#18` answers about the node an operator has in front of
// them without their having to look a run id up first.
func target(reference string) *millv1.InspectRequest {
	if strings.Contains(reference, "#") {
		return &millv1.InspectRequest{NodeRef: reference}
	}
	return &millv1.InspectRequest{Id: reference}
}

// isNodeRef reports whether a reference is a dagger node's rather than a
// run's.
func isNodeRef(reference string) bool { return strings.Contains(reference, "#") }

// inspected is one run as the read tools ask for it, with the ledger's
// not-found turned into the sentence the operator needs.
//
// The store answers a reference it holds no run for with a not-found, whose
// wire message is about a row. What an operator asked was about a node, and
// "the node has no run" is a different fact from "there is no such node":
// the mill's ledger only ever knows the first, so that is what it says.
func inspected(ctx context.Context, store millv1connect.StoreServiceClient, tool, reference string) (*millv1.InspectResponse, error) {
	res, err := store.Inspect(ctx, connect.NewRequest(target(reference)))
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", tool, reference, noRun(reference, err))
	}
	return res.Msg, nil
}

// noRun replaces a not-found with what it means for the reference asked
// about, and leaves every other failure alone.
func noRun(reference string, err error) error {
	if connect.CodeOf(err) != connect.CodeNotFound {
		return err
	}
	if isNodeRef(reference) {
		return fmt.Errorf("the ledger holds no run for node %s: the mill has never worked it", reference)
	}
	return fmt.Errorf("the ledger holds no run with id %s", reference)
}

// Status.

// runStatuses parses the --status filter.
func runStatuses(names []string) ([]millv1.RunStatus, error) {
	out := make([]millv1.RunStatus, 0, len(names))
	for _, name := range names {
		status := millv1.RunStatus(millv1.RunStatus_value["RUN_STATUS_"+strings.ToUpper(strings.ReplaceAll(name, "-", "_"))])
		if status == millv1.RunStatus_RUN_STATUS_UNSPECIFIED {
			return nil, fmt.Errorf("--status: %q is not a run status", name)
		}
		out = append(out, status)
	}
	return out, nil
}

func newStatusCommand() *cobra.Command {
	opts := readOptions{}
	cmd := &cobra.Command{
		Use:   "status",
		Short: "List runs by state with the step each is on",
		Long: `List every live run: the step it is on, how long it has been going, what it
has cost, and the gate a waiting run is blocked on.

With no --status filter this is the runs that have not ended: selecting,
running, waiting and parked.

--ended lists runs that have ended instead, newest-ended first, one page at a
time; the printed cursor pages to the next.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStatus(cmd, opts)
		},
	}
	addReadFlags(cmd, &opts)
	cmd.Flags().StringSliceVar(&opts.statuses, "status", nil, "only runs in these states (default: every state that has not ended)")
	cmd.Flags().StringVar(&opts.project, "project", "", "only runs on nodes in this dagger project")
	cmd.Flags().StringVar(&opts.config, "config", defaultConfigPath(), "path of the configuration file the worker's claim filter is read from for the header line (default: $"+configEnv+", else $XDG_CONFIG_HOME/mill/config.yaml, else /etc/mill/config.yaml)")
	cmd.Flags().BoolVar(&opts.ended, "ended", false, "list runs that have ended instead of ones still live")
	cmd.Flags().StringVar(&opts.since, "since", "", "with --ended, only runs that ended within this window, such as 24h or 7d")
	cmd.Flags().StringVar(&opts.cursor, "cursor", "", "with --ended, resume the page named by the previous call's printed cursor")
	cmd.Flags().Int32Var(&opts.limit, "limit", 0, "with --ended, runs per page, up to 200 (default: 50)")
	return cmd
}

func runStatus(cmd *cobra.Command, opts readOptions) error {
	v, err := parseViewName(opts.view)
	if err != nil {
		return err
	}
	statuses, err := runStatuses(opts.statuses)
	if err != nil {
		return err
	}
	config, err := LoadConfig(opts.config)
	if err != nil {
		return err
	}
	projects, labels := resolvedClaims(cmd.Context(), opts.socket, config)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "worker claims from: %s\n", claimsFrom(projects, labels))

	if opts.ended {
		return runStatusEnded(cmd, opts, statuses, v)
	}

	res, err := storeClient(opts.socket).Status(cmd.Context(), connect.NewRequest(&millv1.StatusRequest{
		Statuses: statuses, Project: opts.project,
	}))
	if err != nil {
		return fmt.Errorf("status: %w", err)
	}
	if err := renderStatus(cmd, res.Msg.GetRuns(), v); err != nil {
		return err
	}
	reportUndecodable(cmd, res.Msg.GetFindings())
	return nil
}

// runStatusEnded is `mill status --ended`: one page of ended runs, windowed
// by --since, in the same view as the live listing.
func runStatusEnded(cmd *cobra.Command, opts readOptions, statuses []millv1.RunStatus, v view) error {
	request := &millv1.ListRunsRequest{
		Statuses: statuses, Project: opts.project, View: string(v), Cursor: opts.cursor, Limit: opts.limit,
	}
	if opts.since != "" {
		window, err := parseWindow(opts.since)
		if err != nil {
			return err
		}
		request.EndedAfter = timestamp(time.Now().Add(-window))
	}
	res, err := storeClient(opts.socket).ListRuns(cmd.Context(), connect.NewRequest(request))
	if err != nil {
		return fmt.Errorf("status: %w", err)
	}
	if err := renderStatus(cmd, res.Msg.GetRuns(), v); err != nil {
		return err
	}
	if cursor := res.Msg.GetNextCursor(); cursor != "" {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "next: --cursor %s\n", cursor)
	}
	reportUndecodable(cmd, res.Msg.GetFindings())
	return nil
}

// reportUndecodable prints the workflow rows a listing could not decode,
// one line each on standard error, after the answer: the answer is still
// the answer, and a row that will not read is a warning about the ledger,
// not a column of it. The remedy is the operator's (docs/runbook.md); the
// tool neither deletes nor repairs the row.
func reportUndecodable(cmd *cobra.Command, findings []*millv1.UndecodableWorkflow) {
	for _, finding := range findings {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", undecodableText(finding))
	}
}

// undecodableText is one finding on one line: the row, the field when the
// decoder could name one, and the decoder's own words.
func undecodableText(finding *millv1.UndecodableWorkflow) string {
	if finding.GetField() == "" {
		return fmt.Sprintf("workflow %s does not decode and was left out: %s", finding.GetWorkflowId(), finding.GetDetail())
	}
	return fmt.Sprintf("workflow %s does not decode and was left out: field %q: %s",
		finding.GetWorkflowId(), finding.GetField(), finding.GetDetail())
}

// renderStatus prints the ledger's status query as a table.
func renderStatus(cmd *cobra.Command, runs []*millv1.RunSummary, v view) error {
	w := table(cmd)
	switch v {
	case viewRefs:
		_, _ = fmt.Fprintln(w, "NODE\tRUN")
		for _, summary := range runs {
			_, _ = fmt.Fprintf(w, "%s\t%s\n", summary.GetRun().GetNodeRef(), summary.GetRun().GetId())
		}
	case viewFull:
		_, _ = fmt.Fprintln(w, "NODE\tRUN\tSTATUS\tSTEP\tATTEMPT\tELAPSED\tLEASES\tGATE\tWORKER\tTOKENS\tMACHINE\tIMAGE\tVETH\tCACHES\tLAST STEP\tROUNDS")
		for _, summary := range runs {
			run := summary.GetRun()
			sandbox := summary.GetWorkspace().GetSandbox()
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\n",
				run.GetNodeRef(), run.GetId(), runStatusName(run.GetStatus()),
				summary.GetCurrentStep(), summary.GetCurrentAttempt(), elapsed(run),
				summary.GetLeases(), dash(run.GetGateNodeId()), dash(run.GetWorkerRef()),
				tokenText(summary.GetTotals()),
				dash(sandbox.GetMachine()), dash(sandbox.GetImageDigest()), dash(sandbox.GetVeth()), dash(lineagesText(sandbox)),
				lastStepText(summary), summary.GetRounds())
		}
	default:
		_, _ = fmt.Fprintln(w, "NODE\tSTATUS\tSTEP\tATTEMPT\tELAPSED\tTOKENS\tGATE\tMACHINE\tLAST STEP\tROUNDS")
		for _, summary := range runs {
			run := summary.GetRun()
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\t%d\n",
				run.GetNodeRef(), runStatusName(run.GetStatus()), summary.GetCurrentStep(),
				summary.GetCurrentAttempt(), elapsed(run), tokenText(summary.GetTotals()),
				dash(run.GetGateNodeId()), dash(summary.GetWorkspace().GetSandbox().GetMachine()),
				lastStepText(summary), summary.GetRounds())
		}
	}
	return w.Flush()
}

// lastStepText is the step a run most recently attempted, with the status
// that attempt ended in: dash for a run that has attempted nothing.
func lastStepText(summary *millv1.RunSummary) string {
	if summary.GetLastStep() == "" {
		return "-"
	}
	return fmt.Sprintf("%s (%s)", summary.GetLastStep(), stepRunStatusName(summary.GetLastStepStatus()))
}

// lineagesText is a sandbox's cache lineages on one line: each as
// `<scope>/<name>@<restored>`, with `->` and the promoted generation once
// the run has finished and promoted it. Empty for no lineages.
func lineagesText(sandbox *millv1.Sandbox) string {
	parts := make([]string, 0, len(sandbox.GetLineages()))
	for _, lineage := range sandbox.GetLineages() {
		parts = append(parts, lineageText(lineage))
	}
	return strings.Join(parts, ", ")
}

// lineageText is one lineage as lineagesText spells it.
func lineageText(lineage *millv1.SandboxLineage) string {
	text := fmt.Sprintf("%s/%s@%d", lineage.GetScope(), lineage.GetName(), lineage.GetRestored())
	if lineage.Promoted != nil {
		text += fmt.Sprintf("->%d", lineage.GetPromoted())
	}
	return text
}

// renderSandbox prints the machine behind a run's workspace under the
// inspect header: the name and unit, the digest it booted, the veth, and
// each cache lineage with the generation it restored and the one it
// promoted (docs/ledger.md §workspaces). Nothing for a run with no machine.
func renderSandbox(out io.Writer, sandbox *millv1.Sandbox) {
	if sandbox == nil {
		return
	}
	_, _ = fmt.Fprintf(out, "  machine   %s (%s)\n", sandbox.GetMachine(), sandbox.GetUnit())
	_, _ = fmt.Fprintf(out, "  image     %s\n", sandbox.GetImageDigest())
	_, _ = fmt.Fprintf(out, "  veth      %s\n", dash(sandbox.GetVeth()))
	for _, lineage := range sandbox.GetLineages() {
		promoted := "-"
		if lineage.Promoted != nil {
			promoted = fmt.Sprint(lineage.GetPromoted())
		}
		_, _ = fmt.Fprintf(out, "  cache     %s/%s restored %d promoted %s\n",
			lineage.GetScope(), lineage.GetName(), lineage.GetRestored(), promoted)
	}
}

// Inspect.

func newInspectCommand() *cobra.Command {
	opts := readOptions{}
	cmd := &cobra.Command{
		Use:   "inspect <run>",
		Short: "Show one run's every attempt with its evidence",
		Long: `Show one run in full: the workflow version it follows and every attempt at
every step, in order, with the evidence that closed it, how its process
ended, the session it ran in, and what it cost.

The run is named by its id or by its node's reference, such as mill#18.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInspect(cmd, opts, args[0])
		},
	}
	addReadFlags(cmd, &opts)
	return cmd
}

func runInspect(cmd *cobra.Command, opts readOptions, reference string) error {
	v, err := parseViewName(opts.view)
	if err != nil {
		return err
	}
	detail, err := inspected(cmd.Context(), storeClient(opts.socket), "inspect", reference)
	if err != nil {
		return err
	}
	return renderInspect(cmd, detail, v)
}

// renderInspect prints a run's header and one row per attempt. Every closed
// attempt shows its evidence value, because "what closed this step" is the
// question inspect exists to answer.
func renderInspect(cmd *cobra.Command, detail *millv1.InspectResponse, v view) error {
	out := cmd.OutOrStdout()
	run := detail.GetSummary().GetRun()
	if v == viewRefs {
		w := table(cmd)
		_, _ = fmt.Fprintln(w, "STEP\tATTEMPT\tSTATUS")
		for _, step := range detail.GetSteps() {
			_, _ = fmt.Fprintf(w, "%s\t%d\t%s\n", step.GetStepRun().GetStepName(),
				step.GetStepRun().GetAttempt(), stepRunStatusName(step.GetStepRun().GetStatus()))
		}
		return w.Flush()
	}

	_, _ = fmt.Fprintf(out, "%s run %s generation %d\n", run.GetNodeRef(), run.GetId(), run.GetGeneration())
	_, _ = fmt.Fprintf(out, "  status    %s\n", runStatusName(run.GetStatus()))
	if workflow := detail.GetWorkflow(); workflow != nil {
		_, _ = fmt.Fprintf(out, "  workflow  %s@%s\n", workflow.GetName(), short(workflow.GetVersion()))
	}
	_, _ = fmt.Fprintf(out, "  step      %s attempt %d\n", dash(detail.GetSummary().GetCurrentStep()), detail.GetSummary().GetCurrentAttempt())
	if run.GetGateNodeId() != "" {
		_, _ = fmt.Fprintf(out, "  gate      %s\n", run.GetGateNodeId())
	}
	if run.GetCancelRequested() {
		_, _ = fmt.Fprintf(out, "  cancel    requested\n")
	}
	_, _ = fmt.Fprintf(out, "  branch    %s\n", run.GetBranch())
	renderSandbox(out, detail.GetSummary().GetWorkspace().GetSandbox())
	_, _ = fmt.Fprintf(out, "  tokens    %s\n\n", tokenText(detail.GetSummary().GetTotals()))

	w := table(cmd)
	if v == viewFull {
		_, _ = fmt.Fprintln(w, "STEP\tATTEMPT\tKIND\tSTATUS\tEXIT\tEVIDENCE\tVALUE\tSESSION\tTOKENS")
	} else {
		_, _ = fmt.Fprintln(w, "STEP\tATTEMPT\tSTATUS\tEXIT\tEVIDENCE\tVALUE")
	}
	for _, step := range detail.GetSteps() {
		sr := step.GetStepRun()
		row := []string{
			sr.GetStepName(),
			fmt.Sprint(sr.GetAttempt()),
			stepRunStatusName(sr.GetStatus()),
			exitText(sr.GetExit()),
			evidenceKindName(step.GetEvidence().GetKind()),
			stepValue(sr, step.GetEvidence()),
		}
		if v == viewFull {
			row = []string{
				sr.GetStepName(), fmt.Sprint(sr.GetAttempt()), stepKindName(sr.GetKind()),
				stepRunStatusName(sr.GetStatus()), exitText(sr.GetExit()),
				evidenceKindName(step.GetEvidence().GetKind()), stepValue(sr, step.GetEvidence()),
				dash(sr.GetSessionId()), tokensCounted(step),
			}
		}
		_, _ = fmt.Fprintln(w, strings.Join(row, "\t"))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if err := renderHeartbeats(cmd, detail); err != nil {
		return err
	}
	return renderEnvironments(cmd, detail)
}

// renderHeartbeats prints what an attempt's wait cost: how many times the
// harness asked a session that had ended its turn on a task that then went
// quiet whether the task was still needed (docs/domain-model.md §Session).
// It is a table of its own, under the steps, and only for the attempts that
// were asked at all — an attempt whose exit reads `stalled` is one whose
// heartbeats ran out, and this is where the count that led there is.
func renderHeartbeats(cmd *cobra.Command, detail *millv1.InspectResponse) error {
	rows := [][]string{}
	for _, step := range detail.GetSteps() {
		if step.GetHeartbeats() == 0 {
			continue
		}
		rows = append(rows, []string{
			step.GetStepRun().GetStepName(),
			fmt.Sprint(step.GetStepRun().GetAttempt()),
			fmt.Sprint(step.GetHeartbeats()),
		})
	}
	if len(rows) == 0 {
		return nil
	}
	_, _ = fmt.Fprintln(cmd.OutOrStdout())
	w := table(cmd)
	_, _ = fmt.Fprintln(w, "STEP\tATTEMPT\tHEARTBEATS")
	for _, row := range rows {
		_, _ = fmt.Fprintln(w, strings.Join(row, "\t"))
	}
	return w.Flush()
}

// renderEnvironments prints what the environment mode gave each session,
// under the steps table and only for the attempts that have something to
// say. It is a table of its own because the lists are long and per-attempt:
// folded into the steps table they would widen every row for the one step
// that carved something out.
func renderEnvironments(cmd *cobra.Command, detail *millv1.InspectResponse) error {
	rows := [][]string{}
	for _, step := range detail.GetSteps() {
		sr := step.GetStepRun()
		if len(sr.GetEnvPassthrough()) == 0 && len(sr.GetEnvStripped()) == 0 {
			continue
		}
		rows = append(rows, []string{
			sr.GetStepName(), fmt.Sprint(sr.GetAttempt()),
			dash(strings.Join(sr.GetEnvPassthrough(), " ")),
			dash(strings.Join(sr.GetEnvStripped(), " ")),
		})
	}
	if len(rows) == 0 {
		return nil
	}
	_, _ = fmt.Fprintln(cmd.OutOrStdout())
	w := table(cmd)
	_, _ = fmt.Fprintln(w, "STEP\tATTEMPT\tENV PASSTHROUGH\tENV STRIPPED")
	for _, row := range rows {
		_, _ = fmt.Fprintln(w, strings.Join(row, "\t"))
	}
	return w.Flush()
}

// Explain.

// Explanation is why a run is where it is: the one answer both the CLI and
// `mill mcp` render, so an agent asking over MCP and an operator asking in a
// shell are told the same things. It carries no transcript content — the
// ledger holds none, and this is the tool a thicket agent reaches for.
type Explanation struct {
	// Node is the dagger node, Run the ledger's id for its run.
	Node string `json:"node"`
	Run  string `json:"run"`
	// Status is where the run is: running, waiting, parked.
	Status string `json:"status"`
	// Step is the step the run is on, and Kind how it executes.
	Step string `json:"step,omitempty"`
	Kind string `json:"step_kind,omitempty"`
	// StepStatus is the latest attempt's status, and Exit how its process
	// ended.
	StepStatus string `json:"step_run_status,omitempty"`
	Exit       string `json:"exit,omitempty"`
	// Evidence is the kind that would close the step.
	Evidence string `json:"evidence,omitempty"`
	// Attempts is how many attempts this step has used, of Retries.
	Attempts int64 `json:"attempts_used"`
	Retries  int64 `json:"retries"`
	// Gate is the dagger node a waiting or parked run is blocked on, and
	// GateTitle its ask.
	Gate      string `json:"gate,omitempty"`
	GateTitle string `json:"gate_title,omitempty"`
	// ParkedReason says what tripped the circuit breaker, for a parked run.
	ParkedReason string `json:"parked_reason,omitempty"`
	// Waiting is the sentence an operator reads first.
	Waiting string `json:"waiting_on"`
}

func newExplainCommand() *cobra.Command {
	opts := readOptions{}
	cmd := &cobra.Command{
		Use:   "explain <run>",
		Short: "Say where a run is and what it is waiting on",
		Long: `Say what step a run is on, why that step is open, what evidence would close
it, how many attempts it has used, and what it is waiting on.

A waiting or parked run is blocked on a dagger gate: its reference and its
title are printed, and answering it in dagger is what lets the run go on.
Nothing a session said appears here; the ledger holds none of it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runExplain(cmd, opts, args[0])
		},
	}
	addReadFlags(cmd, &opts)
	addDaggerFlags(cmd, &opts)
	return cmd
}

func runExplain(cmd *cobra.Command, opts readOptions, reference string) error {
	explanation, err := explain(cmd.Context(), storeClient(opts.socket), daggerFor(opts), reference)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "%s run %s is %s\n", explanation.Node, explanation.Run, explanation.Status)
	if explanation.Step != "" {
		_, _ = fmt.Fprintf(out, "  step      %s (%s) %s\n", explanation.Step, explanation.Kind, explanation.StepStatus)
		if explanation.Exit != "" {
			_, _ = fmt.Fprintf(out, "  exit      %s\n", explanation.Exit)
		}
		_, _ = fmt.Fprintf(out, "  evidence  %s would close it\n", explanation.Evidence)
		_, _ = fmt.Fprintf(out, "  attempts  %d of %d retries\n", explanation.Attempts, explanation.Retries)
	}
	if explanation.Gate != "" {
		_, _ = fmt.Fprintf(out, "  gate      %s\n", explanation.Gate)
		if explanation.GateTitle != "" {
			_, _ = fmt.Fprintf(out, "  ask       %s\n", explanation.GateTitle)
		}
	}
	if explanation.ParkedReason != "" {
		_, _ = fmt.Fprintf(out, "  parked    %s\n", explanation.ParkedReason)
	}
	_, _ = fmt.Fprintf(out, "  waiting   %s\n", explanation.Waiting)
	return nil
}

// gates resolves the gate a run is blocked on to what a human would call it.
// It is a function so `mill explain` works with no dagger at all: an
// unreachable service costs the gate's reference and title and nothing else.
type gates func(ctx context.Context, nodeRef, runID, gateNodeID string) (reference, title string)

// explain builds the explanation both faces render.
func explain(ctx context.Context, store millv1connect.StoreServiceClient, gate gates, reference string) (*Explanation, error) {
	detail, err := inspected(ctx, store, "explain", reference)
	if err != nil {
		return nil, err
	}
	run := detail.GetSummary().GetRun()
	explanation := &Explanation{
		Node:         run.GetNodeRef(),
		Run:          run.GetId(),
		Status:       runStatusName(run.GetStatus()),
		Step:         detail.GetSummary().GetCurrentStep(),
		Attempts:     detail.GetSummary().GetCurrentAttempt(),
		ParkedReason: parkedReasonName(run.GetParkedReason()),
	}

	// The step the run is on, as the workflow defines it: what closes it and
	// how many attempts it is allowed before the breaker trips.
	if step := stepNamed(detail.GetWorkflow(), explanation.Step); step != nil {
		explanation.Kind = stepKindName(step.GetKind())
		explanation.Evidence = evidenceKindName(step.GetEvidence())
		explanation.Retries = int64(step.GetRetries())
	}
	// The latest attempt at it, as the ledger recorded it.
	if latest := latestAttempt(detail.GetSteps(), explanation.Step); latest != nil {
		explanation.StepStatus = stepRunStatusName(latest.GetStepRun().GetStatus())
		explanation.Exit = exitText(latest.GetStepRun().GetExit())
	}
	if run.GetGateNodeId() != "" {
		explanation.Gate = run.GetGateNodeId()
		if gate != nil {
			explanation.Gate, explanation.GateTitle = gate(ctx, run.GetNodeRef(), run.GetId(), run.GetGateNodeId())
		}
	}
	explanation.Waiting = waitingOn(explanation, run)
	return explanation, nil
}

// waitingOn is the one sentence that says why nothing is moving.
func waitingOn(e *Explanation, run *millv1.Run) string {
	switch run.GetStatus() {
	case millv1.RunStatus_RUN_STATUS_WAITING:
		return "a human to answer gate " + e.Gate
	case millv1.RunStatus_RUN_STATUS_PARKED:
		return "a human to answer park gate " + e.Gate
	case millv1.RunStatus_RUN_STATUS_SELECTING:
		return "a workflow to be selected"
	case millv1.RunStatus_RUN_STATUS_RUNNING:
		if e.StepStatus == "running" {
			return fmt.Sprintf("step %s to produce %s evidence", e.Step, e.Evidence)
		}
		return "a worker to claim the node and resume at step " + e.Step
	default:
		return "nothing: the run has ended"
	}
}

// latestAttempt is the newest attempt at a step. Attempts arrive in order,
// so the last one named is the current one.
func latestAttempt(steps []*millv1.StepRunDetail, name string) *millv1.StepRunDetail {
	var latest *millv1.StepRunDetail
	for _, step := range steps {
		if step.GetStepRun().GetStepName() == name {
			latest = step
		}
	}
	return latest
}

// History.

func newHistoryCommand() *cobra.Command {
	opts := readOptions{}
	cmd := &cobra.Command{
		Use:   "history <run>",
		Short: "Show a run's audit trail",
		Long: `Show every event recorded against a run, oldest first: what changed, when,
and under which worker. The events rows of docs/ledger.md, and the record
of a run that has already ended.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHistory(cmd, opts, args[0])
		},
	}
	addReadFlags(cmd, &opts)
	return cmd
}

func runHistory(cmd *cobra.Command, opts readOptions, reference string) error {
	v, err := parseViewName(opts.view)
	if err != nil {
		return err
	}
	asked := target(reference)
	res, err := storeClient(opts.socket).History(cmd.Context(), connect.NewRequest(&millv1.HistoryRequest{
		Id: asked.GetId(), NodeRef: asked.GetNodeRef(),
	}))
	if err != nil {
		return fmt.Errorf("history %s: %w", reference, noRun(reference, err))
	}
	w := table(cmd)
	if v == viewFull {
		_, _ = fmt.Fprintln(w, "WHEN\tOP\tSTEP RUN\tWORKER\tBEFORE\tAFTER")
	} else {
		_, _ = fmt.Fprintln(w, "WHEN\tOP\tAFTER")
	}
	for _, event := range res.Msg.GetEvents() {
		when := event.GetOccurredAt().AsTime().Format(time.RFC3339)
		if v == viewFull {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", when, event.GetOp(),
				dash(event.GetStepRunId()), dash(event.GetWorkerRef()),
				fields(event.GetBefore()), fields(event.GetAfter()))
			continue
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", when, event.GetOp(), fields(event.GetAfter()))
	}
	return w.Flush()
}

// Cost.

func newCostCommand() *cobra.Command {
	opts := readOptions{}
	cmd := &cobra.Command{
		Use:   "cost",
		Short: "Roll usage up by run, workflow, step and model",
		Long: `Roll a window of usage up by run, by workflow version, by step name, and by
model — and, with --bucket day, by UTC calendar day too.

Counts, never currency. The ledger records tokens; a rate table is applied
by whoever wants money. STEP rows also carry the funnel: how many runs
reached the step, passed it, died there, needed more than one attempt, and
how many revise rounds it answered.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCost(cmd, opts)
		},
	}
	addReadFlags(cmd, &opts)
	cmd.Flags().StringVar(&opts.since, "since", "", "only usage recorded within this window, such as 24h or 7d")
	cmd.Flags().StringVar(&opts.until, "until", "", "exclude usage within this window of now, such as 1h")
	cmd.Flags().StringVar(&opts.bucket, "bucket", "", "also group the window over time: day")
	cmd.Flags().StringVar(&opts.project, "project", "", "only runs on nodes in this dagger project")
	return cmd
}

// costSection is one of `mill cost`'s tables: a title, the buckets it
// renders, and whether its rows carry the step funnel's extra columns.
type costSection struct {
	title   string
	buckets []*millv1.CostBucket
	counted bool
}

func runCost(cmd *cobra.Command, opts readOptions) error {
	request := &millv1.CostRequest{Project: opts.project}
	if opts.since != "" {
		window, err := parseWindow(opts.since)
		if err != nil {
			return err
		}
		request.Since = timestamp(time.Now().Add(-window))
	}
	if opts.until != "" {
		window, err := parseWindow(opts.until)
		if err != nil {
			return err
		}
		request.Until = timestamp(time.Now().Add(-window))
	}
	bucket, err := parseBucket(opts.bucket)
	if err != nil {
		return err
	}
	request.Bucket = bucket
	res, err := storeClient(opts.socket).Cost(cmd.Context(), connect.NewRequest(request))
	if err != nil {
		return fmt.Errorf("cost: %w", err)
	}
	out := cmd.OutOrStdout()
	fillByModel(res.Msg)
	sections := []costSection{
		{"RUN", res.Msg.GetByRun(), false},
		{"WORKFLOW", res.Msg.GetByWorkflow(), false},
		{"STEP", res.Msg.GetByStep(), true},
		{"MODEL", res.Msg.GetByModel(), false},
	}
	if bucket == millv1.TimeBucket_TIME_BUCKET_DAY {
		sections = append(sections, costSection{"DAY", res.Msg.GetByDay(), false})
	}
	for i, section := range sections {
		if i > 0 {
			_, _ = fmt.Fprintln(out)
		}
		// One writer per section: a shared one would align unrelated tables
		// to each other's widest key.
		w := table(cmd)
		if section.counted {
			_, _ = fmt.Fprintf(w, "%s\tSOURCE\tINPUT\tOUTPUT\tCACHE READ\tCACHE WRITE\tREACHED\tPASSED\tDIED\tRETRIED\tREVISES\tATTEMPTS\n", section.title)
		} else {
			_, _ = fmt.Fprintf(w, "%s\tSOURCE\tINPUT\tOUTPUT\tCACHE READ\tCACHE WRITE\n", section.title)
		}
		for _, bucket := range section.buckets {
			row := func(source string, in, outTokens, read, write int64) {
				if !section.counted {
					_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t%d\n", bucket.GetKey(), source, in, outTokens, read, write)
					return
				}
				c := bucket.GetCounts()
				_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\n",
					bucket.GetKey(), source, in, outTokens, read, write,
					c.GetReached(), c.GetPassed(), c.GetDied(), c.GetRetried(), c.GetRevises(), c.GetAttempts())
			}
			sources := sourcesOf(bucket.GetTotals())
			if len(sources) == 0 {
				if !section.counted {
					continue
				}
				// A step whose counts come from step runs the usage window
				// never billed anything for — a check, a gate — still gets
				// a row: the funnel is what this section is for.
				row("-", 0, 0, 0, 0)
				continue
			}
			// One row per who counted: the adapter's report and, where a
			// gateway sat in front of the session, its own count beside
			// it (docs/sandbox.md §Credentials). The counts are per bucket,
			// not per source, so they repeat on every row of a bucket that
			// has more than one.
			for _, source := range sources {
				in, outTokens, read, write := sum(bySource(bucket.GetTotals(), source))
				row(sourceName(source), in, outTokens, read, write)
			}
		}
		if err := w.Flush(); err != nil {
			return err
		}
	}
	return nil
}

// parseBucket reads a --bucket flag. Empty means no time grouping.
func parseBucket(text string) (millv1.TimeBucket, error) {
	switch text {
	case "":
		return millv1.TimeBucket_TIME_BUCKET_UNSPECIFIED, nil
	case "day":
		return millv1.TimeBucket_TIME_BUCKET_DAY, nil
	default:
		return millv1.TimeBucket_TIME_BUCKET_UNSPECIFIED, fmt.Errorf("--bucket: %q is not a supported bucket, want \"day\"", text)
	}
}

// sourcesOf is who counted a bucket's rows, the adapter first.
func sourcesOf(totals []*millv1.ModelTotals) []millv1.UsageSource {
	var sources []millv1.UsageSource
	for _, total := range totals {
		if !slices.Contains(sources, total.GetSource()) {
			sources = append(sources, total.GetSource())
		}
	}
	slices.Sort(sources)
	return sources
}

// bySource is the rows one source counted.
func bySource(totals []*millv1.ModelTotals, source millv1.UsageSource) []*millv1.ModelTotals {
	var out []*millv1.ModelTotals
	for _, total := range totals {
		if total.GetSource() == source {
			out = append(out, total)
		}
	}
	return out
}

// sourceName is a source as the table prints it. Unspecified is the
// adapter's: every row written before the gateway existed was its report.
func sourceName(source millv1.UsageSource) string {
	if source == millv1.UsageSource_USAGE_SOURCE_GATEWAY {
		return "gateway"
	}
	return "sdk"
}

// fillByModel derives the MODEL section from the rows the STEP section
// renders, and returns the response for chaining.
//
// The four sections are one pass over the same usage rows, so every model
// that billed anything billed it under some step, and each step's bucket
// already carries its totals per model and per who counted them. Summing
// them here is what makes the two tables agree by construction: a MODEL
// section with headers and no rows under a STEP section with rows is a
// contradiction an operator has to spend a command discovering, and there
// is nothing the by-model rollup knows that the by-step one does not.
func fillByModel(cost *millv1.CostResponse) *millv1.CostResponse {
	if cost == nil {
		return cost
	}
	totals := map[string]map[millv1.UsageSource]*millv1.ModelTotals{}
	for _, bucket := range cost.GetByStep() {
		for _, model := range bucket.GetTotals() {
			counted, seen := totals[model.GetModel()]
			if !seen {
				counted = map[millv1.UsageSource]*millv1.ModelTotals{}
				totals[model.GetModel()] = counted
			}
			running, seen := counted[model.GetSource()]
			if !seen {
				running = &millv1.ModelTotals{Model: model.GetModel(), Source: model.GetSource()}
				counted[model.GetSource()] = running
			}
			running.InputTokens += model.GetInputTokens()
			running.OutputTokens += model.GetOutputTokens()
			running.CacheReadTokens += model.GetCacheReadTokens()
			running.CacheWriteTokens += model.GetCacheWriteTokens()
		}
	}
	cost.ByModel = make([]*millv1.CostBucket, 0, len(totals))
	for _, model := range slices.Sorted(maps.Keys(totals)) {
		bucket := &millv1.CostBucket{Key: model}
		for _, source := range slices.Sorted(maps.Keys(totals[model])) {
			bucket.Totals = append(bucket.Totals, totals[model][source])
		}
		cost.ByModel = append(cost.ByModel, bucket)
	}
	return cost
}

// parseWindow reads a --since window. Go's duration syntax, plus `d` for a
// day, because nobody types 168h.
func parseWindow(text string) (time.Duration, error) {
	if days, found := strings.CutSuffix(text, "d"); found {
		count, err := time.ParseDuration(days + "h")
		if err == nil {
			return count * 24, nil
		}
	}
	window, err := time.ParseDuration(text)
	if err != nil || window <= 0 {
		return 0, fmt.Errorf("--since: %q is not a window such as 24h or 7d", text)
	}
	return window, nil
}

// Shared rendering.

// addReadFlags puts the flags every read tool takes on a command.
func addReadFlags(cmd *cobra.Command, opts *readOptions) {
	cmd.Flags().StringVar(&opts.socket, "socket", defaultSocketPath(), "path of the service's unix socket (default: $"+socketEnv+")")
	cmd.Flags().StringVar(&opts.view, "view", string(viewSummary), "how much to show: "+strings.Join(views, ", "))
}

// addDaggerFlags puts the flags a tool that reads dagger takes.
func addDaggerFlags(cmd *cobra.Command, opts *readOptions) {
	cmd.Flags().StringVar(&opts.daggerSocket, "dagger-socket", envOr(daggerSocketEnv, defaultDaggerSocket), "path of dagger's unix socket (default: $"+daggerSocketEnv+")")
	cmd.Flags().StringVar(&opts.project, "project", envOr(daggerProjectEnv, ""), "dagger project a bare #7 reference resolves in (default: $"+daggerProjectEnv+")")
}

// daggerFor resolves gate nodes through dagger. A gate the service cannot be
// asked about yields its id and no title: a read tool that cannot reach
// dagger still answers everything the ledger knows.
func daggerFor(opts readOptions) gates {
	client := dagger.Dial(opts.daggerSocket, opts.project)
	return func(ctx context.Context, nodeRef, runID, gateNodeID string) (string, string) {
		// A dagger reference is `#7`, never an id, so a gate id is resolved
		// the way the run loop resolves it: the gates filed on the run's own
		// node, matched by id (docs/domain-model.md §Gate).
		filed, err := client.Gates(ctx, nodeRef, dagger.GateLabel, dagger.RunLabel(runID))
		if err != nil {
			return gateNodeID, ""
		}
		for _, gate := range filed {
			if gate.GetId() == gateNodeID {
				return gate.GetReference(), gate.GetTitle()
			}
		}
		return gateNodeID, ""
	}
}

// elapsed is how long a run has been going, or how long it took.
func elapsed(run *millv1.Run) string {
	end := time.Now()
	if run.GetEndedAt() != nil {
		end = run.GetEndedAt().AsTime()
	}
	return end.Sub(run.GetCreatedAt().AsTime()).Round(time.Second).String()
}

// tokensCounted is an attempt's TOKENS cell: the adapter's report, and
// beside it what the credential gateway counted when one sat in front of
// the session, marked as the gateway's (docs/sandbox.md §Credentials).
func tokensCounted(step *millv1.StepRunDetail) string {
	text := tokenText(step.GetTotals())
	if len(step.GetGatewayTotals()) > 0 {
		text += " gateway:" + tokenText(step.GetGatewayTotals())
	}
	return text
}

// tokenText is a usage rollup as one cell: input and output, summed.
func tokenText(totals []*millv1.ModelTotals) string {
	if len(totals) == 0 {
		return "-"
	}
	in, out, _, _ := sum(totals)
	return fmt.Sprintf("%d/%d", in, out)
}

// sum adds a rollup's models together.
func sum(totals []*millv1.ModelTotals) (in, out, read, write int64) {
	for _, total := range totals {
		in += total.GetInputTokens()
		out += total.GetOutputTokens()
		read += total.GetCacheReadTokens()
		write += total.GetCacheWriteTokens()
	}
	return in, out, read, write
}

// stepValue is an attempt's evidence as inspect prints it. A commit is shown
// as the range it closed on — the head the attempt started at, then the
// commit it made, both abbreviated — because "what did this step add" is the
// question the base commit exists to answer.
func stepValue(sr *millv1.StepRun, value *millv1.Evidence) string {
	commit, ok := value.GetValue().(*millv1.Evidence_Commit)
	if !ok || sr.GetBaseCommit() == "" {
		return evidenceValue(value)
	}
	return short(sr.GetBaseCommit()) + ".." + short(commit.Commit)
}

// evidenceValue is the evidence a step closed on, as one cell. The claim
// itself — a SHA, a URL, a digest — never a transcript.
func evidenceValue(value *millv1.Evidence) string {
	switch body := value.GetValue().(type) {
	case *millv1.Evidence_Commit:
		return body.Commit
	case *millv1.Evidence_Url:
		return body.Url
	case *millv1.Evidence_Artifact:
		return body.Artifact
	case *millv1.Evidence_Gate:
		return body.Gate
	case *millv1.Evidence_Exit:
		return exitText(body.Exit)
	case *millv1.Evidence_Status:
		return body.Status
	case *millv1.Evidence_Verdict:
		return verdictValue(body.Verdict)
	case *millv1.Evidence_Structured:
		return fields(body.Structured)
	default:
		return "-"
	}
}

// verdictValue is a verdict as one cell: the decision, and then the findings
// that blocked it. The blocking findings are inline because they are why the
// step did not close and what the next attempt has to answer, and reading
// them should not cost a second command; notes are not, because they did not
// decide anything. The whole verdict, findings and all, is on the node as a
// text artifact (docs/domain-model.md §Verdict).
func verdictValue(verdict *millv1.Verdict) string {
	decision := strings.ToLower(strings.TrimPrefix(verdict.GetDecision().String(), "VERDICT_DECISION_"))
	var blocking []string
	for _, finding := range verdict.GetFindings() {
		if finding.GetSeverity() != millv1.FindingSeverity_FINDING_SEVERITY_BLOCKING {
			continue
		}
		text := finding.GetSummary()
		if criterion := finding.GetCriterion(); criterion != "" {
			text = criterion + ": " + text
		}
		blocking = append(blocking, text)
	}
	if len(blocking) == 0 {
		return decision
	}
	return decision + " (" + strings.Join(blocking, "; ") + ")"
}

// fields renders a struct column as `key=value` pairs, sorted, so two
// renderings of one row read alike.
func fields(value *structpb.Struct) string {
	if value == nil || len(value.GetFields()) == 0 {
		return "-"
	}
	pairs := make([]string, 0, len(value.GetFields()))
	for _, key := range sorted(value.GetFields()) {
		pairs = append(pairs, key+"="+scalar(value.GetFields()[key]))
	}
	return strings.Join(pairs, " ")
}

// scalar renders one struct field for a table cell. Nested values are
// rendered as their JSON, which is what an audit row holds when it holds
// anything structured at all.
func scalar(value *structpb.Value) string {
	switch body := value.GetKind().(type) {
	case *structpb.Value_StringValue:
		return body.StringValue
	case *structpb.Value_BoolValue:
		return fmt.Sprint(body.BoolValue)
	case *structpb.Value_NumberValue:
		return strings.TrimSuffix(strconv.FormatFloat(body.NumberValue, 'f', -1, 64), ".0")
	case *structpb.Value_NullValue, nil:
		return "null"
	default:
		raw, err := value.MarshalJSON()
		if err != nil {
			return "?"
		}
		return string(raw)
	}
}

// table is a tab writer over the command's output, which is how every list
// this package prints is aligned.
func table(cmd *cobra.Command) *tabwriter.Writer {
	return tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
}

// timestamp is an instant on the wire.
func timestamp(at time.Time) *timestamppb.Timestamp { return timestamppb.New(at) }

// exitText is how a process ended, as one cell. A status of -1 means the
// session never ran and the reason says why, which is the distinction the
// harness contract exists to keep.
func exitText(exit *harnessv1.ProcessExit) string {
	if exit == nil {
		return ""
	}
	if exit.GetStatus() < 0 {
		return "never-ran:" + enumWord(exit.GetReason().String(), "EXIT_REASON_")
	}
	if exit.GetSignal() != 0 {
		return fmt.Sprintf("%d signal %d", exit.GetStatus(), exit.GetSignal())
	}
	// A session that ran can still carry a reason: the breaker stops a
	// stalled one at a status of its own choosing, and the status alone
	// would say only that the attempt failed.
	if exit.GetReason() != harnessv1.ExitReason_EXIT_REASON_UNSPECIFIED {
		return fmt.Sprintf("%d:%s", exit.GetStatus(), enumWord(exit.GetReason().String(), "EXIT_REASON_"))
	}
	return fmt.Sprint(exit.GetStatus())
}

// stepNamed finds a step in a workflow definition.
func stepNamed(workflow *millv1.Workflow, name string) *millv1.Step {
	for _, step := range workflow.GetSteps() {
		if step.GetName() == name {
			return step
		}
	}
	return nil
}

// runStatusName, stepRunStatusName and parkedReasonName render enums as the
// ledger's own words.
func runStatusName(status millv1.RunStatus) string {
	return enumWord(status.String(), "RUN_STATUS_")
}

func stepRunStatusName(status millv1.StepRunStatus) string {
	return enumWord(status.String(), "STEP_RUN_STATUS_")
}

func parkedReasonName(reason millv1.ParkedReason) string {
	if reason == millv1.ParkedReason_PARKED_REASON_UNSPECIFIED {
		return ""
	}
	return enumWord(reason.String(), "PARKED_REASON_")
}

func enumWord(name, prefix string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(name, prefix), "_", "-"))
}

// dash renders an empty cell as a dash, so a table's columns stay legible.
func dash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

// sorted is a map's keys in order.
func sorted[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
