package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	millv1 "github.com/ivy/mill/gen/mill/v1"
)

// `mill rerun <run> --from <step>` is "the work up to here is fine, redo
// from this step". It supersedes the latest attempt at every step from the
// named one through the step the run is on — the same mechanism a `revise`
// verdict uses (internal/run/revise.go) — and leaves the run resumable at
// the named step.
//
// Rerun and restart are different acts. A rerun keeps the generation, the
// branch and the sessions, so the work before the named step stands and the
// session resumes with its transcript; a restart opens a new generation with
// new session ids and throws the run's work away (docs/domain-model.md
// §Restart).
//
// Like cancel, it carries no lease: the caller is a person. What guards it
// is the socket's peer uid, and the store refuses a run a worker is still
// holding, because superseding a span under a running step would have the
// worker close a step the ledger has already put behind it.

func newRerunCommand() *cobra.Command {
	opts := readOptions{}
	from := ""
	cmd := &cobra.Command{
		Use:   "rerun <run> --from <step>",
		Short: "Redo a run from a step, keeping the generation and the sessions",
		Long: `Supersede every step from the one named through the step the run is on, and
leave the run resumable at the named step.

The superseded attempts stay in the ledger with the evidence they produced;
they simply close nothing, which is what moves the run backwards. "mill run"
on the same node then starts at the named step, in the same generation, on
the same branch, resuming the same sessions.

"mill restart" is the other verb: a new generation with new session ids,
which throws the run's work away. This keeps it.

A run that failed is reopened by the same act, because a rerun is a person
overruling the ending. A run a later generation has replaced is not: use
"mill restart" on the node instead.

A run a worker is still holding is refused naming the holder: cancel it
first. A step the run has never attempted is refused naming the steps it
has.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if from == "" {
				return fmt.Errorf("mill rerun: --from names the step to redo from")
			}
			return runRerun(cmd, opts, args[0], from)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.socket, "socket", defaultSocketPath(), "path of the service's unix socket")
	f.StringVar(&from, "from", "", "the step to redo from; every step from it through the current one is superseded")
	f.StringVar(&opts.reason, "reason", "", "why, recorded on the run's audit trail")
	return cmd
}

func runRerun(cmd *cobra.Command, opts readOptions, reference, from string) error {
	store := storeClient(opts.socket)
	// A rerun takes a run id and the operator has a node reference as often
	// as not, so the id is resolved first through the read every other tool
	// uses.
	detail, err := inspected(cmd.Context(), store, "rerun", reference)
	if err != nil {
		return err
	}
	row := detail.GetSummary().GetRun()
	res, err := store.Rerun(cmd.Context(), connect.NewRequest(&millv1.RerunRequest{
		RunId: row.GetId(), FromStep: from, Reason: opts.reason,
	}))
	if err != nil {
		return fmt.Errorf("rerun %s: %w", reference, err)
	}
	return printRerun(cmd, row, from, res.Msg)
}

// printRerun says what was superseded, one line per attempt, and where the
// run stands now. The attempts are the whole of what a rerun did.
func printRerun(cmd *cobra.Command, row *millv1.Run, from string, done *millv1.RerunResponse) error {
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "rerun %s run %s from %s through %s\n",
		row.GetNodeRef(), row.GetId(), from, done.GetThroughStep())
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "STEP\tATTEMPT\tSTATUS\tREASON")
	for _, sr := range done.GetStepRuns() {
		_, _ = fmt.Fprintf(w, "%s\t%d\t%s\t%s\n",
			sr.GetStepName(), sr.GetAttempt(), stepRunStatusName(sr.GetStatus()), "rerun")
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if done.GetReopened() {
		_, _ = fmt.Fprintf(out, "\n%s had ended %s and is live again\n",
			row.GetNodeRef(), runStatusName(row.GetStatus()))
	}
	_, _ = fmt.Fprintf(out,
		"\n%s is resumable at %s in generation %d; the sessions and the branch are kept\n",
		row.GetNodeRef(), from, done.GetRun().GetGeneration())
	return nil
}

// rerunSteps is the step names a rerun superseded, for the MCP tool's answer
// and for anything else that wants the span rather than the rows.
func rerunSteps(done *millv1.RerunResponse) []string {
	names := make([]string, 0, len(done.GetStepRuns()))
	for _, sr := range done.GetStepRuns() {
		names = append(names, sr.GetStepName())
	}
	return names
}

// rerunSummary is the line a session reads: what was undone, and where the
// run now stands.
func rerunSummary(from string, done *millv1.RerunResponse) string {
	summary := fmt.Sprintf("superseded %s; resumable at %s in generation %d",
		strings.Join(rerunSteps(done), ", "), from, done.GetRun().GetGeneration())
	if done.GetReopened() {
		summary += "; the run had ended and is live again"
	}
	return summary
}
