package fake_test

import (
	"context"
	"errors"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	harnessv1 "github.com/ivy/mill/gen/harness/v1"
	"github.com/ivy/mill/internal/harness"
	"github.com/ivy/mill/internal/harness/fake"
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
		session = harness.Session{ID: "01JB-implement", Model: "claude-opus-5", Events: log.sink()}
	})

	It("replays a terminal record with structured output", func(ctx context.Context) {
		replay := fake.New(fake.Script{
			Events:   []*harnessv1.Event{fake.Text("r1", "reviewing")},
			Terminal: fake.Terminal("reviewed", map[string]any{"decision": "approve", "findings": []any{}}, nil),
		})

		turn, err := replay.Start(ctx, session, harness.Prompt{Text: "review"}, nil, harness.Policy{Deny: []string{"Write"}})
		Expect(err).NotTo(HaveOccurred())
		terminal, err := turn.Wait(ctx)

		Expect(err).NotTo(HaveOccurred())
		Expect(terminal.StructuredOutput).To(HaveKeyWithValue("decision", "approve"))
		Expect(log.collected()).To(HaveLen(1))
		Expect(replay.Calls()).To(HaveLen(1))
		Expect(replay.Calls()[0].Verb).To(Equal("start"))
		Expect(replay.Calls()[0].Policy.Deny).To(Equal([]string{"Write"}))
	})

	It("replays a terminal record billing two models", func(ctx context.Context) {
		replay := fake.New(fake.Script{
			Events:   []*harnessv1.Event{fake.UsageEvent("r1", billed)},
			Terminal: fake.Terminal("implemented", nil, billed),
		})

		turn, err := replay.Start(ctx, session, harness.Prompt{Text: "implement"}, nil, harness.Policy{})
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
			turn, err := replay.Start(ctx, session, harness.Prompt{}, nil, harness.Policy{})
			Expect(err).NotTo(HaveOccurred())
			terminal, err := turn.Wait(ctx)
			Expect(err).NotTo(HaveOccurred())
			said = append(said, terminal.FinalMessage)
		}

		Expect(said).To(Equal([]string{"first", "second", "second"}))
	})

	It("holds a turn until the session is cancelled", func(ctx context.Context) {
		replay := fake.New(fake.Script{Hold: true})

		turn, err := replay.Resume(ctx, session, harness.Prompt{}, nil, harness.Policy{})
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

		_, err := replay.Start(ctx, session, harness.Prompt{}, nil, harness.Policy{})

		Expect(err).To(MatchError(refused))
	})
})
