package run_test

import (
	"context"
	"fmt"
	"sync"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	harnessv1 "github.com/ivy/mill/gen/harness/v1"
	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/harness"
	"github.com/ivy/mill/internal/harness/fake"
	"github.com/ivy/mill/internal/run"
	"github.com/ivy/mill/internal/testsupport"
)

// A session that ends its turn with a background task still running is
// waiting to be woken. The harness watches that wait: it asks a session
// whose task has gone quiet whether the task is still needed, and after the
// last heartbeat it kills what the session would not stop and ends the
// session with reason `stalled` (docs/domain-model.md §Session,
// sidecar/src/tasks.ts).
//
// What the mill owes that is a record: the asks are rows of the run's audit
// trail, and the exit is a failed attempt like any other — the session ran,
// so its last working words are what the next attempt is given.

var _ = Describe("A session the harness's breaker stopped", Label(testsupport.DBLabel), func() {
	var f *fixture
	var ctx context.Context

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
		f = start(ctx)
		f.node(ctx, "one line change", "Change the README's first line.")
	})

	It("records the heartbeats it took and fails the attempt with the session's last words", func() {
		By("given a step whose first session stalls after two heartbeats")
		const stallModel = "stalls-once"
		workflow := f.retriedReview(ctx, stallModel, 1, nil)
		adapter := &staller{model: stallModel, commits: f.commits}

		By("when the run reaches it")
		Expect(f.runner(withHarness(adapter), func(o *run.Options) { o.Workflow = workflow }).
			Execute(ctx, f.nodeRef)).To(Succeed())

		By("then the stalled attempt failed, and its exit says why")
		detail := f.inspect(ctx)
		attempts := stepRuns(detail, "review")
		Expect(attempts).To(HaveLen(2))
		Expect(attempts[0].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_FAILED))
		Expect(attempts[0].GetExit().GetStatus()).To(Equal(int32(1)))
		Expect(attempts[0].GetExit().GetReason()).To(Equal(harnessv1.ExitReason_EXIT_REASON_STALLED))
		Expect(attempts[1].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED))

		By("and the two heartbeats are on that attempt")
		var stalled *millv1.StepRunDetail
		for _, d := range detail.GetSteps() {
			if d.GetStepRun().GetId() == attempts[0].GetId() {
				stalled = d
			}
		}
		Expect(stalled).NotTo(BeNil())
		Expect(stalled.GetHeartbeats()).To(Equal(int64(2)))

		By("and the run's history says what was asked")
		res, err := f.store.History(ctx, connect.NewRequest(&millv1.HistoryRequest{NodeRef: f.nodeRef}))
		Expect(err).NotTo(HaveOccurred())
		asked := []*millv1.Event{}
		for _, event := range res.Msg.GetEvents() {
			if event.GetOp() == "session_heartbeat" {
				asked = append(asked, event)
			}
		}
		Expect(asked).To(HaveLen(2))
		Expect(asked[0].GetStepRunId()).To(Equal(attempts[0].GetId()))
		Expect(asked[0].GetAfter().GetFields()["strike"].GetNumberValue()).To(Equal(float64(1)))
		Expect(asked[1].GetAfter().GetFields()["strikes"].GetNumberValue()).To(Equal(float64(3)))

		By("and the session's last working words reached the next attempt")
		prompts := adapter.reviewed()
		Expect(prompts).To(HaveLen(2))
		Expect(prompts[1]).To(ContainSubstring("I started the suite and was waiting on it"))
	})
})

// staller is a harness whose first turn at the step under test ends the way
// the sidecar's breaker ends one: the heartbeats it sent, and a failure
// carrying the last working turn's words. Every other turn commits, because
// the `commit` step closes on a commit that was not there before it.
type staller struct {
	model   string
	commits func(harness.Session)

	mu      sync.Mutex
	rounds  int
	prompts []string
}

func (s *staller) Start(_ context.Context, session harness.Session, prompt harness.Prompt, _ []harness.Attachment) (*harness.Turn, error) {
	return s.play(session, prompt), nil
}

func (s *staller) Resume(_ context.Context, session harness.Session, prompt harness.Prompt, _ []harness.Attachment) (*harness.Turn, error) {
	return s.play(session, prompt), nil
}

func (s *staller) Cancel(context.Context, harness.Session) error { return nil }

func (s *staller) play(session harness.Session, prompt harness.Prompt) *harness.Turn {
	s.mu.Lock()
	stalling := session.Options["model"] == s.model
	round := 0
	if stalling {
		s.rounds++
		round = s.rounds
		s.prompts = append(s.prompts, prompt.Text)
	}
	s.mu.Unlock()

	turn := harness.NewTurn()
	go func() {
		if stalling && round == 1 {
			const words = "I started the suite and was waiting on it"
			session.Events.Send(fake.Heartbeat("stall-1", 1, 3, 180, "Bash: make test"))
			session.Events.Send(fake.Heartbeat("stall-1", 2, 3, 360, "Bash: make test"))
			turn.Finish(fake.Stalled(words), nil)
			return
		}
		if !stalling && s.commits != nil {
			s.commits(session)
		}
		turn.Finish(fake.Terminal(fmt.Sprintf("round %d: looks right", round), nil, usage(1)), nil)
	}()
	return turn
}

// reviewed is what each turn at the step under test was given, in order.
func (s *staller) reviewed() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.prompts...)
}
