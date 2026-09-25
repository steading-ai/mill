package scripted_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/mill/internal/harness"
	"github.com/steading-ai/mill/internal/harness/scripted"
)

var _ = Describe("The scripted adapter", Label("unit"), func() {
	var adapter *scripted.Adapter

	BeforeEach(func() {
		adapter = scripted.NewAdapter("testdata")
	})

	sessionFor := func(id, script string) harness.Session {
		return harness.Session{ID: id, Options: map[string]any{"script": script}}
	}

	It("gives each session its own cursor into its own script", func(ctx context.Context) {
		implement := sessionFor("implement", "session-a.yaml")
		review := sessionFor("review", "session-b.yaml")

		turn, err := adapter.Start(ctx, implement, harness.Prompt{}, nil)
		Expect(err).NotTo(HaveOccurred())
		terminal, err := turn.Wait(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(terminal.FinalMessage).To(Equal("a-1"))

		turn, err = adapter.Start(ctx, review, harness.Prompt{}, nil)
		Expect(err).NotTo(HaveOccurred())
		terminal, err = turn.Wait(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(terminal.FinalMessage).To(Equal("b-1"), "review's cursor must not have been advanced by implement's Start")

		turn, err = adapter.Resume(ctx, implement, harness.Prompt{}, nil)
		Expect(err).NotTo(HaveOccurred())
		terminal, err = turn.Wait(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(terminal.FinalMessage).To(Equal("a-2"), "implement resumes at its own next turn, unaffected by review's Start")

		turn, err = adapter.Resume(ctx, implement, harness.Prompt{}, nil)
		Expect(err).NotTo(HaveOccurred())
		terminal, err = turn.Wait(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(terminal.FinalMessage).To(Equal("a-2"), "a session run past its script's end clamps to the last turn")
	})

	It("refuses a session with no script option", func(ctx context.Context) {
		_, err := adapter.Start(ctx, harness.Session{ID: "no-script"}, harness.Prompt{}, nil)
		Expect(err).To(MatchError(ContainSubstring("script")))
	})

	It("Cancel is a no-op that never errors", func(ctx context.Context) {
		Expect(adapter.Cancel(ctx, sessionFor("implement", "session-a.yaml"))).To(Succeed())
	})
})
