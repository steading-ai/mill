package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/workspace/local"
)

// `mill workspace` is the two questions an operator has about the files a
// run left on this host: where are they, and may they go.
//
// A workspace is destroyed when its run ends well and retained while the run
// is waiting or parked, so that the worker that resumes it — or the human
// inspecting it — finds the tree intact (docs/domain-model.md §Workspace).
// What is left over is the residue of runs that ended some other way: a run
// abandoned by a restart, a run failed by a refusal, a run whose worker died
// before it could destroy anything.
//
// Neither subcommand opens a workspace. `show` reads the checkout with git
// and stats the socket; `clean` removes directories. Attaching would bind
// the socket and check the branch out, which is more than a question
// deserves and would collide with a worker still holding the run.

// terminalStatuses are the runs whose workspace has nothing left to say:
// done, failed, or abandoned. `clean` removes only these, because a waiting
// or parked run's checkout is what its resume attaches to.
var terminalStatuses = []millv1.RunStatus{
	millv1.RunStatus_RUN_STATUS_DONE,
	millv1.RunStatus_RUN_STATUS_FAILED,
	millv1.RunStatus_RUN_STATUS_ABANDONED,
}

type workspaceOptions struct {
	socket       string
	workspaceDir string
	olderThan    string
	dryRun       bool
}

func newWorkspaceCommand() *cobra.Command {
	opts := workspaceOptions{}
	cmd := &cobra.Command{
		Use:   "workspace",
		Short: "Show and clean up the workspaces this host is holding",
		Long: `A run's workspace is a clone under the worker's account, one directory per
run, reached by its session over a unix socket.

"show" says where one run's workspace is and what state it is in.
"clean" removes the workspaces of runs that have ended, and only those.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	f := cmd.PersistentFlags()
	f.StringVar(&opts.socket, "socket", defaultSocketPath(), "path of the service's unix socket")
	f.StringVar(&opts.workspaceDir, "workspace-dir", defaultWorkspaceDir(), "directory run workspaces live under")
	cmd.AddCommand(newWorkspaceShowCommand(&opts), newWorkspaceCleanCommand(&opts))
	return cmd
}

func newWorkspaceShowCommand(opts *workspaceOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "show <run>",
		Short: "Say where a run's workspace is and what state it is in",
		Long: `Print the checkout's path, the branch it is on, where that branch points,
whether the tree is clean, whether the workspace's socket is still bound,
and why the workspace is still here.

This host's workspaces only: a run worked elsewhere has nothing here to
show.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkspaceShow(cmd, opts, args[0])
		},
	}
}

func runWorkspaceShow(cmd *cobra.Command, opts *workspaceOptions, reference string) error {
	store := storeClient(opts.socket)
	detail, err := inspected(cmd.Context(), store, "workspace show", reference)
	if err != nil {
		return err
	}
	row := detail.GetSummary().GetRun()
	info, err := inspector(opts).Inspect(cmd.Context(), row.GetId())
	if err != nil {
		return fmt.Errorf("workspace show %s: %w", reference, err)
	}
	return printWorkspace(cmd, row, info)
}

// printWorkspace is one workspace as a field list: what an operator needs
// before they go and look at the tree themselves.
func printWorkspace(cmd *cobra.Command, row *millv1.Run, info *local.Info) error {
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "%s run %s\n", row.GetNodeRef(), row.GetId())
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, field := range [][2]string{
		{"path", info.Checkout},
		{"branch", dash(info.Branch)},
		{"head", dash(info.Head)},
		{"tree", treeText(info)},
		{"socket", socketText(info)},
		{"retained", retainedText(row)},
	} {
		_, _ = fmt.Fprintf(w, "  %s\t%s\n", field[0], field[1])
	}
	if info.Detail != "" {
		_, _ = fmt.Fprintf(w, "  %s\t%s\n", "detail", info.Detail)
	}
	return w.Flush()
}

// treeText is whether git sees anything uncommitted, and how much.
func treeText(info *local.Info) string {
	if info.Detail != "" {
		return "unknown"
	}
	if info.Clean {
		return "clean"
	}
	return fmt.Sprintf("dirty (%d changed)", info.Changed)
}

// socketText is whether a session could still reach this workspace.
func socketText(info *local.Info) string {
	if info.SocketPresent {
		return info.Socket + " (present)"
	}
	return info.Socket + " (absent)"
}

// retainedText is why the workspace is still on disk: the run's own status,
// which is the whole of the rule (docs/domain-model.md §Workspace).
func retainedText(row *millv1.Run) string {
	switch row.GetStatus() {
	case millv1.RunStatus_RUN_STATUS_WAITING:
		return "the run is waiting on a gate"
	case millv1.RunStatus_RUN_STATUS_PARKED:
		return "the run is parked for a human"
	case millv1.RunStatus_RUN_STATUS_RUNNING, millv1.RunStatus_RUN_STATUS_SELECTING:
		return "the run is being worked"
	case millv1.RunStatus_RUN_STATUS_DONE, millv1.RunStatus_RUN_STATUS_FAILED, millv1.RunStatus_RUN_STATUS_ABANDONED:
		return "the run has ended; `mill workspace clean` removes it"
	case millv1.RunStatus_RUN_STATUS_UNSPECIFIED:
		return "unknown"
	default:
		return "unknown"
	}
}

func newWorkspaceCleanCommand(opts *workspaceOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "clean",
		Short: "Remove the workspaces of runs that have ended",
		Long: `Remove the workspace of every run this host holds one for that is done,
failed, or abandoned.

A run that is selecting, running, waiting or parked keeps its workspace
whatever its age: a waiting or parked run's checkout is what its resume
attaches to, and a running one's is in use. A directory whose run the ledger
does not know is left alone too, because deciding it is rubbish needs a
record that says so.

"--dry-run" lists what would go and removes nothing.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWorkspaceClean(cmd, opts)
		},
	}
	cmd.Flags().StringVar(&opts.olderThan, "older-than", "", "only workspaces of runs that ended longer ago than this, such as 7d")
	cmd.Flags().BoolVar(&opts.dryRun, "dry-run", false, "list what would be removed and remove nothing")
	return cmd
}

func runWorkspaceClean(cmd *cobra.Command, opts *workspaceOptions) error {
	removable, err := cleanable(cmd.Context(), opts)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if len(removable) == 0 {
		_, _ = fmt.Fprintf(out, "nothing to remove under %s\n", opts.workspaceDir)
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "RUN\tNODE\tSTATUS\tPATH\tACTION")
	i := inspector(opts)
	for _, c := range removable {
		action := "removed"
		if opts.dryRun {
			action = "would remove"
		} else if err := i.Remove(c.runID); err != nil {
			_ = w.Flush()
			return fmt.Errorf("workspace clean: %w", err)
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", c.runID, c.nodeRef, c.status, c.dir, action)
	}
	return w.Flush()
}

// candidate is one workspace clean would remove, with the ledger's reason.
type candidate struct {
	runID   string
	nodeRef string
	status  string
	dir     string
}

// cleanable pairs the directories this host holds with the ledger's word on
// their runs, and keeps the ones whose run has ended. The ledger decides,
// not the directory: a checkout with nothing in it belonging to a parked run
// is still that run's to resume into.
func cleanable(ctx context.Context, opts *workspaceOptions) ([]candidate, error) {
	held, err := inspector(opts).RunIDs()
	if err != nil {
		return nil, fmt.Errorf("workspace clean: %w", err)
	}
	if len(held) == 0 {
		return nil, nil
	}
	listed, err := storeClient(opts.socket).Status(ctx, connect.NewRequest(&millv1.StatusRequest{
		Statuses: terminalStatuses,
	}))
	if err != nil {
		return nil, fmt.Errorf("workspace clean: %w", err)
	}
	var before time.Time
	if opts.olderThan != "" {
		window, err := parseWindow(opts.olderThan)
		if err != nil {
			return nil, fmt.Errorf("--older-than: %q is not a window such as 24h or 7d", opts.olderThan)
		}
		before = time.Now().Add(-window)
	}

	ended := map[string]*millv1.Run{}
	for _, summary := range listed.Msg.GetRuns() {
		row := summary.GetRun()
		if !before.IsZero() && !row.GetEndedAt().AsTime().Before(before) {
			continue
		}
		ended[row.GetId()] = row
	}
	var out []candidate
	for _, runID := range held {
		row, ok := ended[runID]
		if !ok {
			continue
		}
		out = append(out, candidate{
			runID:   runID,
			nodeRef: row.GetNodeRef(),
			status:  runStatusName(row.GetStatus()),
			dir:     filepath.Join(opts.workspaceDir, runID),
		})
	}
	return out, nil
}

// inspector reads the workspaces under the root the flags name, opening
// none of them.
func inspector(opts *workspaceOptions) *local.Inspector {
	return local.NewInspector(local.Options{Root: opts.workspaceDir})
}
