package cli_test

import (
	"context"
	"strings"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/testsupport"
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
