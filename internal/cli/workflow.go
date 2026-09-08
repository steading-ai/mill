package cli

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/gen/mill/v1/millv1connect"
	"github.com/ivy/mill/internal/workflow"
)

// workflowOptions are what every workflow subcommand needs: where the
// service is, and which scope to speak about.
type workflowOptions struct {
	socket      string
	scope       string
	project     string
	version     string
	name        string
	view        string
	allVersions bool
}

// newWorkflowCommand is the operate face of workflows: files in, versions
// out. A workflow is data (docs/domain-model.md §Workflow), so the CLI's
// whole job here is to hash a directory, hand it to the store, and hand it
// back.
func newWorkflowCommand() *cobra.Command {
	opts := workflowOptions{}
	cmd := &cobra.Command{
		Use:   "workflow",
		Short: "Push, pull, and list workflow definitions",
		Long: `Workflows are data: a workflow.yaml beside a skills/ directory, stored as an
immutable version identified by the hash of its content.

"push" loads a directory, validates it, and stores the version and its
skills. Pushing the same files twice stores nothing new.
"pull" writes a stored version back out as a directory push accepts.
"list" shows what is stored.
"show" prints one version's steps: what each closes on, what it may do, and
what its failure does to the run.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.PersistentFlags().StringVar(&opts.socket, "socket", defaultSocketPath(), "path of the service's unix socket")
	cmd.AddCommand(
		newWorkflowPushCommand(&opts),
		newWorkflowPullCommand(&opts),
		newWorkflowListCommand(&opts),
		newWorkflowShowCommand(&opts),
	)
	return cmd
}

func newWorkflowPushCommand(opts *workflowOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "push <dir>",
		Short: "Store a workflow directory as a version",
		Long: `Load a workflow directory, refuse it if it is malformed, and store the
version and every skill its steps pin.

The version is the hash of the definition, and a step pins its skill by
name and digest, so changing one byte of one skill body is a new skill
digest and a new workflow version. Pushing an unchanged directory stores
nothing and reports the version already there.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkflowPush(cmd, opts, args[0])
		},
	}
	cmd.Flags().StringVar(&opts.scope, "scope", "", "override the definition's scope: org or project")
	cmd.Flags().StringVar(&opts.project, "project", "", "the project, when the scope is project")
	return cmd
}

func runWorkflowPush(cmd *cobra.Command, opts *workflowOptions, dir string) error {
	definition, skills, err := workflow.Load(dir)
	if err != nil {
		return err
	}
	if opts.scope != "" {
		scope, ok := map[string]millv1.Scope{
			"org":     millv1.Scope_SCOPE_ORG,
			"project": millv1.Scope_SCOPE_PROJECT,
		}[opts.scope]
		if !ok {
			return fmt.Errorf("--scope: %q is not org or project", opts.scope)
		}
		definition.Scope = scope
	}
	if opts.project != "" {
		definition.Project = opts.project
	}
	if (definition.GetScope() == millv1.Scope_SCOPE_PROJECT) != (definition.GetProject() != "") {
		return fmt.Errorf("workflow %s: a project scope names a project and an org scope names none", definition.GetName())
	}
	source := pushSource(dir)
	definition.Source = source

	client := storeClient(opts.socket)
	out := cmd.OutOrStdout()
	for _, skill := range skills {
		skill.Source = source
		stored, err := client.PutSkill(cmd.Context(), connect.NewRequest(&millv1.PutSkillRequest{Skill: skill}))
		if err != nil {
			return fmt.Errorf("push skill %s: %w", skill.GetName(), err)
		}
		_, _ = fmt.Fprintf(out, "skill %s %s %s\n", skill.GetName(), short(skill.GetDigest()), stateOf(stored.Msg.GetCreated()))
	}
	stored, err := client.PutWorkflow(cmd.Context(), connect.NewRequest(&millv1.PutWorkflowRequest{Workflow: definition}))
	if err != nil {
		return fmt.Errorf("push workflow %s: %w", definition.GetName(), err)
	}
	_, _ = fmt.Fprintf(out, "workflow %s %s %s %s\n",
		stored.Msg.GetWorkflow().GetName(),
		scopeText(stored.Msg.GetWorkflow()),
		short(stored.Msg.GetWorkflow().GetVersion()),
		stateOf(stored.Msg.GetCreated()))
	return nil
}

func newWorkflowPullCommand(opts *workflowOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pull <name> <dir>",
		Short: "Write a stored workflow version out as a directory",
		Long: `Write a stored version back out as workflow.yaml and one SKILL.md per
skill step, in a directory push accepts unchanged.

A skill that carries scripts, assets, or references cannot be written back:
the store keeps their digests, not their bytes.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkflowPull(cmd, opts, args[0], args[1])
		},
	}
	cmd.Flags().StringVar(&opts.scope, "scope", "", "resolve in this scope: org or project")
	cmd.Flags().StringVar(&opts.project, "project", "", "the project, when the scope is project")
	cmd.Flags().StringVar(&opts.version, "version", "", "a specific version, rather than the newest")
	return cmd
}

func runWorkflowPull(cmd *cobra.Command, opts *workflowOptions, name, dir string) error {
	client := storeClient(opts.socket)
	found, err := client.GetWorkflow(cmd.Context(), connect.NewRequest(&millv1.GetWorkflowRequest{
		Name:    name,
		Scope:   parseScope(opts.scope),
		Project: opts.project,
		Version: opts.version,
	}))
	if err != nil {
		return fmt.Errorf("pull workflow %s: %w", name, err)
	}
	definition := found.Msg.GetWorkflow()
	skills, err := pullSkills(cmd.Context(), client, definition)
	if err != nil {
		return err
	}
	if err := workflow.Write(dir, definition, skills); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "workflow %s %s %s written to %s\n",
		definition.GetName(), scopeText(definition), short(definition.GetVersion()), dir)
	return nil
}

// pullSkills fetches every skill the definition's steps pin, once each.
func pullSkills(ctx context.Context, client millv1connect.StoreServiceClient, definition *millv1.Workflow) ([]*millv1.Skill, error) {
	seen := map[string]bool{}
	var skills []*millv1.Skill
	for _, step := range definition.GetSteps() {
		ref := step.GetSkill()
		if ref.GetName() == "" || seen[ref.GetName()] {
			continue
		}
		seen[ref.GetName()] = true
		found, err := client.GetSkill(ctx, connect.NewRequest(&millv1.GetSkillRequest{
			Name: ref.GetName(), Digest: ref.GetDigest(),
		}))
		if err != nil {
			return nil, fmt.Errorf("pull skill %s: %w", ref.GetName(), err)
		}
		skills = append(skills, found.Msg.GetSkill())
	}
	return skills, nil
}

func newWorkflowListCommand(opts *workflowOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List stored workflow versions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWorkflowList(cmd, opts)
		},
	}
	cmd.Flags().StringVar(&opts.name, "name", "", "only this workflow")
	cmd.Flags().StringVar(&opts.scope, "scope", "", "only this scope: org or project")
	cmd.Flags().StringVar(&opts.project, "project", "", "only this project")
	cmd.Flags().BoolVar(&opts.allVersions, "all-versions", false, "every version, not just the newest of each name")
	return cmd
}

func runWorkflowList(cmd *cobra.Command, opts *workflowOptions) error {
	listed, err := storeClient(opts.socket).ListWorkflows(cmd.Context(), connect.NewRequest(&millv1.ListWorkflowsRequest{
		Name:        opts.name,
		Scope:       parseScope(opts.scope),
		Project:     opts.project,
		AllVersions: opts.allVersions,
	}))
	if err != nil {
		return fmt.Errorf("list workflows: %w", err)
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "NAME\tSCOPE\tVERSION\tSTEPS\tCREATED")
	for _, definition := range listed.Msg.GetWorkflows() {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\n",
			definition.GetName(), scopeText(definition), short(definition.GetVersion()),
			len(definition.GetSteps()), definition.GetCreatedAt().AsTime().Format("2006-01-02 15:04"))
	}
	return w.Flush()
}

func newWorkflowShowCommand(opts *workflowOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <name>",
		Short: "Print one workflow version's steps",
		Long: `Print a stored version as a run would walk it: every step in order, its
kind, what evidence closes it, the capabilities its workspace handle grants,
how many times it may fail before the breaker parks the run, and what a
failure does.

This is the definition, not a run. "mill inspect" shows what a run made of
it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkflowShow(cmd, opts, args[0])
		},
	}
	cmd.Flags().StringVar(&opts.scope, "scope", "", "resolve in this scope: org or project")
	cmd.Flags().StringVar(&opts.project, "project", "", "the project, when the scope is project")
	cmd.Flags().StringVar(&opts.version, "version", "", "a specific version, rather than the newest")
	cmd.Flags().StringVar(&opts.view, "view", string(viewSummary), "how much to show: "+strings.Join(views, ", "))
	return cmd
}

func runWorkflowShow(cmd *cobra.Command, opts *workflowOptions, name string) error {
	v, err := parseViewName(opts.view)
	if err != nil {
		return err
	}
	found, err := storeClient(opts.socket).GetWorkflow(cmd.Context(), connect.NewRequest(&millv1.GetWorkflowRequest{
		Name:    name,
		Scope:   parseScope(opts.scope),
		Project: opts.project,
		Version: opts.version,
	}))
	if err != nil {
		return fmt.Errorf("show workflow %s: %w", name, err)
	}
	definition := found.Msg.GetWorkflow()
	if definition == nil {
		return fmt.Errorf("show workflow %s: no such workflow is stored", name)
	}
	return renderWorkflow(cmd, definition, v)
}

// renderWorkflow prints a definition's header and one row per step.
func renderWorkflow(cmd *cobra.Command, definition *millv1.Workflow, v view) error {
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "%s %s %s\n", definition.GetName(), scopeText(definition), definition.GetVersion())
	if v == viewFull {
		_, _ = fmt.Fprintf(out, "  stored  %s\n", definition.GetCreatedAt().AsTime().Format(time.RFC3339))
		_, _ = fmt.Fprintf(out, "  source  %s\n", dash(definition.GetSource().GetPath()))
	}
	_, _ = fmt.Fprintln(out)

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if v == viewRefs {
		_, _ = fmt.Fprintln(w, "STEP\tKIND")
		for _, step := range definition.GetSteps() {
			_, _ = fmt.Fprintf(w, "%s\t%s\n", step.GetName(), stepKindName(step.GetKind()))
		}
		return w.Flush()
	}
	_, _ = fmt.Fprintln(w, "STEP\tKIND\tEVIDENCE\tCAPABILITIES\tRETRIES\tON FAILURE\tSESSION")
	for _, step := range definition.GetSteps() {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
			step.GetName(), stepKindName(step.GetKind()), evidenceKindName(step.GetEvidence()),
			dash(strings.Join(capabilityNames(step.GetCapabilities()), ",")),
			step.GetRetries(), onFailureText(step.GetOnFailure()), dash(step.GetSession()))
	}
	return w.Flush()
}

// onFailureText is what a step's failure does to the run: park, or revise a
// named earlier step.
func onFailureText(onFailure *millv1.OnFailure) string {
	if onFailure.GetKind() == millv1.OnFailureKind_ON_FAILURE_KIND_REVISE {
		return "revise:" + onFailure.GetReviseTarget()
	}
	return "park"
}

// pushSource records who pushed and from where. It is excluded from the
// version hash, so two people pushing the same files still store one
// version.
func pushSource(dir string) *millv1.Source {
	path, err := filepath.Abs(dir)
	if err != nil {
		path = dir
	}
	pushedBy := os.Getenv("USER")
	if current, err := user.Current(); err == nil {
		pushedBy = current.Username
	}
	return &millv1.Source{Kind: millv1.SourceKind_SOURCE_KIND_CLI, Path: path, PushedBy: pushedBy}
}

func parseScope(text string) millv1.Scope {
	switch text {
	case "org":
		return millv1.Scope_SCOPE_ORG
	case "project":
		return millv1.Scope_SCOPE_PROJECT
	default:
		return millv1.Scope_SCOPE_UNSPECIFIED
	}
}

func scopeText(definition *millv1.Workflow) string {
	if definition.GetScope() == millv1.Scope_SCOPE_PROJECT {
		return "project:" + definition.GetProject()
	}
	return "org"
}

// stateOf says whether a push stored something or found it already there,
// which is the whole of what idempotency looks like from a terminal.
func stateOf(created bool) string {
	if created {
		return "stored"
	}
	return "unchanged"
}

// short is a content hash as a human reads it. The full hash is what the
// store and the ledger carry.
func short(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}
