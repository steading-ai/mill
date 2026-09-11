package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/ivy/mill/internal/dagger"
	"github.com/ivy/mill/internal/forge"
	"github.com/ivy/mill/internal/forge/gitlab"
	"github.com/ivy/mill/internal/run"
	"github.com/ivy/mill/internal/sweep"
	"github.com/ivy/mill/internal/workspace"
	"github.com/ivy/mill/internal/workspace/machine"
	"github.com/ivy/mill/internal/workspace/machine/image"
)

// defaultImageExpiry is how long an unbooted image lives when nothing
// configures it (docs/sandbox.md §Eviction).
const defaultImageExpiry = 30 * 24 * time.Hour

// machineStartup is what a machine worker does before its first claim
// (run.Startup): docs/sandbox.md §Reconciliation's pass over the host,
// recorded in the ledger, then eviction — expiry of unbooted images today;
// the watermarks are the eviction node's.
type machineStartup struct {
	reconciler *machine.Reconciler
	ledger     machine.Ledger
	images     *image.Store
	expiry     time.Duration
	out        io.Writer
}

// workerStartup is the startup for the configured backend: the machine
// backend's pass, or nothing for the local one. The sweep's dagger client is
// the worker's own — the configured socket and project, the worker's agent
// ref — and never one the environment names.
func workerStartup(kind workspace.Kind, opts runOptions, project, workerRef string, out io.Writer) (run.Startup, error) {
	if kind != workspace.KindMachine {
		return nil, nil
	}
	config := machineConfig(opts, nil, nil)
	externals := []machine.External{
		sweep.Dagger(dagger.Dial(opts.daggerSocket, project, dagger.WithAgentRef(workerRef))),
	}
	if opts.sandbox.scratchNamespace != "" {
		repo, err := forge.RepoFor(forge.Project{Slug: project, VCSURL: opts.vcsURL})
		if err != nil {
			return nil, err
		}
		externals = append(externals, sweep.Forge(gitlab.NewScratch(repo.BaseURL, opts.sandbox.scratchNamespace,
			forge.Static(envOr(forgeUserEnv, defaultForgeUser), os.Getenv(forgeTokenEnv)))))
	}
	expiry := opts.sandbox.imageExpiry
	if expiry <= 0 {
		expiry = defaultImageExpiry
	}
	return &machineStartup{
		reconciler: machine.NewReconciler(machineDriver(), config, externals...),
		ledger:     run.NewLedger(storeClient(opts.socket), workerRef),
		images:     image.New(machineDriver(), config.Images),
		expiry:     expiry,
		out:        out,
	}, nil
}

// Reconcile runs the pass and prints its findings, one line each.
func (s *machineStartup) Reconcile(ctx context.Context) error {
	report, err := s.reconciler.Reconcile(ctx, s.ledger)
	for _, finding := range report.Findings {
		_, _ = fmt.Fprintf(s.out, "reconcile: %s\n", finding)
	}
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(s.out, "reconcile: %d findings recorded\n", len(report.Findings))
	return nil
}

// Evict removes the image subvolumes nothing has booted within the expiry.
func (s *machineStartup) Evict(ctx context.Context) error {
	pruned, err := s.images.Prune(ctx, s.expiry)
	for _, manifest := range pruned {
		_, _ = fmt.Fprintf(s.out, "evict: image %s, last used %s\n", manifest.Digest.Short(), manifest.LastUsed().Format(time.RFC3339))
	}
	return err
}
