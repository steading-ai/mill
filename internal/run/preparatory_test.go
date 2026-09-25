package run_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/durationpb"

	harnessv1 "github.com/steading-ai/mill/gen/harness/v1"
	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/harness"
	"github.com/steading-ai/mill/internal/harness/fake"
	"github.com/steading-ai/mill/internal/run"
	"github.com/steading-ai/mill/internal/testsupport"
	"github.com/steading-ai/mill/internal/workspace"
	"github.com/steading-ai/mill/internal/workspace/machine/config"
	"github.com/steading-ai/mill/internal/workspace/machine/image"
)

// Preparatory steps: docs/sandbox.md §"Preparatory steps". A step that
// readies the machine closes on its exit and output whichever way it ended,
// its output stays out of every session, and its failure is a finding the
// first session receives — never a failed attempt, never the breaker.
//
// The workspace here is the local one, which provisions no machine, wrapped
// to say it does (workspace.Provisioned): the loop runs a preparatory step
// only in a workspace that readies a machine, and what the loop does with
// the step is the same whichever workspace runs it. The toolchain command
// is mise's own, isolated from the host's configuration so that what it
// installs — nothing — and what it refuses come from the checkout alone.

// provisioned is the fixture's workspace claiming a machine: findings
// provisioning left, and the shim's reports taken over the socket.
type provisioned struct {
	run.Workspace
	findings []string

	mu       sync.Mutex
	installs []workspace.Install
}

func (p *provisioned) Findings() []string { return p.findings }

func (p *provisioned) ReportInstall(_ context.Context, packages []string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.installs = append(p.installs, workspace.Install{Packages: packages})
	return nil
}

func (p *provisioned) Installs() []workspace.Install {
	p.mu.Lock()
	defer p.mu.Unlock()
	installs := p.installs
	p.installs = nil
	return installs
}

// miseInstall is the toolchain command as the spec runs it on the host: the
// checkout's mise.toml and nothing of the account's. The local workspace
// gives a command no environment at all, so the spec lends it the process's
// own PATH and HOME to find mise and its state; the machine has its own.
func miseInstall() string {
	return `HOME="` + os.Getenv("HOME") + `" PATH="` + os.Getenv("PATH") + `" ` +
		`MISE_GLOBAL_CONFIG_FILE=/dev/null MISE_SYSTEM_CONFIG_FILE=/dev/null MISE_TRUSTED_CONFIG_PATHS="$PWD" mise install`
}

var _ = Describe("Preparatory steps", Label(testsupport.DBLabel), func() {
	var f *fixture
	var ctx context.Context
	var ws *provisioned
	var logDir string

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
		f = start(ctx)
		f.node(ctx, "one line change", "Change the README's first line.")
		logDir = GinkgoT().TempDir()
		ws = &provisioned{}
		f.wrapWorkspace = func(inner run.Workspace) run.Workspace {
			ws.Workspace = inner
			return ws
		}
	})

	// seedOrigin commits files onto the origin's default branch, so the
	// run's checkout carries them.
	seedOrigin := func(files map[string]string) {
		GinkgoHelper()
		work := filepath.Join(f.root, "origin-work")
		for name, content := range files {
			Expect(os.WriteFile(filepath.Join(work, name), []byte(content), 0o644)).To(Succeed())
			git(work, "add", name)
		}
		git(work, "commit", "-m", "seed")
		git(work, "push", f.origin, "main")
	}

	// toolchain replaces quick-fix's toolchain command, and its budget when
	// one is given.
	toolchain := func(name string, command []string, timeout time.Duration) string {
		return f.variant(ctx, name, func(definition *millv1.Workflow) {
			step := stepOf(definition, "toolchain")
			step.GetCheck().Command = command
			if timeout > 0 {
				step.Timeout = durationpb.New(timeout)
			}
		})
	}

	// firstPrompt is what the first session was asked.
	firstPrompt := func() string {
		GinkgoHelper()
		calls := f.harness.Calls()
		Expect(calls).NotTo(BeEmpty())
		return calls[0].Prompt.Text
	}

	// spoken is a clean pass of quick-fix whose every session says something
	// before it ends, so the ledger marks each session started as a real
	// harness would: the findings are owed to the first session and to no
	// session after it has begun.
	spoken := func() []fake.Script {
		scripts := f.quickFixScripts()
		for i := range scripts {
			scripts[i].Events = []*harnessv1.Event{fake.Text("turn-1", "working")}
		}
		return scripts
	}

	execute := func(workflow string) {
		GinkgoHelper()
		f.script(spoken()...)
		Expect(f.runner(func(o *run.Options) { o.Workflow = workflow; o.LogDir = logDir }).
			Execute(ctx, f.nodeRef)).To(Succeed())
	}

	// Scenario: The toolchain step's output stays out of the session
	//   When the toolchain step runs and the first session starts
	//   Then the step's evidence holds the output
	//   And the session's transcript does not
	It("The toolchain step's output stays out of the session", func() {
		By("given a checkout whose mise.toml pins nothing, so mise install exits 0")
		seedOrigin(map[string]string{"mise.toml": "[tools]\n"})
		workflow := toolchain("readied", []string{"sh", "-c", miseInstall()}, 0)

		By("when the toolchain step runs and the first session starts")
		execute(workflow)

		By("then the step closed on the exit code and captured output as evidence")
		detail := f.inspect(ctx)
		Expect(stepRuns(detail, "toolchain")).To(HaveLen(1))
		Expect(stepRuns(detail, "toolchain")[0].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED))
		evidence := evidenceOf(detail, "toolchain")
		Expect(evidence.GetKind()).To(Equal(millv1.EvidenceKind_EVIDENCE_KIND_EXIT))
		Expect(evidence.GetExit().GetStatus()).To(BeZero())
		Expect(evidence.GetOutput()).To(ContainSubstring("all tools are installed"), "mise's own words are the record")
		Expect(evidence.GetVerifiedBy()).To(Equal("preparatory toolchain"))

		By("and the session's transcript contains none of mise's output")
		Expect(firstPrompt()).NotTo(ContainSubstring("all tools are installed"))
		Expect(firstPrompt()).NotTo(ContainSubstring("Preparatory step"), "a step that exited 0 owes no finding")
		Expect(renderedLog(logDir, f.runOf(ctx).GetId(), "gather-context", 1)).
			NotTo(ContainSubstring("all tools are installed"))

		By("and the step's own log holds it, for mill logs")
		Expect(renderedLog(logDir, f.runOf(ctx).GetId(), "toolchain", 1)).
			To(ContainSubstring("all tools are installed"))
	})

	// Scenario: A failed toolchain step becomes a finding and the run continues
	//   Given mise.toml declares a tool version that does not exist
	//   When the toolchain step runs
	//   Then it exits non-zero, the run continues, and the first session receives the finding
	It("A failed toolchain step becomes a finding and the run continues", func() {
		By("given mise.toml declares a tool version that does not exist")
		seedOrigin(map[string]string{"mise.toml": "[tools]\n\"no-such-tool-xyz\" = \"1.2.3\"\n"})
		workflow := toolchain("misdeclared", []string{"sh", "-c", miseInstall()}, 0)

		By("when the toolchain step runs")
		execute(workflow)

		By("then it exits non-zero and the step is closed on that record")
		detail := f.inspect(ctx)
		evidence := evidenceOf(detail, "toolchain")
		Expect(evidence.GetExit().GetStatus()).To(BeNumerically(">", 0))
		Expect(stepRuns(detail, "toolchain")).To(HaveLen(1))
		Expect(stepRuns(detail, "toolchain")[0].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED))

		By("and the run continues to the end")
		Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_DONE))
		Expect(closedSteps(detail)).To(HaveKey("merge"))

		By("and the first session's inputs carry a finding with the last lines of the output")
		prompt := firstPrompt()
		Expect(prompt).To(ContainSubstring("## What the last attempt failed on"), "the path a revise's findings take")
		Expect(prompt).To(ContainSubstring("Preparatory step `toolchain`"))
		Expect(prompt).To(ContainSubstring("exited 1"))
		Expect(prompt).To(ContainSubstring("no-such-tool-xyz not found in mise tool registry"))

		By("and only the first session: the ones after it have begun and owe nothing")
		calls := f.harness.Calls()
		Expect(len(calls)).To(BeNumerically(">", 1))
		for _, call := range calls[1:] {
			Expect(call.Prompt.Text).NotTo(ContainSubstring("Preparatory step `toolchain`"))
		}
	})

	// Scenario: No preparatory step can trip the circuit breaker
	//   Given every preparatory step of a run failed
	//   When the run's retry accounting is inspected
	//   Then no attempt has been consumed
	It("No preparatory step can trip the circuit breaker", func() {
		By("given a workflow with two preparatory steps that both fail, and no retries to spend")
		workflow := f.variant(ctx, "twice-unready", func(definition *millv1.Workflow) {
			step := stepOf(definition, "toolchain")
			step.GetCheck().Command = []string{"sh", "-c", "echo 'mise ERROR nothing here' >&2; exit 3"}
			step.Retries = 1
			insertAfter(definition, "toolchain", &millv1.Step{
				Name:     "warm-cache",
				Kind:     millv1.StepKind_STEP_KIND_PREPARATORY,
				Check:    &millv1.Check{Command: []string{"sh", "-c", "echo 'cache miss'; exit 2"}},
				Evidence: millv1.EvidenceKind_EVIDENCE_KIND_EXIT,
				Retries:  1,
				Timeout:  durationpb.New(time.Minute),
			})
		})

		By("when every preparatory step of the run has failed")
		execute(workflow)

		By("then no attempt has been consumed: one closed attempt each, nothing failed, nothing parked")
		detail := f.inspect(ctx)
		for _, name := range []string{"toolchain", "warm-cache"} {
			attempts := stepRuns(detail, name)
			Expect(attempts).To(HaveLen(1), name)
			Expect(attempts[0].GetAttempt()).To(Equal(int64(1)), name)
			Expect(attempts[0].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED), name)
			Expect(evidenceOf(detail, name).GetExit().GetStatus()).To(BeNumerically(">", 0), name)
		}
		for _, d := range detail.GetSteps() {
			Expect(d.GetStepRun().GetStatus()).NotTo(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_FAILED),
				"%s attempt %d", d.GetStepRun().GetStepName(), d.GetStepRun().GetAttempt())
		}
		Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_DONE))
		Expect(f.gatesOn(ctx)).To(BeEmpty(), "no park gate was filed")

		By("and both findings reached the first session, in workflow order")
		prompt := firstPrompt()
		Expect(prompt).To(ContainSubstring("Preparatory step `toolchain`"))
		Expect(prompt).To(ContainSubstring("mise ERROR nothing here"))
		Expect(prompt).To(ContainSubstring("Preparatory step `warm-cache`"))
		Expect(prompt).To(ContainSubstring("cache miss"))
		Expect(strings.Index(prompt, "Preparatory step `toolchain`")).
			To(BeNumerically("<", strings.Index(prompt, "Preparatory step `warm-cache`")))
	})

	It("A preparatory step over its timeout is stopped and its finding names the budget", func() {
		By("given a toolchain step whose budget is a second and whose command outlives it")
		workflow := toolchain("slow-toolchain", []string{"sh", "-c", "echo warming up; sleep 30"}, time.Second)

		By("when the step exceeds it")
		started := time.Now()
		execute(workflow)
		Expect(time.Since(started)).To(BeNumerically("<", 25*time.Second), "the command was stopped, not waited for")

		By("then the step is stopped and closed on that record")
		detail := f.inspect(ctx)
		evidence := evidenceOf(detail, "toolchain")
		Expect(evidence.GetExit().GetStatus()).To(Equal(int32(-1)))
		Expect(evidence.GetExit().GetReason()).To(Equal(harnessv1.ExitReason_EXIT_REASON_TIMEOUT))
		Expect(evidence.GetOutput()).To(ContainSubstring("warming up"))

		By("and the finding names the budget")
		prompt := firstPrompt()
		Expect(prompt).To(ContainSubstring("Preparatory step `toolchain`"))
		Expect(prompt).To(ContainSubstring("was stopped at its 1s budget"))
		Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_DONE))
	})

	It("A derived build that failed and an invalid mill.toml each reach the first session as findings in the same shape", func() {
		By("given provisioning left both findings on the workspace, in the words image.Ensure and config.Load use")
		build := &image.Finding{
			Packages: []string{"vips-devel", "libpq-devl"},
			Stage:    "build",
			Output:   "No match for argument: libpq-devl\nError: Unable to find a match: libpq-devl",
		}
		declaration := config.Finding{File: "mill.toml", Key: "cache.go-build.path", Message: "the path is not under the home directory"}
		ws.findings = []string{declaration.String(), build.String()}
		workflow := toolchain("well-readied", []string{"true"}, 0)

		By("when the first session starts")
		execute(workflow)

		By("then both reach it under the one heading a revise's findings take, each one line in the same shape")
		prompt := firstPrompt()
		Expect(prompt).To(ContainSubstring("## What the last attempt failed on"))
		Expect(prompt).To(ContainSubstring("Provisioning: " + declaration.String()))
		Expect(prompt).To(ContainSubstring("Provisioning: " + build.String()))
		Expect(strings.Count(prompt, "## What the last attempt failed on")).To(Equal(1), "one path, one heading")

		By("and the toolchain step that exited 0 added nothing")
		Expect(prompt).NotTo(ContainSubstring("Preparatory step"))
	})

	It("An install reported over the socket is the run's provenance, and not the reviewer's", func() {
		By("given a session that installs a package with the shim and does not change mill.toml")
		workflow := toolchain("installing", []string{"true"}, 0)
		scripts := spoken()
		scripts[0].Do = func(harness.Session) {
			defer GinkgoRecover()
			Expect(ws.ReportInstall(context.Background(), []string{"vips-devel"})).To(Succeed())
		}
		f.script(scripts...)
		Expect(f.runner(func(o *run.Options) { o.Workflow = workflow }).Execute(ctx, f.nodeRef)).To(Succeed())

		By("when the run's provenance is read")
		res, err := f.store.History(ctx, connect.NewRequest(&millv1.HistoryRequest{NodeRef: f.nodeRef}))
		Expect(err).NotTo(HaveOccurred())
		var installed []*millv1.Event
		for _, event := range res.Msg.GetEvents() {
			if event.GetOp() == "installed" {
				installed = append(installed, event)
			}
		}

		By("then the install is recorded as undeclared, against the attempt whose session made it")
		Expect(installed).To(HaveLen(1))
		Expect(installed[0].GetAfter().AsMap()).To(HaveKeyWithValue("detail", "installed vips-devel, not declared in mill.toml"))
		Expect(installed[0].GetAfter().AsMap()).To(HaveKeyWithValue("packages", []any{"vips-devel"}))
		Expect(installed[0].GetStepRunId()).To(Equal(stepRuns(f.inspect(ctx), "gather-context")[0].GetId()))

		By("and no later session was told")
		for _, call := range f.harness.Calls()[1:] {
			Expect(call.Prompt.Text).NotTo(ContainSubstring("vips-devel"))
		}
		Expect(ws.Installs()).To(BeEmpty(), "drained into the ledger")
	})

	It("A workspace that provisions no machine closes a preparatory step on the record that it did not run", func() {
		By("given the local workspace as it is, with nothing to ready")
		f.wrapWorkspace = nil
		workflow := toolchain("unreadied", []string{"sh", "-c", "echo 'this would trust the host'; exit 1"}, 0)

		By("when the run walks its toolchain step")
		execute(workflow)

		By("then the step closed without running, on exit evidence that says so, and no finding was owed")
		detail := f.inspect(ctx)
		evidence := evidenceOf(detail, "toolchain")
		Expect(evidence.GetKind()).To(Equal(millv1.EvidenceKind_EVIDENCE_KIND_EXIT))
		Expect(evidence.GetExit().GetStatus()).To(Equal(int32(-1)))
		Expect(evidence.GetExit().GetReason()).To(Equal(harnessv1.ExitReason_EXIT_REASON_REFUSED))
		Expect(evidence.GetOutput()).To(ContainSubstring("not run"))
		Expect(evidence.GetOutput()).NotTo(ContainSubstring("this would trust the host"))
		Expect(firstPrompt()).NotTo(ContainSubstring("## What the last attempt failed on"))
		Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_DONE))
	})
})
