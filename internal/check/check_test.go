package check_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	harnessv1 "github.com/ivy/mill/gen/harness/v1"
	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/check"
	"github.com/ivy/mill/internal/evidence"
	"github.com/ivy/mill/internal/harness"
	"github.com/ivy/mill/internal/harness/fake"
)

// dirtyLine is what clean-tree writes to stderr when the tree has changes.
// docs/domain-model.md §Check quotes it; the script is the canonical check.
const dirtyLine = "Workspace is dirty: stage, delete, or ignore uncommitted files"

var _ = Describe("The clean-tree check", Label("unit"), func() {
	var (
		ctx  context.Context
		tree *checkout
		step *millv1.Step
	)

	BeforeEach(func() {
		ctx = context.Background()
		tree = newCheckout()
		tree.repository()
		step = &millv1.Step{
			Name: "clean-tree",
			Kind: millv1.StepKind_STEP_KIND_CHECK,
			Check: &millv1.Check{
				Command:  []string{"scripts/clean-tree"},
				Exit:     []int32{0},
				Stdout:   millv1.StdoutShape_STDOUT_SHAPE_NONE,
				Feedback: millv1.Feedback_FEEDBACK_REVISE,
			},
			Evidence: millv1.EvidenceKind_EVIDENCE_KIND_EXIT,
			OnFailure: &millv1.OnFailure{
				Kind:         millv1.OnFailureKind_ON_FAILURE_KIND_REVISE,
				ReviseTarget: "implement",
			},
		}
	})

	run := func() check.Result {
		GinkgoHelper()
		result, err := check.Run(ctx, tree, check.Request{
			Name:        step.GetName(),
			Check:       step.GetCheck(),
			WorkflowDir: workflowDir(),
		})
		Expect(err).NotTo(HaveOccurred())
		return result
	}

	It("passes on a tree with nothing uncommitted", func() {
		result := run()

		Expect(result.Passed).To(BeTrue(), result.Stderr)
		Expect(result.Exit.GetStatus()).To(BeZero())
		Expect(result.Stderr).To(BeEmpty())
		Expect(check.Feedback(step.GetCheck(), result)).To(BeEmpty())
	})

	DescribeTable("fails with the same stderr line whatever made the tree dirty",
		func(dirty func()) {
			dirty()

			result := run()

			Expect(result.Passed).To(BeFalse())
			Expect(result.Exit.GetStatus()).To(Equal(int32(1)))
			Expect(firstLine(result.Stderr)).To(Equal(dirtyLine))
		},
		Entry("an untracked file", func() { tree.write("scratch.txt", "left behind\n") }),
		Entry("a modified file", func() { tree.write("README.md", "changed after the commit\n") }),
		Entry("a staged but uncommitted file", func() {
			tree.write("staged.txt", "added to the index\n")
			tree.git("add", "staged.txt")
		}),
	)

	It("names the dirty paths after the line, so the session knows what to do", func() {
		tree.write("scratch.txt", "left behind\n")

		result := run()

		Expect(result.Stderr).To(ContainSubstring("?? scratch.txt"))
	})

	It("produces exit evidence the verifier refuses when the tree is dirty", func() {
		tree.write("scratch.txt", "left behind\n")

		result := run()
		_, err := evidence.Verify(ctx, millv1.EvidenceKind_EVIDENCE_KIND_EXIT, evidence.Step{
			Name: step.GetName(), Definition: step, Exit: result.Exit,
		})

		Expect(err).To(MatchError(ContainSubstring("exited 1, not 0")))
	})

	It("A check's stderr becomes the next prompt", func() {
		// Given clean-tree fails with its stderr line.
		tree.write("scratch.txt", "left behind\n")
		result := run()
		Expect(result.Passed).To(BeFalse())
		Expect(result.Stderr).To(ContainSubstring(dirtyLine))

		// And the step's on_failure is revise implement.
		Expect(step.GetOnFailure().GetKind()).To(Equal(millv1.OnFailureKind_ON_FAILURE_KIND_REVISE))
		Expect(step.GetOnFailure().GetReviseTarget()).To(Equal("implement"))

		// When the run loop handles the failure: the check's feedback is
		// what the revised step's session is prompted with.
		implement := fake.New(fake.Script{Terminal: fake.Terminal("fixed", nil, nil)})
		_, err := implement.Resume(ctx,
			harness.Session{ID: "implement"},
			harness.Prompt{Text: check.Feedback(step.GetCheck(), result)},
			nil)
		Expect(err).NotTo(HaveOccurred())

		// Then the implement session's next prompt contains that stderr
		// line verbatim.
		calls := implement.Calls()
		Expect(calls).To(HaveLen(1))
		Expect(calls[0].Verb).To(Equal("resume"))
		Expect(calls[0].Session.ID).To(Equal("implement"))
		Expect(calls[0].Prompt.Text).To(ContainSubstring(dirtyLine))
		Expect(calls[0].Prompt.Text).To(ContainSubstring("?? scratch.txt"))
	})

	It("hands nothing back when the definition parks instead of revising", func() {
		tree.write("scratch.txt", "left behind\n")
		step.Check.Feedback = millv1.Feedback_FEEDBACK_PARK

		result := run()

		Expect(result.Stderr).To(ContainSubstring(dirtyLine))
		Expect(check.Feedback(step.GetCheck(), result)).To(BeEmpty())
	})
})

var _ = Describe("Exit-code semantics", Label("unit"), func() {
	var tree *checkout

	BeforeEach(func() { tree = newCheckout() })

	run := func(c *millv1.Check, dir string) check.Result {
		GinkgoHelper()
		result, err := check.Run(context.Background(), tree, check.Request{Name: "probe", Check: c, WorkflowDir: dir})
		Expect(err).NotTo(HaveOccurred())
		return result
	}

	It("passes on zero when the definition names no code at all", func() {
		dir := script(GinkgoT().TempDir(), "scripts/ok", "#!/bin/sh\nexit 0\n")

		result := run(&millv1.Check{Command: []string{"scripts/ok"}}, dir)

		Expect(result.Passed).To(BeTrue())
		Expect(evidence.Codes(&millv1.Check{})).To(Equal([]int32{0}))
	})

	It("passes on a non-zero status the definition names", func() {
		dir := script(GinkgoT().TempDir(), "scripts/two", "#!/bin/sh\nexit 2\n")

		result := run(&millv1.Check{Command: []string{"scripts/two"}, Exit: []int32{0, 2}}, dir)

		Expect(result.Passed).To(BeTrue())
		Expect(result.Exit.GetStatus()).To(Equal(int32(2)))
	})

	It("reports a command killed by a signal as never having run", func() {
		dir := script(GinkgoT().TempDir(), "scripts/die", "#!/bin/sh\nkill -TERM $$\n")

		result := run(&millv1.Check{Command: []string{"scripts/die"}}, dir)

		Expect(result.Passed).To(BeFalse())
		Expect(result.Exit.GetStatus()).To(Equal(int32(-1)))
		Expect(result.Exit.GetSignal()).To(Equal(int32(15)))
		Expect(result.Reason).To(Equal("the command never ran"))
	})

	It("kills the command when its context ends, and records the budget as the reason", func() {
		dir := script(GinkgoT().TempDir(), "scripts/slow", "#!/bin/sh\necho warming up\nsleep 30\n")
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()

		started := time.Now()
		result, err := check.Run(ctx, tree, check.Request{Name: "probe",
			Check: &millv1.Check{Command: []string{"scripts/slow"}, Stdout: millv1.StdoutShape_STDOUT_SHAPE_TEXT}, WorkflowDir: dir})
		Expect(err).NotTo(HaveOccurred())

		Expect(time.Since(started)).To(BeNumerically("<", 10*time.Second), "stopped at the budget, not waited for")
		Expect(result.Passed).To(BeFalse())
		Expect(result.Exit.GetStatus()).To(Equal(int32(-1)))
		Expect(result.Exit.GetReason()).To(Equal(harnessv1.ExitReason_EXIT_REASON_TIMEOUT))
		Expect(result.Stdout).To(Equal("warming up\n"), "what it wrote before the budget is kept")
	})

	It("passes the command's arguments through as the definition wrote them", func() {
		dir := script(GinkgoT().TempDir(), "scripts/argv", "#!/bin/sh\nprintf '%s\\n' \"$@\"\n")

		result := run(&millv1.Check{
			Command: []string{"scripts/argv", "--fix", "internal/check"},
			Stdout:  millv1.StdoutShape_STDOUT_SHAPE_TEXT,
		}, dir)

		Expect(result.Stdout).To(Equal("--fix\ninternal/check\n"))
	})
})

var _ = Describe("A check's stdout contract", Label("unit"), func() {
	const schema = `{
      "type": "object",
      "required": ["status", "files"],
      "properties": {
        "status": {"type": "string", "enum": ["clean", "dirty"]},
        "files": {"type": "array", "items": {"type": "object", "required": ["path"],
                  "properties": {"path": {"type": "string"}, "lines": {"type": "integer"}}}}
      }
    }`

	var tree *checkout

	BeforeEach(func() { tree = newCheckout() })

	run := func(body string, shape millv1.StdoutShape, stdoutSchema string) check.Result {
		GinkgoHelper()
		dir := script(GinkgoT().TempDir(), "scripts/report", body)
		result, err := check.Run(context.Background(), tree, check.Request{
			Name:        "report",
			Check:       &millv1.Check{Command: []string{"scripts/report"}, Stdout: shape, StdoutSchema: stdoutSchema},
			WorkflowDir: dir,
		})
		Expect(err).NotTo(HaveOccurred())
		return result
	}

	It("keeps nothing when the contract is none", func() {
		result := run("#!/bin/sh\necho noise\n", millv1.StdoutShape_STDOUT_SHAPE_NONE, "")

		Expect(result.Passed).To(BeTrue())
		Expect(result.Stdout).To(BeEmpty())
		Expect(result.JSON).To(BeNil())
	})

	It("keeps stdout as written when the contract is text", func() {
		result := run("#!/bin/sh\necho 'two files changed'\n", millv1.StdoutShape_STDOUT_SHAPE_TEXT, "")

		Expect(result.Stdout).To(Equal("two files changed\n"))
	})

	It("parses and validates stdout when the contract is json with a schema", func() {
		result := run(`#!/bin/sh
echo '{"status": "clean", "files": [{"path": "internal/check/check.go", "lines": 3}]}'
`, millv1.StdoutShape_STDOUT_SHAPE_JSON, schema)

		Expect(result.Passed).To(BeTrue(), result.Reason)
		Expect(result.JSON.GetFields()["status"].GetStringValue()).To(Equal("clean"))
	})

	It("rejects output that does not validate, naming the violating path", func() {
		result := run(`#!/bin/sh
echo '{"status": "clean", "files": [{"path": "a.go"}, {"path": "b.go", "lines": "three"}]}'
`, millv1.StdoutShape_STDOUT_SHAPE_JSON, schema)

		Expect(result.Passed).To(BeFalse())
		Expect(result.Exit.GetStatus()).To(BeZero(), "the command itself exited 0; its output is what failed")
		Expect(result.Reason).To(ContainSubstring("/files/1/lines is a string, not integer"))
	})

	It("rejects output that is not JSON at all", func() {
		result := run("#!/bin/sh\necho 'not json'\n", millv1.StdoutShape_STDOUT_SHAPE_JSON, schema)

		Expect(result.Passed).To(BeFalse())
		Expect(result.Reason).To(ContainSubstring("stdout is not a JSON object"))
	})

	It("produces exit evidence the verifier refuses when the output did not validate", func() {
		result := run("#!/bin/sh\necho '{}'\n", millv1.StdoutShape_STDOUT_SHAPE_JSON, schema)

		Expect(result.Passed).To(BeFalse())
		Expect(result.Reason).To(ContainSubstring("/status is required and missing"))
		Expect(result.Exit).To(Equal(&harnessv1.ProcessExit{Status: 0}))
	})
})

var _ = Describe("Resolving a check's command", Label("unit"), func() {
	It("prefers the skill's own copy of a script over the workflow's", func() {
		skill := script(GinkgoT().TempDir(), "scripts/lint", "#!/bin/sh\nexit 0\n")
		flow := script(GinkgoT().TempDir(), "scripts/lint", "#!/bin/sh\nexit 1\n")

		command, err := check.Command(check.Request{
			Name:        "lint",
			Check:       &millv1.Check{Command: []string{"scripts/lint"}},
			SkillDir:    skill,
			WorkflowDir: flow,
		})

		Expect(err).NotTo(HaveOccurred())
		Expect(command.Path).To(HavePrefix(skill))
	})

	It("falls back to the workflow when the skill does not carry the script", func() {
		flow := script(GinkgoT().TempDir(), "scripts/lint", "#!/bin/sh\nexit 0\n")

		command, err := check.Command(check.Request{
			Name:        "lint",
			Check:       &millv1.Check{Command: []string{"scripts/lint"}},
			SkillDir:    GinkgoT().TempDir(),
			WorkflowDir: flow,
		})

		Expect(err).NotTo(HaveOccurred())
		Expect(command.Path).To(Equal(flow + "/scripts/lint"))
	})

	It("leaves a bare program name for the workspace to resolve on PATH", func() {
		command, err := check.Command(check.Request{
			Name:        "test",
			Check:       &millv1.Check{Command: []string{"make", "test"}},
			WorkflowDir: GinkgoT().TempDir(),
		})

		Expect(err).NotTo(HaveOccurred())
		Expect(command.Path).To(Equal("make"))
		Expect(command.Args).To(Equal([]string{"test"}))
	})

	It("refuses a command that climbs out of the definition's directory", func() {
		_, err := check.Command(check.Request{
			Name:        "sneak",
			Check:       &millv1.Check{Command: []string{"../../../usr/bin/env"}},
			WorkflowDir: GinkgoT().TempDir(),
		})

		Expect(errors.Is(err, check.ErrOutsideDefinition)).To(BeTrue(), "%v", err)
	})

	It("refuses an absolute command", func() {
		_, err := check.Command(check.Request{
			Name:        "sneak",
			Check:       &millv1.Check{Command: []string{"/usr/bin/env"}},
			WorkflowDir: GinkgoT().TempDir(),
		})

		Expect(errors.Is(err, check.ErrOutsideDefinition)).To(BeTrue(), "%v", err)
	})

	It("says where it looked when the script is in neither directory", func() {
		flow := GinkgoT().TempDir()

		_, err := check.Command(check.Request{
			Name:        "lint",
			Check:       &millv1.Check{Command: []string{"scripts/lint"}},
			WorkflowDir: flow,
		})

		Expect(err).To(MatchError(ContainSubstring(strings.TrimSuffix(flow, "/") + "/scripts/lint")))
	})

	It("refuses a definition with no command", func() {
		_, err := check.Command(check.Request{Name: "empty", Check: &millv1.Check{}})

		Expect(err).To(MatchError(ContainSubstring("names no command")))
	})
})

// fakeSink is check.Sink for a spec that wants to see what Run sends it: the
// resolved command and contract, and each output chunk in the order Run
// produced it. Its own methods lock, since Run's two drain goroutines call
// Output concurrently.
type fakeSink struct {
	mu       sync.Mutex
	command  []string
	contract *millv1.Check
	chunks   []struct{ stream, data string }
}

func (s *fakeSink) Started(command []string, contract *millv1.Check) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.command, s.contract = command, contract
}

func (s *fakeSink) Output(stream string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chunks = append(s.chunks, struct{ stream, data string }{stream, string(data)})
}

// stream is all the data seen for stream so far, in the order it arrived.
func (s *fakeSink) stream(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out strings.Builder
	for _, c := range s.chunks {
		if c.stream == name {
			out.WriteString(c.data)
		}
	}
	return out.String()
}

var _ = Describe("A check's Sink", Label("unit"), func() {
	var tree *checkout

	BeforeEach(func() { tree = newCheckout() })

	It("records the resolved command and the contract before the process runs", func() {
		sink := &fakeSink{}
		contract := &millv1.Check{
			Command: []string{"sh", "-c", "exit 0"},
			Stdout:  millv1.StdoutShape_STDOUT_SHAPE_NONE,
		}

		_, err := check.Run(context.Background(), tree, check.Request{
			Name: "probe", Check: contract, WorkflowDir: GinkgoT().TempDir(), Sink: sink,
		})

		Expect(err).NotTo(HaveOccurred())
		Expect(sink.command).To(Equal(contract.GetCommand()))
		Expect(sink.contract).To(Equal(contract))
	})

	It("streams output to the sink while Run is still draining, not only after it returns", func() {
		sink := &fakeSink{}
		done := make(chan check.Result, 1)
		go func() {
			defer GinkgoRecover()
			result, err := check.Run(context.Background(), tree, check.Request{
				Name: "slow",
				Check: &millv1.Check{
					Command: []string{"sh", "-c", "echo one; sleep 0.2; echo two"},
					Stdout:  millv1.StdoutShape_STDOUT_SHAPE_TEXT,
				},
				WorkflowDir: GinkgoT().TempDir(),
				Sink:        sink,
			})
			Expect(err).NotTo(HaveOccurred())
			done <- result
		}()

		Eventually(func() string { return sink.stream("stdout") }).Should(Equal("one\n"))
		Consistently(done, 100*time.Millisecond).ShouldNot(Receive(),
			"the first line reached the sink before the command's sleep, so Run was still draining")

		var result check.Result
		Eventually(done, 2*time.Second).Should(Receive(&result))
		Expect(result.Stdout).To(Equal("one\ntwo\n"))
		Expect(sink.stream("stdout")).To(Equal("one\ntwo\n"))
	})
})
