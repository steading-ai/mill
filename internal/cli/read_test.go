package cli_test

import (
	"context"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	harnessv1 "github.com/ivy/mill/gen/harness/v1"
	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/testsupport"
	"github.com/ivy/mill/internal/workflow"
)

var _ = Describe("The read tools", Label(testsupport.DBLabel), func() {
	var ctx context.Context

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
	})

	Describe("mill status", func() {
		It("renders the ledger's status query: the step each run is on, its attempt, and its cost", func() {
			r := openRun(ctx, "millcli#1", nodeID(1))
			r.closeGatherContext(ctx, "sha256:"+strings.Repeat("a", 64))
			r.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)

			out := r.cli(ctx, "status")

			row := rowFor(out, "millcli#1")
			Expect(row).NotTo(BeEmpty(), out)
			Expect(row).To(ContainSubstring("running"))
			Expect(row).To(ContainSubstring("implement"))
		})

		It("One bad workflow row does not stop the listing", func() {
			// Given a store with two workflows, one of which fails to decode
			m := serving(ctx)
			openRunOn(ctx, m, "millcli#1", nodeID(1))
			definition, _, err := workflow.Load(quickFix)
			Expect(err).NotTo(HaveOccurred())
			definition.Name = "legacy"
			stale := openRunFollowing(ctx, m, definition, "millcli#2", nodeID(2))
			db, err := pgxpool.New(ctx, m.databaseURL)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(db.Close)
			// The bytes a mill whose schema had a `model` field would have
			// written, which this one's decoder refuses.
			_, err = db.Exec(ctx, `UPDATE workflows SET definition = definition || '{"model": "opus"}'::jsonb WHERE id = $1`,
				stale.workflow.GetId())
			Expect(err).NotTo(HaveOccurred())

			// When mill status runs
			out, err := runCLI(ctx, m.socket, "status")
			Expect(err).NotTo(HaveOccurred(), out)

			// Then the good workflow is listed and a finding names the bad one
			Expect(rowFor(out, "millcli#1")).To(ContainSubstring("gather-context"), out)
			Expect(rowFor(out, "millcli#2")).NotTo(BeEmpty(), "the run on the bad row is still a live run")
			Expect(out).To(ContainSubstring("warning: workflow " + stale.workflow.GetId() + " does not decode"))
			Expect(out).To(ContainSubstring(`field "model"`))
			By("and the row is left exactly as it was")
			var stored string
			Expect(db.QueryRow(ctx, `SELECT definition::text FROM workflows WHERE id = $1`, stale.workflow.GetId()).
				Scan(&stored)).To(Succeed())
			Expect(stored).To(ContainSubstring(`"model"`))
		})

		It("takes a view, and refs is identifiers alone", func() {
			r := openRun(ctx, "millcli#2", nodeID(2))

			refs := r.cli(ctx, "status", "--view", "refs")
			Expect(strings.Fields(lines(refs)[0])).To(Equal([]string{"NODE", "RUN"}))
			Expect(rowFor(refs, "millcli#2")).To(ContainSubstring(r.run.GetId()))
			Expect(refs).NotTo(ContainSubstring("ELAPSED"))

			full := r.cli(ctx, "status", "--view", "full")
			Expect(full).To(ContainSubstring("WORKER"))
			Expect(full).To(ContainSubstring("LEASES"))
		})
	})

	Describe("mill inspect", func() {
		It("shows every attempt in order with the evidence value that closed it", func() {
			r := openRun(ctx, "millcli#3", nodeID(3))
			digest := "sha256:" + strings.Repeat("b", 64)
			r.closeGatherContext(ctx, digest)
			r.fail(ctx, "implement", 1)

			out := r.cli(ctx, "inspect", "millcli#3")

			Expect(out).To(ContainSubstring("millcli#3 run " + r.run.GetId()))
			Expect(out).To(ContainSubstring("quick-fix@"))
			// Each step run's evidence value, which is the claim the mill
			// verified: the artifact digest itself, not a description of it.
			Expect(rowFor(out, "gather-context")).To(ContainSubstring(digest))
			Expect(rowFor(out, "gather-context")).To(ContainSubstring("closed"))
			Expect(rowFor(out, "implement")).To(ContainSubstring("failed"))
			Expect(rowFor(out, "select")).To(ContainSubstring("workflow=quick-fix"))
		})

		// Scenario: Inspect shows it
		//   Given a finished machine-backed run
		//   When the operator runs mill inspect
		//   Then the machine, digest, veth and lineages are printed
		It("Inspect shows it", func() {
			By("given a finished machine-backed run: its workspace recorded at creation and finished with a promotion")
			r := openRun(ctx, "millcli#80", nodeID(80))
			_, err := r.store.RecordWorkspace(ctx, connect.NewRequest(&millv1.RecordWorkspaceRequest{
				LeaseId: r.lease, RunId: r.run.GetId(), Kind: millv1.WorkspaceKind_WORKSPACE_KIND_MACHINE,
				WorkerRef: "spec@host", Ref: "main",
				Sandbox: &millv1.Sandbox{
					Machine: "mill-01jrun", Unit: "mill-machine@01jrun.service",
					ImageDigest: "sha256:3c9a43", Veth: "ve-mill-01jru",
					Lineages: []*millv1.SandboxLineage{
						{Scope: "global", Name: "mise", Restored: 3},
						{Scope: "repo/abc123", Name: "go-build", Restored: 0},
					},
				},
			}))
			Expect(err).NotTo(HaveOccurred())
			_, err = r.store.Done(ctx, connect.NewRequest(&millv1.DoneRequest{LeaseId: r.lease, RunId: r.run.GetId()}))
			Expect(err).NotTo(HaveOccurred())
			four := int64(4)
			_, err = r.store.FinishWorkspace(ctx, connect.NewRequest(&millv1.FinishWorkspaceRequest{
				LeaseId: r.lease, RunId: r.run.GetId(),
				Promoted: []*millv1.SandboxLineage{{Scope: "global", Name: "mise", Restored: 3, Promoted: &four}},
			}))
			Expect(err).NotTo(HaveOccurred())

			By("when the operator runs mill inspect")
			out := r.cli(ctx, "inspect", "millcli#80")

			By("then the machine, digest, veth and lineages are printed")
			Expect(out).To(ContainSubstring("machine   mill-01jrun (mill-machine@01jrun.service)"))
			Expect(out).To(ContainSubstring("image     sha256:3c9a43"))
			Expect(out).To(ContainSubstring("veth      ve-mill-01jru"))
			Expect(out).To(ContainSubstring("cache     global/mise restored 3 promoted 4"))
			Expect(out).To(ContainSubstring("cache     repo/abc123/go-build restored 0 promoted -"))

			By("and status names the machine")
			status := r.cli(ctx, "status", "--status", "done", "--view", "full")
			Expect(rowFor(status, "millcli#80")).To(ContainSubstring("mill-01jrun"))
			Expect(rowFor(status, "millcli#80")).To(ContainSubstring("global/mise@3->4"))
		})

		It("prints no machine for a local run", func() {
			r := openRun(ctx, "millcli#81", nodeID(81))
			_, err := r.store.RecordWorkspace(ctx, connect.NewRequest(&millv1.RecordWorkspaceRequest{
				LeaseId: r.lease, RunId: r.run.GetId(), Kind: millv1.WorkspaceKind_WORKSPACE_KIND_LOCAL,
				WorkerRef: "spec@host", Ref: "main",
			}))
			Expect(err).NotTo(HaveOccurred())

			out := r.cli(ctx, "inspect", "millcli#81")

			Expect(out).To(ContainSubstring("branch    mill/millcli-81"))
			Expect(out).NotTo(ContainSubstring("machine"))
			Expect(out).NotTo(ContainSubstring("veth"))
			Expect(out).NotTo(ContainSubstring("cache "))
		})

		It("shows a verdict's decision and the findings that blocked it, inline", func() {
			r := openRun(ctx, "millcli#10", nodeID(10))
			sr := r.start(ctx, "review", millv1.StepKind_STEP_KIND_SKILL)
			r.close(ctx, sr, &millv1.CloseStepRunRequest{
				Evidence: &millv1.Evidence{
					Kind: millv1.EvidenceKind_EVIDENCE_KIND_VERDICT,
					Value: &millv1.Evidence_Verdict{Verdict: &millv1.Verdict{
						Decision: millv1.VerdictDecision_VERDICT_DECISION_APPROVE,
						Findings: []*millv1.Finding{
							{
								Criterion: "AC-2",
								Severity:  millv1.FindingSeverity_FINDING_SEVERITY_BLOCKING,
								Summary:   "the scenario has no spec",
							},
							{
								Criterion: "AC-3",
								Severity:  millv1.FindingSeverity_FINDING_SEVERITY_NOTE,
								Summary:   "the comment could say more",
							},
						},
					}},
					VerifiedBy: "mill: the verdict parsed",
				},
			})

			out := r.cli(ctx, "inspect", "millcli#10")

			row := rowFor(out, "review")
			Expect(row).To(ContainSubstring("approve"))
			Expect(row).To(ContainSubstring("AC-2: the scenario has no spec"))
			Expect(row).NotTo(ContainSubstring("the comment could say more"),
				"a note decided nothing and is read off the node's artifact")
		})

		It("shows what the environment mode gave a session, when there is something to show", func() {
			r := openRun(ctx, "millcli#8", nodeID(8))
			sr := r.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)
			_, err := r.store.RecordStepEnvironment(ctx, connect.NewRequest(&millv1.RecordStepEnvironmentRequest{
				LeaseId:        r.lease,
				StepRunId:      sr.GetId(),
				EnvPassthrough: []string{"GOFLAGS"},
				EnvStripped:    []string{"ANTHROPIC_API_KEY", "DATABASE_URL"},
			}))
			Expect(err).NotTo(HaveOccurred())

			out := r.cli(ctx, "inspect", "millcli#8")

			Expect(out).To(ContainSubstring("ENV PASSTHROUGH"))
			Expect(out).To(ContainSubstring("GOFLAGS"))
			Expect(out).To(ContainSubstring("ANTHROPIC_API_KEY DATABASE_URL"))
		})

		It("shows a stalled attempt with the heartbeats that led to it", func() {
			r := openRun(ctx, "millcli#11", nodeID(11))
			sr := r.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)
			for strike := int32(1); strike <= 2; strike++ {
				_, err := r.store.RecordHeartbeat(ctx, connect.NewRequest(&millv1.RecordHeartbeatRequest{
					LeaseId:        r.lease,
					RunId:          r.run.GetId(),
					StepRunId:      sr.GetId(),
					Strike:         strike,
					Strikes:        3,
					WaitingSeconds: int64(strike) * 180,
					Tasks:          []string{"Bash: make test"},
				}))
				Expect(err).NotTo(HaveOccurred())
			}
			_, err := r.store.FailStepRun(ctx, connect.NewRequest(&millv1.FailStepRunRequest{
				LeaseId: r.lease, StepRunId: sr.GetId(),
				Exit: &harnessv1.ProcessExit{Status: 1, Reason: harnessv1.ExitReason_EXIT_REASON_STALLED},
			}))
			Expect(err).NotTo(HaveOccurred())

			out := r.cli(ctx, "inspect", "millcli#11")

			By("then the attempt's exit says the breaker stopped it")
			Expect(rowFor(out, "implement")).To(ContainSubstring("1:stalled"))

			By("and the heartbeats it took to get there are under the steps")
			Expect(out).To(ContainSubstring("HEARTBEATS"))
			Expect(lines(out)).To(ContainElement(MatchRegexp(`^implement\s+1\s+2$`)))
		})

		It("says nothing about the environment when no step carved anything out", func() {
			r := openRun(ctx, "millcli#9", nodeID(9))
			r.closeGatherContext(ctx, "sha256:"+strings.Repeat("d", 64))

			out := r.cli(ctx, "inspect", "millcli#9")

			Expect(out).NotTo(ContainSubstring("ENV PASSTHROUGH"))
		})
	})

	Describe("mill history", func() {
		It("shows the run's audit trail oldest first", func() {
			r := openRun(ctx, "millcli#4", nodeID(4))
			r.closeGatherContext(ctx, "sha256:"+strings.Repeat("c", 64))

			out := r.cli(ctx, "history", "millcli#4")

			Expect(out).To(ContainSubstring("create_run"))
			Expect(out).To(ContainSubstring("close_step_run"))
			Expect(strings.Index(out, "create_run")).To(BeNumerically("<", strings.Index(out, "close_step_run")))
			Expect(out).To(ContainSubstring("evidence_kind=artifact"))
		})
	})

	Describe("mill cost", func() {
		It("rolls usage up by run, workflow, step and model", func() {
			r := openRun(ctx, "millcli#5", nodeID(5))
			sr := r.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)
			_, err := r.store.RecordUsage(ctx, connect.NewRequest(&millv1.RecordUsageRequest{
				LeaseId:   r.lease,
				StepRunId: sr.GetId(),
				Usage: []*millv1.Usage{{
					StepRunId: sr.GetId(), Model: "claude-sonnet",
					InputTokens: 100, OutputTokens: 20,
				}},
			}))
			Expect(err).NotTo(HaveOccurred())

			out := r.cli(ctx, "cost")

			Expect(rowFor(out, "millcli#5")).To(ContainSubstring("100"))
			Expect(rowFor(out, "implement")).To(ContainSubstring("100"))
			Expect(rowFor(out, "claude-sonnet")).To(ContainSubstring("20"))
			Expect(out).To(ContainSubstring("quick-fix@"))
		})

		// One assistant turn bills an auxiliary model alongside the one the
		// step asked for, so the MODEL table is where "which model billed"
		// is answered. It is filled from the rows the STEP table renders,
		// so the two cannot disagree and it cannot come back empty under a
		// STEP table that has rows.
		It("prints one MODEL row per model that has usage, summing to the STEP table's totals", func() {
			r := openRun(ctx, "millcli#26", nodeID(26))
			// One attempt at a time, because the ledger allows one: the
			// first step's usage is recorded and the step closed before the
			// second opens.
			gather := r.start(ctx, "gather-context", millv1.StepKind_STEP_KIND_SKILL)
			record(ctx, r, gather, map[string]int64{"claude-opus-5": 300, "claude-haiku-4-5": 7})
			r.close(ctx, gather, &millv1.CloseStepRunRequest{
				Evidence: &millv1.Evidence{
					Kind:       millv1.EvidenceKind_EVIDENCE_KIND_ARTIFACT,
					Value:      &millv1.Evidence_Artifact{Artifact: "sha256:" + strings.Repeat("e", 64)},
					VerifiedBy: "spec",
				},
			})
			implement := r.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)
			record(ctx, r, implement, map[string]int64{"claude-opus-5": 500, "claude-sonnet-4-5": 90})

			out := r.cli(ctx, "cost")

			// One row per model, and the input column of each is the sum of
			// what the steps billed it.
			models := section(out, "MODEL")
			Expect(lines(models)).To(HaveLen(3), out)
			Expect(rowFor(models, "claude-opus-5")).To(ContainSubstring("800"))
			Expect(rowFor(models, "claude-haiku-4-5")).To(ContainSubstring("7"))
			Expect(rowFor(models, "claude-sonnet-4-5")).To(ContainSubstring("90"))

			// And the two tables agree: 300+7+500+90 either way.
			Expect(inputTotal(section(out, "MODEL"))).To(Equal(inputTotal(section(out, "STEP"))))
			Expect(inputTotal(section(out, "MODEL"))).To(Equal(int64(897)))
		})
	})

	Describe("the gateway's count beside the adapter's", func() {
		// The credential gateway sees every response and counts what the
		// model billed from outside the session; the adapter reports what
		// the SDK told it. Both are ledger rows, each saying who counted
		// it, and `mill cost` lists them beside each other
		// (docs/sandbox.md §Credentials).
		It("Usage is recorded from outside the session", func() {
			By("given step 3 of a run, whose session the adapter reported on")
			r := openRun(ctx, "millcli#57", nodeID(57))
			sr := r.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)
			_, err := r.store.RecordUsage(ctx, connect.NewRequest(&millv1.RecordUsageRequest{
				LeaseId: r.lease, StepRunId: sr.GetId(),
				Usage: []*millv1.Usage{{
					StepRunId: sr.GetId(), Model: "claude-sonnet-4-5", Source: millv1.UsageSource_USAGE_SOURCE_SDK,
					InputTokens: 100, OutputTokens: 20, CacheReadTokens: 1000,
				}},
			}))
			Expect(err).NotTo(HaveOccurred())

			By("when a response passes through the gateway for step 3 with model M")
			_, err = r.store.RecordUsage(ctx, connect.NewRequest(&millv1.RecordUsageRequest{
				LeaseId: r.lease, StepRunId: sr.GetId(),
				Usage: []*millv1.Usage{{
					StepRunId: sr.GetId(), Model: "claude-sonnet-4-5-20250929", Source: millv1.UsageSource_USAGE_SOURCE_GATEWAY,
					InputTokens: 102, OutputTokens: 20, CacheReadTokens: 1000,
				}},
			}))
			Expect(err).NotTo(HaveOccurred())

			By("then the ledger holds a usage row for step 3 and M from the gateway, distinct from the SDK-reported row")
			detail, err := r.store.Inspect(ctx, connect.NewRequest(&millv1.InspectRequest{Id: r.run.GetId()}))
			Expect(err).NotTo(HaveOccurred())
			var step *millv1.StepRunDetail
			for _, d := range detail.Msg.GetSteps() {
				if d.GetStepRun().GetId() == sr.GetId() {
					step = d
				}
			}
			Expect(step).NotTo(BeNil())
			Expect(step.GetTotals()).To(HaveLen(1), "the adapter's row is the attempt's cost")
			Expect(step.GetTotals()[0].GetModel()).To(Equal("claude-sonnet-4-5"))
			Expect(step.GetTotals()[0].GetInputTokens()).To(Equal(int64(100)))
			Expect(step.GetGatewayTotals()).To(HaveLen(1), "the gateway's row sits beside it")
			Expect(step.GetGatewayTotals()[0].GetModel()).To(Equal("claude-sonnet-4-5-20250929"))
			Expect(step.GetGatewayTotals()[0].GetSource()).To(Equal(millv1.UsageSource_USAGE_SOURCE_GATEWAY))
			Expect(step.GetGatewayTotals()[0].GetInputTokens()).To(Equal(int64(102)))

			By("and mill cost lists it beside the SDK's row for the same turn")
			out := r.cli(ctx, "cost")
			steps := section(out, "STEP")
			Expect(lines(steps)).To(HaveLen(2), out)
			Expect(lines(steps)[0]).To(MatchRegexp(`implement\s+sdk\s+100\s+20\s+1000`))
			Expect(lines(steps)[1]).To(MatchRegexp(`implement\s+gateway\s+102\s+20\s+1000`))
			models := section(out, "MODEL")
			Expect(rowFor(models, "claude-sonnet-4-5")).To(ContainSubstring("sdk"))
			Expect(rowFor(models, "claude-sonnet-4-5-20250929")).To(ContainSubstring("gateway"))

			By("and status counts the adapter's rows alone")
			status := r.cli(ctx, "status")
			Expect(rowFor(status, "millcli#57")).To(ContainSubstring("100/20"))
		})
	})

	Describe("mill workflow show", func() {
		It("prints the definition's steps, what closes each, and what a failure does", func() {
			r := openRun(ctx, "millcli#6", nodeID(6))

			out := r.cli(ctx, "workflow", "show", "quick-fix")

			Expect(rowFor(out, "implement")).To(ContainSubstring("artifact"))
			Expect(rowFor(out, "implement")).To(ContainSubstring("read,write,exec"))
			Expect(rowFor(out, "clean-tree")).To(ContainSubstring("revise:implement"))
		})

		It("prints each step's harness options as the document they are stored as", func() {
			r := openRun(ctx, "millcli#7", nodeID(7))

			out := r.cli(ctx, "workflow", "show", "quick-fix")

			Expect(out).To(ContainSubstring("OPTIONS"))
			Expect(out).To(ContainSubstring(
				`{"effort":"medium","model":"sonnet","tools":{"allow":["Glob","Grep","Read"]}}`))
		})
	})
})

// record puts one attempt's usage in the ledger, one entry per model, as a
// session's terminal record does.
func record(ctx context.Context, r *runs, sr *millv1.StepRun, byModel map[string]int64) {
	GinkgoHelper()
	usage := make([]*millv1.Usage, 0, len(byModel))
	for model, input := range byModel {
		usage = append(usage, &millv1.Usage{StepRunId: sr.GetId(), Model: model, InputTokens: input})
	}
	_, err := r.store.RecordUsage(ctx, connect.NewRequest(&millv1.RecordUsageRequest{
		LeaseId: r.lease, StepRunId: sr.GetId(), Usage: usage,
	}))
	Expect(err).NotTo(HaveOccurred())
}

// section is one of `mill cost`'s four tables: the rows under the header
// that names it, up to the blank line that ends the table. The header itself
// is dropped, so what comes back is what the section reported.
func section(output, title string) string {
	var rows []string
	inside := false
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		switch {
		case len(fields) == 0:
			inside = false
		case !inside && fields[0] == title:
			inside = true
		case inside:
			rows = append(rows, line)
		}
	}
	return strings.Join(rows, "\n")
}

// inputTotal sums a cost section's INPUT column, which is what "the two
// tables agree" means.
func inputTotal(rows string) int64 {
	GinkgoHelper()
	var total int64
	for _, line := range lines(rows) {
		fields := strings.Fields(line)
		Expect(len(fields)).To(BeNumerically(">=", 3), line)
		count, err := strconv.ParseInt(fields[2], 10, 64)
		Expect(err).NotTo(HaveOccurred(), line)
		total += count
	}
	return total
}
