package scripted_test

import (
	"context"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	harnessv1 "github.com/steading-ai/mill/gen/harness/v1"
	"github.com/steading-ai/mill/internal/harness"
	"github.com/steading-ai/mill/internal/harness/scripted"
)

// timestamped collects each event the adapter sends, with when it arrived,
// so a spec can measure the gap between two sends rather than trusting
// the sink was called at all.
type timestamped struct {
	mu   sync.Mutex
	seen []struct {
		event *harnessv1.Event
		at    time.Time
	}
}

func (t *timestamped) sink() harness.Sink {
	return func(event *harnessv1.Event) {
		t.mu.Lock()
		defer t.mu.Unlock()
		t.seen = append(t.seen, struct {
			event *harnessv1.Event
			at    time.Time
		}{event, time.Now()})
	}
}

func (t *timestamped) at(i int) time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.seen[i].at
}

func (t *timestamped) len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.seen)
}

func (t *timestamped) event(i int) *harnessv1.Event {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.seen[i].event
}

var _ = Describe("The scripted adapter's after", Label("unit"), func() {
	var adapter *scripted.Adapter

	BeforeEach(func() {
		adapter = scripted.NewAdapter("testdata")
	})

	It("emits an event no earlier than its after names, measured from the one before it", func(ctx context.Context) {
		log := &timestamped{}
		session := harness.Session{ID: "implement", Options: map[string]any{"script": "after.yaml"}, Events: log.sink()}

		turn, err := adapter.Start(ctx, session, harness.Prompt{}, nil)
		Expect(err).NotTo(HaveOccurred())
		terminal, err := turn.Wait(ctx)

		Expect(err).NotTo(HaveOccurred())
		Expect(terminal.FinalMessage).To(Equal("done"))
		Expect(log.len()).To(Equal(2))
		Expect(log.at(1).Sub(log.at(0))).To(BeNumerically(">=", 300*time.Millisecond),
			"the second event's after must be waited out before it is sent, counted from the first")
	})

	It("ends the turn with the context's error when the context is cancelled during a long wait", func() {
		startCtx, cancel := context.WithCancel(context.Background())
		session := harness.Session{ID: "implement", Options: map[string]any{"script": "after.yaml"}}

		// after.yaml's second turn is the one paused on after: 10m; reach it
		// with one Start (turn 1, instant) then one Resume (turn 2, paused).
		turn, err := adapter.Start(startCtx, session, harness.Prompt{}, nil)
		Expect(err).NotTo(HaveOccurred())
		_, err = turn.Wait(context.Background())
		Expect(err).NotTo(HaveOccurred())

		turn, err = adapter.Resume(startCtx, session, harness.Prompt{}, nil)
		Expect(err).NotTo(HaveOccurred())
		cancel()

		waitCtx, waitCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer waitCancel()
		terminal, err := turn.Wait(waitCtx)

		Expect(err).To(MatchError(context.Canceled),
			"the turn must end on startCtx's own cancellation well inside the 10m wait, not on waitCtx's unrelated timeout")
		Expect(terminal).To(BeNil())
	})

	It("behaves exactly as before when a script names no after", func(ctx context.Context) {
		session := harness.Session{ID: "implement", Options: map[string]any{"script": "session-a.yaml"}}

		started := time.Now()
		turn, err := adapter.Start(ctx, session, harness.Prompt{}, nil)
		Expect(err).NotTo(HaveOccurred())
		terminal, err := turn.Wait(ctx)

		Expect(err).NotTo(HaveOccurred())
		Expect(terminal.FinalMessage).To(Equal("a-1"))
		Expect(time.Since(started)).To(BeNumerically("<", time.Second),
			"a script with no after must not wait on anything")
	})

	It("keeps a delay on the event the script named even when a dispatch ahead of it shifts the replay", func(ctx context.Context) {
		log := &timestamped{}
		session := harness.Session{
			ID:      "implement",
			Options: map[string]any{"script": "dispatch-after.yaml"},
			Events:  log.sink(),
			// No real workspace is dialled: dispatch.yaml's dispatch fails fast
			// against a socket path nothing is listening on, which is fine —
			// this spec is about where the delay lands, not about the dispatch
			// itself succeeding (dispatch_test.go already proves that against a
			// real workspace).
			Workspace: harness.Workspace{Socket: "testdata/no-such-socket", Token: "unused"},
		}

		turn, err := adapter.Start(ctx, session, harness.Prompt{Text: dispatchPrompt}, nil)
		Expect(err).NotTo(HaveOccurred())
		terminal, err := turn.Wait(ctx)

		Expect(err).NotTo(HaveOccurred())
		Expect(terminal.FinalMessage).To(Equal("done"))
		Expect(log.len()).To(Equal(3), "the dispatching tool_call, its synthetic tool_result, then the delayed text event")
		Expect(log.event(2).GetText().GetText()).To(Equal("after the dispatch's synthetic tool_result"))
		Expect(log.at(2).Sub(log.at(1))).To(BeNumerically(">=", 200*time.Millisecond),
			"the delay named on the text event must land on the text event's own send, not the synthetic tool_result ahead of it")
	})
})
