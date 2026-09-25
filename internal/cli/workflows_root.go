package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/domain"
	"github.com/steading-ai/mill/internal/workflow"
)

// pushWorkflowsRoot pushes every workflow directory under root — one
// carrying a workflow.yaml — the way `mill workflow push` pushes one by
// hand, so the store never falls behind the tree a deployment put in
// service. An empty root pushes nothing, the way an empty `--workflows-root`
// or an absent one always has.
//
// A definition the loader refuses is returned as an error naming the
// directory and, from Load itself, the step; nothing under root is pushed
// once one has failed, matching a directory tree where a bad definition in
// one workflow should not leave the others silently pushed while the one
// that broke is skipped.
func pushWorkflowsRoot(ctx context.Context, svc *domain.Service, root string, hostMcpServers map[string]struct{}, log *slog.Logger) error {
	if root == "" {
		return nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("workflows-root %s: %w", root, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		if _, err := os.Stat(filepath.Join(dir, workflow.DefinitionFile)); err != nil {
			continue
		}
		if err := pushWorkflowDir(ctx, svc, dir, hostMcpServers, log); err != nil {
			return fmt.Errorf("workflows-root %s: %w", root, err)
		}
	}
	return nil
}

// pushWorkflowDir loads and stores one workflow directory, the version and
// every skill its steps pin, stamped with SOURCE_KIND_REPO rather than the
// SOURCE_KIND_CLI a person's `mill workflow push` stamps: this push came
// from the deployed tree, not from a terminal.
func pushWorkflowDir(ctx context.Context, svc *domain.Service, dir string, hostMcpServers map[string]struct{}, log *slog.Logger) error {
	definition, skills, err := workflow.Load(dir)
	if err != nil {
		return fmt.Errorf("%s: %w", dir, err)
	}
	source := &millv1.Source{Kind: millv1.SourceKind_SOURCE_KIND_REPO, Path: absPath(dir), PushedBy: "mill serve"}
	for _, skill := range skills {
		skill.Source = source
		stored, created, err := svc.PutSkill(ctx, skill)
		if err != nil {
			return fmt.Errorf("%s: skill %s: %w", dir, skill.GetName(), err)
		}
		log.Info("workflow skill pushed", "dir", dir, "skill", stored.GetName(), "digest", short(stored.GetDigest()), "state", stateOf(created))
	}
	definition.Source = source
	stored, created, err := svc.PutWorkflow(ctx, definition, hostMcpServers)
	if err != nil {
		return fmt.Errorf("%s: workflow %s: %w", dir, definition.GetName(), err)
	}
	log.Info("workflow pushed", "dir", dir, "workflow", stored.GetName(), "version", short(stored.GetVersion()), "state", stateOf(created))
	return nil
}
