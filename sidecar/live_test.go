package sidecar_test

import (
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	harnessv1 "github.com/ivy/mill/gen/harness/v1"
)

// The live specs spend a real session against the Claude Agent SDK, using
// the credentials of the account the sidecar runs under — never anything
// from the request. They are skipped unless MILL_SIDECAR_LIVE is set,
// because they cost tokens and need a login; everything they cover about the
// sidecar's own behaviour is covered deterministically in sidecar_test.go.
// What they add is the part no fake can assert: that the SDK really does
// what docs/spikes/sdk-sessions.md observed it doing.
var _ = Describe("A live session", Label("live"), Serial, func() {
	const liveTimeout = 5 * time.Minute

	var (
		sessionID string
		workdir   string
	)

	BeforeEach(func() {
		live()
		sessionID = newSessionID()
		workdir = GinkgoT().TempDir()
	})

	It("runs a trivial prompt to a terminal record", func() {
		By("given a Start request with a trivial prompt and no tools")
		line := request(&harnessv1.Request{
			Id: "live-trivial",
			Body: &harnessv1.Request_Start{Start: &harnessv1.Start{
				SessionId: sessionID,
				Prompt:    "Reply with exactly the word READY and nothing else.",
				Cwd:       workdir,
				Settings:  &harnessv1.Settings{ToolPolicy: &harnessv1.ToolPolicy{Deny: []string{"Bash", "Write", "Edit"}}},
			}},
		})

		By("when the sidecar runs it")
		s := runFor(entrypoint, nil, line, liveTimeout)

		By("then it wrote at least one event and exactly one terminal record, and exited 0")
		Expect(len(s.events())).To(BeNumerically(">=", 1))
		Expect(s.terminals()).To(HaveLen(1))
		Expect(s.exitCode()).To(Equal(0))
		Expect(s.terminal().GetExit().GetStatus()).To(BeZero())
		Expect(s.terminal().GetFinalMessage()).To(ContainSubstring("READY"))

		By("and the usage the SDK reported reached the wire, keyed by model")
		Expect(s.terminal().GetUsage().GetByModel()).NotTo(BeEmpty())
		for model, counts := range s.terminal().GetUsage().GetByModel() {
			Expect(model).NotTo(BeEmpty())
			Expect(counts.GetInput() + counts.GetOutput() + counts.GetCacheRead() + counts.GetCacheWrite()).
				To(BeNumerically(">", 0))
		}
	})

	It("resumes the same session from a fresh process and sees the prior turn", func() {
		By("given a session that has been told a codeword")
		first := request(&harnessv1.Request{
			Id: "live-start",
			Body: &harnessv1.Request_Start{Start: &harnessv1.Start{
				SessionId: sessionID,
				Prompt:    "Remember this codeword: TANGERINE-4417. Reply with OK and nothing else.",
				Cwd:       workdir,
				Settings:  &harnessv1.Settings{ToolPolicy: &harnessv1.ToolPolicy{Deny: []string{"Bash", "Write", "Edit"}}},
			}},
		})
		started := runFor(entrypoint, nil, first, liveTimeout)
		Expect(started.terminal().GetExit().GetStatus()).To(BeZero())

		By("when a fresh process resumes it by the same session id")
		second := request(&harnessv1.Request{
			Id: "live-resume",
			Body: &harnessv1.Request_Resume{Resume: &harnessv1.Resume{
				SessionId: sessionID,
				Prompt:    "What codeword did I give you earlier? Reply with the codeword and nothing else.",
				Cwd:       workdir,
				Settings:  &harnessv1.Settings{ToolPolicy: &harnessv1.ToolPolicy{Deny: []string{"Bash", "Write", "Edit"}}},
			}},
		})
		resumed := runFor(entrypoint, nil, second, liveTimeout)

		By("then the turn references the prior turn's content")
		Expect(resumed.terminal().GetExit().GetStatus()).To(BeZero())
		Expect(resumed.terminal().GetFinalMessage()).To(ContainSubstring("TANGERINE-4417"))
	})

	It("applies a policy that excludes Write and Edit", func() {
		By("given a step whose policy withholds every write path")
		target := filepath.Join(workdir, "escape.txt")
		line := request(&harnessv1.Request{
			Id: "live-policy",
			Body: &harnessv1.Request_Start{Start: &harnessv1.Start{
				SessionId: sessionID,
				Prompt:    "Create the file " + target + " with the exact contents ESCAPED. If you cannot, say why.",
				Cwd:       workdir,
				Settings: &harnessv1.Settings{ToolPolicy: &harnessv1.ToolPolicy{
					Allow: []string{"Read", "Glob", "Grep"},
					Deny:  []string{"Write", "Edit", "NotebookEdit", "Bash", "Task"},
				}},
			}},
		})

		By("when the session is asked to write a file")
		s := runFor(entrypoint, nil, line, liveTimeout)

		By("then the file was not written and the session never called a write tool")
		_, err := os.Stat(target)
		Expect(os.IsNotExist(err)).To(BeTrue(), "the policy did not stop the write")
		for _, event := range s.events() {
			if call := event.GetToolCall(); call != nil {
				Expect(call.GetName()).NotTo(BeElementOf("Write", "Edit", "NotebookEdit", "Bash"))
			}
		}

		By("and the transcript shows the refusal")
		GinkgoWriter.Println(s.terminal().GetFinalMessage())
		Expect(s.terminal().GetFinalMessage()).To(MatchRegexp(`(?i)can'?t|cannot|unable|not (available|permitted|allowed)|no write|denied|polic`))
	})

	It("carries structured output that validates against the step's schema", func() {
		By("given a step that declares a reviewer's output schema")
		line := request(&harnessv1.Request{
			Id: "live-structured",
			Body: &harnessv1.Request_Start{Start: &harnessv1.Start{
				SessionId:    sessionID,
				Prompt:       "Judge whether the criterion 'the CLI exits non-zero on an unknown flag' holds. You have no code to read; decide from the statement alone and answer in the required format.",
				Cwd:          workdir,
				OutputSchema: reviewSchema,
				Settings:     &harnessv1.Settings{ToolPolicy: &harnessv1.ToolPolicy{Deny: []string{"Bash", "Write", "Edit"}}},
			}},
		})

		By("when the session ends")
		s := runFor(entrypoint, nil, line, liveTimeout)

		By("then the terminal record's structured output validates against the schema")
		Expect(s.terminal().GetExit().GetStatus()).To(BeZero())
		output := structuredOutput(s.terminal())
		Expect(output["decision"]).To(BeElementOf("approve", "revise", "needs_human"))
		Expect(output["findings"]).To(BeAssignableToTypeOf([]any{}))
		summary, ok := output["summary"].(map[string]any)
		Expect(ok).To(BeTrue())
		Expect(summary["headline"]).To(BeAssignableToTypeOf(""))
		Expect(summary["confidence"]).To(BeAssignableToTypeOf(float64(0)))
	})
})
