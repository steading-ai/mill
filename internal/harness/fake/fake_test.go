package fake_test

import (
	"context"
	"errors"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	harnessv1 "github.com/steading-ai/mill/gen/harness/v1"
	"github.com/steading-ai/mill/internal/harness"
	"github.com/steading-ai/mill/internal/harness/fake"
)

// billed is the per-model shape a real turn reports: the model the step asked
// for and the auxiliary one it also billed.
var billed = harness.Usage{
	"claude-opus-5":             {Input: 4, Output: 150, CacheRead: 25513, CacheWrite: 25789},
	"claude-haiku-4-5-20251001": {Input: 3870, Output: 76},
}

// recorder collects a turn's events as the worker's step log would.
type recorder struct {
	mu     sync.Mutex
	events []*harnessv1.Event
}

func (r *recorder) sink() harness.Sink {
	return func(event *harnessv1.Event) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.events = append(r.events, event)
	}
}

func (r *recorder) collected() []*harnessv1.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*harnessv1.Event{}, r.events...)
}

var _ = Describe("The fake harness", Label("unit"), func() {
	var (
		log     *recorder
		session harness.Session
	)

	BeforeEach(func() {
		log = &recorder{}
		session = harness.Session{
			ID:      "01JB-implement",
			Options: map[string]any{"model": "claude-opus-5"},
			Events:  log.sink(),
		}
	})

	It("records the options document it was handed, whole", func(ctx context.Context) {
		By("given a step whose options carry a key the adapter does not know")
		document := map[string]any{
			"model":           "claude-opus-5",
			"tools":           map[string]any{"deny": []any{"Write"}},
			"thinking_budget": float64(4096),
		}
		session.Options = document
		replay := fake.New()

		By("when the worker starts the session")
		turn, err := replay.Start(ctx, session, harness.Prompt{Text: "implement"}, nil)
		Expect(err).NotTo(HaveOccurred())
		_, err = turn.Wait(ctx)
		Expect(err).NotTo(HaveOccurred())

		By("then the harness was handed the document unchanged")
		Expect(replay.Calls()).To(HaveLen(1))
		Expect(replay.Calls()[0].Session.Options).To(Equal(document))
	})

	It("replays a terminal record with structured output", func(ctx context.Context) {
		replay := fake.New(fake.Script{
			Events:   []*harnessv1.Event{fake.Text("r1", "reviewing")},
			Terminal: fake.Terminal("reviewed", map[string]any{"decision": "approve", "findings": []any{}}, nil),
		})

		turn, err := replay.Start(ctx, session, harness.Prompt{Text: "review"}, nil)
		Expect(err).NotTo(HaveOccurred())
		terminal, err := turn.Wait(ctx)

		Expect(err).NotTo(HaveOccurred())
		Expect(terminal.StructuredOutput).To(HaveKeyWithValue("decision", "approve"))
		Expect(log.collected()).To(HaveLen(1))
		Expect(replay.Calls()).To(HaveLen(1))
		Expect(replay.Calls()[0].Verb).To(Equal("start"))
		Expect(replay.Calls()[0].Session.Options).To(HaveKeyWithValue("model", "claude-opus-5"))
	})

	It("replays a terminal record billing two models", func(ctx context.Context) {
		replay := fake.New(fake.Script{
			Events:   []*harnessv1.Event{fake.UsageEvent("r1", billed)},
			Terminal: fake.Terminal("implemented", nil, billed),
		})

		turn, err := replay.Start(ctx, session, harness.Prompt{Text: "implement"}, nil)
		Expect(err).NotTo(HaveOccurred())
		terminal, err := turn.Wait(ctx)

		Expect(err).NotTo(HaveOccurred())
		Expect(terminal.Usage).To(Equal(billed))
		Expect(harness.UsageFrom(log.collected()[0].GetUsage())).To(Equal(billed))
	})

	It("replays scripts in order and reuses the last", func(ctx context.Context) {
		replay := fake.New(
			fake.Script{Terminal: fake.Terminal("first", nil, nil)},
			fake.Script{Terminal: fake.Terminal("second", nil, nil)},
		)

		said := []string{}
		for range 3 {
			turn, err := replay.Start(ctx, session, harness.Prompt{}, nil)
			Expect(err).NotTo(HaveOccurred())
			terminal, err := turn.Wait(ctx)
			Expect(err).NotTo(HaveOccurred())
			said = append(said, terminal.FinalMessage)
		}

		Expect(said).To(Equal([]string{"first", "second", "second"}))
	})

	It("holds a turn until the session is cancelled", func(ctx context.Context) {
		replay := fake.New(fake.Script{Hold: true})

		turn, err := replay.Resume(ctx, session, harness.Prompt{}, nil)
		Expect(err).NotTo(HaveOccurred())
		Consistently(turn.Done()).ShouldNot(BeClosed())

		Expect(replay.Cancel(ctx, session)).To(Succeed())

		terminal, err := turn.Wait(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(terminal.Exit).To(Equal(harness.ProcessExit{Status: -1, Reason: harness.ReasonCancelled}))
		Expect(replay.Calls()[1].Verb).To(Equal("cancel"))
	})

	It("cancels a session that is not running without complaint", func(ctx context.Context) {
		Expect(fake.New().Cancel(ctx, session)).To(Succeed())
	})

	It("fails the call when the script says the harness itself failed", func(ctx context.Context) {
		refused := errors.New("no such harness")
		replay := fake.New(fake.Script{Err: refused})

		_, err := replay.Start(ctx, session, harness.Prompt{}, nil)

		Expect(err).To(MatchError(refused))
	})

	It("waits After before sending the event it names, and TerminalAfter before finishing", func(ctx context.Context) {
		replay := fake.New(fake.Script{
			Events:        []*harnessv1.Event{fake.Text("r1", "paused")},
			After:         map[int]time.Duration{0: 50 * time.Millisecond},
			TerminalAfter: 50 * time.Millisecond,
			Terminal:      fake.Terminal("done", nil, nil),
		})

		started := time.Now()
		turn, err := replay.Start(ctx, session, harness.Prompt{}, nil)
		Expect(err).NotTo(HaveOccurred())
		terminal, err := turn.Wait(ctx)

		Expect(err).NotTo(HaveOccurred())
		Expect(terminal.FinalMessage).To(Equal("done"))
		Expect(time.Since(started)).To(BeNumerically(">=", 100*time.Millisecond),
			"the event's own After and the turn's TerminalAfter must both have been waited out")
		Expect(log.collected()).To(HaveLen(1))
	})

	It("ends a paused turn with the context's error when the context is cancelled first", func() {
		startCtx, cancel := context.WithCancel(context.Background())
		replay := fake.New(fake.Script{
			Events:   []*harnessv1.Event{fake.Text("r1", "paused")},
			After:    map[int]time.Duration{0: 10 * time.Minute},
			Terminal: fake.Terminal("done", nil, nil),
		})

		turn, err := replay.Start(startCtx, session, harness.Prompt{}, nil)
		Expect(err).NotTo(HaveOccurred())
		cancel()

		waitCtx, waitCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer waitCancel()
		terminal, err := turn.Wait(waitCtx)

		Expect(err).To(MatchError(context.Canceled), "the turn must end on startCtx's own cancellation, not waitCtx's unrelated timeout")
		Expect(terminal).To(BeNil())
		Expect(log.collected()).To(BeEmpty(), "the paused event must never have been sent")
	})
})
