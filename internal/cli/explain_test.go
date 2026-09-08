package cli_test

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/testsupport"
)

var _ = Describe("mill explain", Label(testsupport.DBLabel), func() {
	var ctx context.Context

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
	})

	It("on a running run names the step, its attempt's status and exit, what would close it, and the attempts used", func() {
		r := openRun(ctx, "millcli#10", nodeID(10))
		r.closeGatherContext(ctx, "sha256:"+strings.Repeat("a", 64))
		// Two failed attempts, so "attempts used" is a number worth
		// printing rather than always one.
		r.fail(ctx, "implement", 2)
		r.fail(ctx, "implement", 2)
		r.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)

		out := r.cli(ctx, "explain", "millcli#10")

		Expect(out).To(ContainSubstring("millcli#10 run " + r.run.GetId() + " is running"))
		Expect(out).To(ContainSubstring("step      implement (skill) running"))
		Expect(out).To(ContainSubstring("evidence  artifact would close it"))
		Expect(out).To(ContainSubstring("attempts  3 of 3 retries"))
	})

	It("prints the exit of an attempt that ended badly", func() {
		r := openRun(ctx, "millcli#11", nodeID(11))
		r.fail(ctx, "gather-context", 7)

		out := r.cli(ctx, "explain", "millcli#11")

		Expect(out).To(ContainSubstring("step      gather-context (skill) failed"))
		Expect(out).To(ContainSubstring("exit      7"))
	})

	It("on a waiting run prints the gate node's reference and the gate's title", func() {
		g := daggerNode(ctx)
		r := openRun(ctx, g.nodeRef, g.nodeID)
		r.closeGatherContext(ctx, "sha256:"+strings.Repeat("a", 64))
		sr := r.start(ctx, "merge", millv1.StepKind_STEP_KIND_GATE)
		g.fileGate(ctx, r.run.GetId(), "merge", "Merge the pull request for "+g.nodeRef+"?")
		r.wait(ctx, g.gateID, sr)

		out := r.cli(ctx, "explain", g.nodeRef,
			"--dagger-socket", g.service.Socket, "--project", daggerProject)

		Expect(out).To(ContainSubstring("is waiting"))
		Expect(out).To(ContainSubstring("gate      " + g.gateRef))
		Expect(out).To(ContainSubstring("ask       " + g.title))
		Expect(out).To(ContainSubstring("waiting   a human to answer gate " + g.gateRef))
	})
})
