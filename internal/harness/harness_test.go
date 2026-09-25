package harness_test

import (
	"context"
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/structpb"

	harnessv1 "github.com/steading-ai/mill/gen/harness/v1"
	"github.com/steading-ai/mill/internal/harness"
	"github.com/steading-ai/mill/internal/harness/fake"
)

var _ = Describe("The harness interface", Label("unit"), func() {
	Describe("the terminal record", func() {
		It("carries every model the harness billed, in the ledger's shape", func() {
			wire := &harnessv1.Terminal{
				RequestId:    "mill-01",
				FinalMessage: "done",
				Usage: &harnessv1.Usage{ByModel: map[string]*harnessv1.TokenCounts{
					"claude-opus-5":             {Input: 4, Output: 150, CacheRead: 25513, CacheWrite: 25789},
					"claude-haiku-4-5-20251001": {Input: 3870, Output: 76},
				}},
				Exit: &harnessv1.ProcessExit{Status: 0},
			}

			terminal := harness.TerminalFrom(wire)

			Expect(terminal.RequestID).To(Equal("mill-01"))
			Expect(terminal.Usage.Models()).To(Equal([]string{"claude-haiku-4-5-20251001", "claude-opus-5"}))
			Expect(terminal.Usage["claude-opus-5"]).To(Equal(harness.TokenCounts{
				Input: 4, Output: 150, CacheRead: 25513, CacheWrite: 25789,
			}))
			Expect(terminal.Exit.NeverRan()).To(BeFalse())
		})

		It("keeps a session that never ran apart from one that ran and failed", func() {
			never := harness.TerminalFrom(&harnessv1.Terminal{Exit: &harnessv1.ProcessExit{
				Status: -1, Reason: harnessv1.ExitReason_EXIT_REASON_TIMEOUT,
			}})
			ran := harness.TerminalFrom(&harnessv1.Terminal{Exit: &harnessv1.ProcessExit{Status: 1}})

			Expect(never.Exit.NeverRan()).To(BeTrue())
			Expect(never.Exit.Reason).To(Equal(harness.ReasonTimeout))
			Expect(ran.Exit.NeverRan()).To(BeFalse())
			Expect(ran.Exit.Reason).To(Equal(harness.ReasonNone))
		})

		It("reads structured output as ordinary values", func() {
			output, err := structpb.NewStruct(map[string]any{"decision": "approve"})
			Expect(err).NotTo(HaveOccurred())

			terminal := harness.TerminalFrom(&harnessv1.Terminal{StructuredOutput: output})

			Expect(terminal.StructuredOutput).To(Equal(map[string]any{"decision": "approve"}))
		})

		It("round-trips every exit reason through the wire's spelling", func() {
			for _, reason := range []harness.ExitReason{
				harness.ReasonNone, harness.ReasonRefused, harness.ReasonPolicy,
				harness.ReasonTimeout, harness.ReasonCancelled, harness.ReasonStalled, harness.ReasonUpstream,
			} {
				Expect(harness.ReasonFrom(reason.Proto())).To(Equal(reason))
			}
		})

		It("round-trips usage through the wire's spelling", func() {
			usage := harness.Usage{"claude-opus-5": {Input: 1, Output: 2, CacheRead: 3, CacheWrite: 4}}

			Expect(harness.UsageFrom(usage.Proto())).To(Equal(usage))
			Expect(harness.Usage(nil).Proto()).To(BeNil())
		})
	})

	Describe("a turn", func() {
		It("ends exactly once, so a cancellation racing a terminal line cannot rewrite it", func(ctx context.Context) {
			turn := harness.NewTurn()
			turn.Finish(&harness.Terminal{FinalMessage: "first"}, nil)
			turn.Finish(&harness.Terminal{FinalMessage: "second"}, nil)

			terminal, err := turn.Wait(ctx)

			Expect(err).NotTo(HaveOccurred())
			Expect(terminal.FinalMessage).To(Equal("first"))
			Expect(turn.Done()).To(BeClosed())
		})

		It("returns the caller's context error rather than a terminal record it does not have", func() {
			turn := harness.NewTurn()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
			defer cancel()

			terminal, err := turn.Wait(ctx)

			Expect(err).To(MatchError(context.DeadlineExceeded))
			Expect(terminal).To(BeNil())
		})
	})

	Describe("the registry", func() {
		It("maps a name to an implementation", func() {
			registry := harness.NewRegistry()
			replay := fake.New()

			Expect(registry.Register(fake.Name, replay)).To(Succeed())

			found, err := registry.Lookup(fake.Name)
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeIdenticalTo(replay))
			Expect(registry.Names()).To(Equal([]string{fake.Name}))
		})

		It("refuses a name twice and an implementation that is not there", func() {
			registry := harness.NewRegistry()
			Expect(registry.Register(fake.Name, fake.New())).To(Succeed())

			Expect(registry.Register(fake.Name, fake.New())).To(MatchError(ContainSubstring("already registered")))
			Expect(registry.Register("", fake.New())).To(MatchError(ContainSubstring("needs a name")))
			Expect(registry.Register("nil", nil)).To(MatchError(ContainSubstring("no implementation")))

			_, err := registry.Lookup("codex")
			Expect(errors.Is(err, harness.ErrUnknownHarness)).To(BeTrue())
		})
	})
})
