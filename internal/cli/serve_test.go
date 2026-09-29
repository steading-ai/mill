package cli_test

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/structpb"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/gen/mill/v1/millv1connect"
	"github.com/steading-ai/mill/internal/cli"
	"github.com/steading-ai/mill/internal/sdnotify"
	"github.com/steading-ai/mill/internal/testsupport"
	"github.com/steading-ai/mill/internal/workflow"
	"github.com/steading-ai/mill/internal/workspace/socket"
)

// notifier stands in for systemd: a datagram socket named by NOTIFY_SOCKET,
// whose lines the service sends its readiness on.
func notifier(dir string) chan string {
	GinkgoHelper()
	path := filepath.Join(dir, "notify.sock")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(conn.Close)
	GinkgoT().Setenv(sdnotify.Env, path)

	states := make(chan string, 8)
	go func() {
		defer GinkgoRecover()
		buf := make([]byte, 4096)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				close(states)
				return
			}
			for _, line := range strings.Split(strings.TrimSpace(string(buf[:n])), "\n") {
				states <- line
			}
		}
	}()
	return states
}

// unixClient dials the socket whatever the URL says.
func unixClient(socket string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
}

var _ = Describe("mill serve", Label(testsupport.DBLabel), func() {
	It("binds its socket, migrates, answers Status, and reports ready once the database has answered", func(ctx SpecContext) {
		db := pg.Database(ctx)
		databaseURL := db.Config().ConnString()

		// The database this spec hands the service has no schema: the
		// template it was cloned from was left empty on purpose.
		var tables int
		Expect(db.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'`).
			Scan(&tables)).To(Succeed())
		Expect(tables).To(BeZero())

		dir := GinkgoT().TempDir()
		socket := filepath.Join(dir, "o.sock")
		states := notifier(dir)

		var stdout, stderr bytes.Buffer
		root := cli.NewRootCommand(&stdout, &stderr)
		root.SetArgs([]string{"serve", "--socket", socket, "--database-url", databaseURL})

		serveCtx, stop := context.WithCancel(ctx)
		served := make(chan error, 1)
		go func() {
			defer GinkgoRecover()
			served <- root.ExecuteContext(serveCtx)
		}()
		DeferCleanup(func() {
			stop()
			Eventually(served).Should(Receive(BeNil()), stderr.String())
		})

		// Readiness is reported, and only once the database has answered:
		// the migrations it applied are in the database by the time the
		// line arrives.
		Eventually(states).Should(Receive(Equal(sdnotify.Ready)), stderr.String())
		Expect(db.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'`).
			Scan(&tables)).To(Succeed())
		Expect(tables).To(BeNumerically(">", 1), "the service migrated the database before reporting ready")

		info, err := os.Stat(socket)
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Mode() & os.ModeSocket).NotTo(BeZero())

		client := millv1connect.NewStoreServiceClient(unixClient(socket), "http://mill")
		listed, err := client.Status(ctx, connect.NewRequest(&millv1.StatusRequest{}))
		Expect(err).NotTo(HaveOccurred())
		Expect(listed.Msg.GetRuns()).To(BeEmpty(), "an empty ledger has no runs to report")
	})

	// This is the proof that `mill serve` itself, not just the handlers it
	// wraps, prices usage: a `--config` naming a rates file is what
	// internal/cli/serve.go reads before it ever calls server.New, and a
	// prior version of this change built the whole pricing path in
	// internal/server without ever wiring it in here, so mill serve
	// answered Cost with every USD unpriced.
	It("prices Cost's totals by the --config file's rates, unconditionally, without --work", func(ctx SpecContext) {
		db := pg.Database(ctx)
		databaseURL := db.Config().ConnString()

		dir := GinkgoT().TempDir()
		configPath := filepath.Join(dir, "config.yaml")
		Expect(os.WriteFile(configPath, []byte(`rates:
  claude-sonnet-4-5:
    input: 3
    output: 15
    cache_read: 0.3
    cache_write: 3.75
`), 0o644)).To(Succeed())

		sock := filepath.Join(dir, "o.sock")
		var stdout, stderr bytes.Buffer
		root := cli.NewRootCommand(&stdout, &stderr)
		root.SetArgs([]string{"serve", "--socket", sock, "--database-url", databaseURL, "--config", configPath})

		serveCtx, stop := context.WithCancel(ctx)
		served := make(chan error, 1)
		go func() {
			defer GinkgoRecover()
			served <- root.ExecuteContext(serveCtx)
		}()
		DeferCleanup(func() {
			stop()
			Eventually(served).Should(Receive(BeNil()), stderr.String())
		})
		Eventually(func() error { _, err := os.Stat(sock); return err }).Should(Succeed(), stderr.String())

		client := millv1connect.NewStoreServiceClient(unixClient(sock), "http://mill")

		stored, err := client.PutWorkflow(ctx, connect.NewRequest(&millv1.PutWorkflowRequest{
			Workflow: &millv1.Workflow{Name: "feature", Scope: millv1.Scope_SCOPE_ORG, Steps: []*millv1.Step{
				{Name: "implement", Kind: millv1.StepKind_STEP_KIND_SKILL, Evidence: millv1.EvidenceKind_EVIDENCE_KIND_COMMIT},
			}},
		}))
		Expect(err).NotTo(HaveOccurred())

		created, err := client.CreateRun(ctx, connect.NewRequest(&millv1.CreateRunRequest{
			LeaseId: "lease-1", NodeId: "node-mill#900", NodeRef: "mill#900",
			ContextId: "ctx-mill#900", WorkerRef: "worker-1", Branch: "mill/mill#900",
		}))
		Expect(err).NotTo(HaveOccurred())

		assess, err := client.StartStepRun(ctx, connect.NewRequest(&millv1.StartStepRunRequest{
			LeaseId: "lease-1", RunId: created.Msg.GetRun().GetId(), StepName: "assess",
			Kind: millv1.StepKind_STEP_KIND_SKILL, WorkerRef: "worker-1",
		}))
		Expect(err).NotTo(HaveOccurred())
		document, err := structpb.NewStruct(map[string]any{"workflow": "feature", "tier": "feature"})
		Expect(err).NotTo(HaveOccurred())
		_, err = client.CloseStepRun(ctx, connect.NewRequest(&millv1.CloseStepRunRequest{
			LeaseId: "lease-1", StepRunId: assess.Msg.GetStepRun().GetId(),
			Evidence: &millv1.Evidence{
				Kind:       millv1.EvidenceKind_EVIDENCE_KIND_STRUCTURED,
				Value:      &millv1.Evidence_Structured{Structured: document},
				VerifiedBy: "assessment",
			},
			WorkflowId: stored.Msg.GetWorkflow().GetId(),
			Tier:       millv1.Tier_TIER_FEATURE,
		}))
		Expect(err).NotTo(HaveOccurred())

		implement, err := client.StartStepRun(ctx, connect.NewRequest(&millv1.StartStepRunRequest{
			LeaseId: "lease-1", RunId: created.Msg.GetRun().GetId(), StepName: "implement",
			Kind: millv1.StepKind_STEP_KIND_SKILL, WorkerRef: "worker-1",
		}))
		Expect(err).NotTo(HaveOccurred())
		_, err = client.RecordUsage(ctx, connect.NewRequest(&millv1.RecordUsageRequest{
			LeaseId: "lease-1", StepRunId: implement.Msg.GetStepRun().GetId(),
			Usage: []*millv1.Usage{{Model: "claude-sonnet-4-5", InputTokens: 1_000_000, OutputTokens: 200_000}},
		}))
		Expect(err).NotTo(HaveOccurred())

		// 1,000,000 input tokens at $3/million plus 200,000 output tokens at
		// $15/million, hand-computed against the --config file above: $3 + $3.
		cost, err := client.Cost(ctx, connect.NewRequest(&millv1.CostRequest{}))
		Expect(err).NotTo(HaveOccurred())
		var got *millv1.ModelTotals
		for _, bucket := range cost.Msg.GetByModel() {
			if bucket.GetKey() == "claude-sonnet-4-5" {
				for _, t := range bucket.GetTotals() {
					if t.GetModel() == "claude-sonnet-4-5" {
						got = t
					}
				}
			}
		}
		Expect(got).NotTo(BeNil())
		Expect(got.GetPriced()).To(BeTrue())
		Expect(got.GetUsd()).To(BeNumerically("~", 6.0, 0.0001))
	})

	// A socket path over the kernel's limit fails on the bind as "invalid
	// argument", and by then the migrations have run for a service that
	// will not start. It is refused first, and the refusal says by how much
	// the path is too long.
	It("refuses a socket path the kernel cannot bind, before it migrates or binds", func(ctx SpecContext) {
		var stdout, stderr bytes.Buffer
		path := filepath.Join(GinkgoT().TempDir(), strings.Repeat("s", socket.MaxPathLen), "mill.sock")
		root := cli.NewRootCommand(&stdout, &stderr)
		// A database URL nothing serves: reaching it at all would be the
		// failure this refusal comes before.
		root.SetArgs([]string{"serve", "--socket", path,
			"--database-url", "postgres://nobody@127.0.0.1:1/none"})

		err := root.ExecuteContext(ctx)

		Expect(err).To(MatchError(ContainSubstring(path)))
		Expect(err).To(MatchError(ContainSubstring(strconv.Itoa(len(path)))))
		Expect(err).To(MatchError(ContainSubstring(strconv.Itoa(socket.MaxPathLen))))
		Expect(err).To(MatchError(ContainSubstring("--socket")))
		Expect(err).NotTo(MatchError(ContainSubstring("migrate")))
	})

	It("refuses to start without a database URL", func(ctx SpecContext) {
		GinkgoT().Setenv("DATABASE_URL", "")
		var stdout, stderr bytes.Buffer
		root := cli.NewRootCommand(&stdout, &stderr)
		root.SetArgs([]string{"serve", "--socket", filepath.Join(GinkgoT().TempDir(), "o.sock")})
		Expect(root.ExecuteContext(ctx)).To(MatchError(ContainSubstring("database URL is required")))
	})

	It("mounts WorkerService only when a worker runs in this process", func(ctx SpecContext) {
		m := serving(ctx)
		client := millv1connect.NewWorkerServiceClient(unixClient(m.socket), "http://mill")
		_, err := client.Probe(ctx, connect.NewRequest(&millv1.ProbeRequest{}))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnimplemented),
			"a bare `serve` has no worker to answer sidecar/signing/forge from")
	})

	It("answers WorkerService.Probe from this process's own worker once --work runs one", func(ctx SpecContext) {
		g := skiftNodeWith(ctx, "https://forge.test/mill/mill.git")
		databaseURL := pg.Database(ctx).Config().ConnString()
		socket := filepath.Join(GinkgoT().TempDir(), "mill.sock")
		config := filepath.Join(GinkgoT().TempDir(), "empty.yaml")
		Expect(os.WriteFile(config, []byte("{}\n"), 0o600)).To(Succeed())

		var stdout, stderr bytes.Buffer
		root := cli.NewRootCommand(&stdout, &stderr)
		root.SetArgs([]string{
			"serve", "--work",
			"--socket", socket,
			"--database-url", databaseURL,
			"--config", config,
			"--project", skiftProject,
			// A label no node carries: what is under test is WorkerService
			// answering beside the worker, not what the worker claims.
			"--label", "nothing-carries-this",
			"--skift-socket", g.service.Socket,
			"--workspace-dir", filepath.Join(GinkgoT().TempDir(), "workspaces"),
			"--log-dir", filepath.Join(GinkgoT().TempDir(), "logs"),
		})

		serveCtx, stop := context.WithCancel(ctx)
		served := make(chan error, 1)
		go func() {
			defer GinkgoRecover()
			served <- root.ExecuteContext(serveCtx)
		}()
		DeferCleanup(func() {
			stop()
			Eventually(served, time.Minute).Should(Receive(BeNil()), stderr.String())
		})
		Eventually(func() error {
			_, err := os.Stat(socket)
			return err
		}, time.Minute, 50*time.Millisecond).Should(Succeed(), stderr.String())

		client := millv1connect.NewWorkerServiceClient(unixClient(socket), "http://mill")
		var err error
		Eventually(func() error {
			_, err = client.Probe(ctx, connect.NewRequest(&millv1.ProbeRequest{}))
			return err
		}, time.Minute, 100*time.Millisecond).Should(Succeed(), stderr.String())
	})
})

var _ = Describe("mill serve --workflows-root", Label(testsupport.DBLabel), func() {
	It("pushes every workflow directory under the root on start", func(ctx SpecContext) {
		m := servingWith(ctx, "--workflows-root", workflowsRoot)

		listed := m.run(ctx, "workflow", "list")
		// mill-feature and mill-quick-fix are the mill project's own scoped
		// versions of feature and quick-fix (AGENTS.md's repo map), not
		// separate names, so what a distinct push from each of the five
		// directories shows up as is these three names across both scopes.
		for _, name := range []string{"quick-fix", "feature", "build-workflow"} {
			Expect(listed).To(ContainSubstring(name), "%s was pushed from the root with no manual push", name)
		}
		Expect(listed).To(ContainSubstring("project:mill"), "mill-feature and mill-quick-fix pushed their project-scoped versions")

		definition, _, err := workflow.Load(quickFix)
		Expect(err).NotTo(HaveOccurred())
		// mill-quick-fix also pushed as "quick-fix", scoped to project:mill, so
		// picking the row by name alone is ambiguous between the two; scoping
		// the listing to org is what selects the one Load above read.
		orgOnly := m.run(ctx, "workflow", "list", "--scope", "org")
		// workflow list prints the version's first 12 hex characters.
		Expect(rowFor(orgOnly, "quick-fix")).To(ContainSubstring(definition.GetVersion()[:12]),
			"the version listed is the one Load computes from the directory on disk")
	})

	It("starting again against the same directories stores nothing new", func(ctx SpecContext) {
		databaseURL := pg.Database(ctx).Config().ConnString()

		start := func() mill {
			sock := filepath.Join(GinkgoT().TempDir(), "mill.sock")
			var stdout, stderr bytes.Buffer
			root := cli.NewRootCommand(&stdout, &stderr)
			root.SetArgs([]string{"serve", "--socket", sock, "--database-url", databaseURL, "--workflows-root", workflowsRoot})
			serveCtx, stop := context.WithCancel(ctx)
			served := make(chan error, 1)
			go func() {
				defer GinkgoRecover()
				served <- root.ExecuteContext(serveCtx)
			}()
			DeferCleanup(func() {
				stop()
				Eventually(served).Should(Receive(BeNil()), stderr.String())
			})
			Eventually(func() error {
				_, err := os.Stat(sock)
				return err
			}, 60*time.Second, 50*time.Millisecond).Should(Succeed(), stderr.String())
			return mill{socket: sock, databaseURL: databaseURL}
		}

		first := start()
		beforeListed := first.run(ctx, "workflow", "list", "--all-versions")

		second := start()
		afterListed := second.run(ctx, "workflow", "list", "--all-versions")

		Expect(strings.Count(afterListed, "quick-fix")).To(Equal(strings.Count(beforeListed, "quick-fix")),
			"the restart pushed the same content, so no new version was stored")
		Expect(rowFor(afterListed, "quick-fix")).To(Equal(rowFor(beforeListed, "quick-fix")))
	})

	It("refuses to start, naming the directory, when one workflow under the root fails to load", func(ctx SpecContext) {
		root := GinkgoT().TempDir()

		definition, skills, err := workflow.Load(quickFix)
		Expect(err).NotTo(HaveOccurred())
		Expect(workflow.Write(filepath.Join(root, "good"), definition, skills)).To(Succeed())

		bad := filepath.Join(root, "bad")
		Expect(os.MkdirAll(bad, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(bad, workflow.DefinitionFile), []byte("not: [valid"), 0o600)).To(Succeed())

		databaseURL := pg.Database(ctx).Config().ConnString()
		sock := filepath.Join(GinkgoT().TempDir(), "mill.sock")
		var stdout, stderr bytes.Buffer
		cmd := cli.NewRootCommand(&stdout, &stderr)
		cmd.SetArgs([]string{"serve", "--socket", sock, "--database-url", databaseURL, "--workflows-root", root})

		err = cmd.ExecuteContext(ctx)
		Expect(err).To(MatchError(ContainSubstring(bad)))

		_, statErr := os.Stat(sock)
		Expect(statErr).To(HaveOccurred(), "a definition the loader refuses fails the start before the socket binds")
	})
})
