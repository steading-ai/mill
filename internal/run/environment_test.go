package run_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/harness"
	"github.com/steading-ai/mill/internal/harness/fake"
	"github.com/steading-ai/mill/internal/testsupport"
)

// What the ledger keeps of a session's environment.
//
// The decision itself is the adapter's — which mode, which floor, which
// deny-listed variable was present — and the specs of the same names in
// internal/harness/claude are where it is asserted. What is asserted here is
// mill's half: an adapter reports what it gave the process on the turn, and
// the attempt records it, so `mill inspect` can say what a workflow asked
// for and what was taken away.

// deciding is a harness that reports an environment decision the way the
// claude adapter does. It stands in for the adapter for the same reason the
// rest of this suite scripts the harness: a spec that spawned a real sidecar
// would be a spec about the sidecar.
type deciding struct {
	*fake.Harness
	env harness.Environment
}

func (d deciding) Start(ctx context.Context, session harness.Session, prompt harness.Prompt, attachments []harness.Attachment) (*harness.Turn, error) {
	return d.decide(d.Harness.Start(ctx, session, prompt, attachments))
}

func (d deciding) Resume(ctx context.Context, session harness.Session, prompt harness.Prompt, attachments []harness.Attachment) (*harness.Turn, error) {
	return d.decide(d.Harness.Resume(ctx, session, prompt, attachments))
}

func (d deciding) decide(turn *harness.Turn, err error) (*harness.Turn, error) {
	if turn != nil {
		turn.Environment = d.env
	}
	return turn, err
}

var _ = Describe("A session's environment", Label(testsupport.DBLabel), func() {
	var f *fixture
	var ctx context.Context

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
		f = start(ctx)
		f.node(ctx, "one line change", "Change the README's first line.")
		f.script(f.quickFixScripts()...)
	})

	// environments is what every skill attempt of the run recorded.
	environments := func(step string) []*millv1.StepRun {
		return stepRuns(f.inspect(ctx), step)
	}

	It("Inherit mode keeps credentials out", func() {
		By("given env_mode inherit and a mill process carrying ANTHROPIC_API_KEY and GOFLAGS")
		adapter := deciding{Harness: f.harness, env: harness.Environment{
			Stripped: []string{"ANTHROPIC_API_KEY", "CLAUDECODE", "DATABASE_URL", "SKIFT_PROJECT"},
		}}

		By("when the adapter spawns the sidecar")
		Expect(f.runner(withHarness(adapter)).Execute(ctx, f.nodeRef)).To(Succeed())

		By("and the step run records ANTHROPIC_API_KEY as stripped")
		attempts := environments("implement")
		Expect(attempts).To(HaveLen(1))
		Expect(attempts[0].GetEnvStripped()).To(ContainElement("ANTHROPIC_API_KEY"))
		Expect(attempts[0].GetEnvStripped()).To(ContainElements("CLAUDECODE", "DATABASE_URL", "SKIFT_PROJECT"))
		Expect(attempts[0].GetEnvPassthrough()).To(BeEmpty())
	})

	It("Strict mode is the floor and what the step asked for", func() {
		By("given a step whose claude options name env.passthrough GOFLAGS, under env_mode strict")
		adapter := deciding{Harness: f.harness, env: harness.Environment{
			Passthrough: []string{"GOFLAGS"},
			Stripped:    []string{"DATABASE_URL"},
		}}

		By("when the adapter spawns the sidecar")
		Expect(f.runner(withHarness(adapter)).Execute(ctx, f.nodeRef)).To(Succeed())

		By("then the step run records what the step asked for")
		attempts := environments("commit")
		Expect(attempts).To(HaveLen(1))
		Expect(attempts[0].GetEnvPassthrough()).To(Equal([]string{"GOFLAGS"}))
		Expect(attempts[0].GetEnvStripped()).To(Equal([]string{"DATABASE_URL"}))
	})

	It("records nothing for a harness that decided nothing", func() {
		Expect(f.runner().Execute(ctx, f.nodeRef)).To(Succeed())

		for _, step := range []string{"gather-context", "implement", "commit", "pr"} {
			attempts := environments(step)
			Expect(attempts).To(HaveLen(1))
			Expect(attempts[0].GetEnvPassthrough()).To(BeEmpty(), step)
			Expect(attempts[0].GetEnvStripped()).To(BeEmpty(), step)
		}
	})
})
