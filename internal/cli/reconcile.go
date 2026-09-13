package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/ivy/mill/internal/dagger"
	"github.com/ivy/mill/internal/forge"
	"github.com/ivy/mill/internal/forge/gitlab"
	"github.com/ivy/mill/internal/run"
	"github.com/ivy/mill/internal/sweep"
	"github.com/ivy/mill/internal/workspace"
	"github.com/ivy/mill/internal/workspace/machine"
)

// machineStartup is what a machine worker does before its first claim
// (run.Startup): docs/sandbox.md §Reconciliation's pass over the host,
// recorded in the ledger, then eviction — expiry, then the watermarks —
// over the lineages and images (§Eviction).
type machineStartup struct {
	reconciler *machine.Reconciler
	ledger     machine.Ledger
	config     machine.Config
	out        io.Writer
}

// workerStartup is the startup for the configured backend: the machine
// backend's pass, or nothing for the local one. The sweep's dagger client is
// the worker's own — the configured socket and project, the worker's agent
// ref — and never one the environment names.
func workerStartup(ctx context.Context, kind workspace.Kind, opts runOptions, project, workerRef string, out io.Writer, log *slog.Logger) (run.Startup, error) {
	if kind != workspace.KindMachine {
		return nil, nil
	}
	config := machineConfig(opts, run.Repository{}, nil)
	client := dagger.Dial(opts.daggerSocket, project, dagger.WithAgentRef(workerRef))
	externals := []machine.External{sweep.Dagger(client)}
	if opts.sandbox.scratchNamespace != "" {
		// The scratch namespace is on the project's own forge, which the
		// project names.
		p, err := client.GetProject(ctx)
		if err != nil {
			return nil, err
		}
		repo, err := forge.RepoFor(forge.Project{Slug: project, VCSURL: p.GetVcsUrl()})
		if err != nil {
			return nil, err
		}
		externals = append(externals, sweep.Forge(gitlab.NewScratch(repo.BaseURL, opts.sandbox.scratchNamespace,
			forge.Static(envOr(forgeUserEnv, defaultForgeUser), os.Getenv(forgeTokenEnv)))))
	}
	return &machineStartup{
		reconciler: machine.NewReconciler(machineDriver(), config, externals...),
		ledger:     run.NewLedger(storeClient(opts.socket), workerRef, log),
		config:     config,
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

// Evict runs eviction over the host and prints what it deleted, one line
// each.
func (s *machineStartup) Evict(ctx context.Context) error {
	report, err := machine.Evict(ctx, machineDriver(), cacheDriver(), s.config)
	for _, d := range report.Deleted {
		_, _ = fmt.Fprintf(s.out, "evict: %s %s, last used %s: %s\n", d.Kind, d.Name(), d.LastUsed.UTC().Format(time.RFC3339), d.Reason)
	}
	return err
}
