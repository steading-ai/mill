package scripted_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/mill/internal/harness"
	"github.com/steading-ai/mill/internal/harness/scripted"
)

var _ = Describe("Loading a script", Label("unit"), func() {
	It("decodes every event kind, a terminal and a dispatching tool_call from YAML", func() {
		script, err := scripted.Load("testdata/turns.yaml")
		Expect(err).NotTo(HaveOccurred())
		Expect(script).To(HaveLen(3))

		first := script[0]
		Expect(first.Events).To(HaveLen(5))
		Expect(first.Events[0].GetText().GetText()).To(Equal("gathering context"))
		Expect(first.Events[1].GetToolCall().GetName()).To(Equal("Read"))
		Expect(first.Events[2].GetToolResult().GetContent()).To(Equal("# mill"))
		Expect(harness.UsageFrom(first.Events[3].GetUsage())).To(Equal(harness.Usage{
			"claude-opus-5": {Input: 100, Output: 20, CacheRead: 5, CacheWrite: 1},
		}))
		Expect(first.Events[4].GetHeartbeat().GetTasks()).To(Equal([]string{"reading the repo"}))
		Expect(first.Terminal.FinalMessage).To(Equal("gathered"))
		Expect(first.Dispatches).To(BeEmpty())
		Expect(first.Delays).To(HaveKeyWithValue(0, 300*time.Millisecond),
			"the first event's after decodes to a wait keyed by its own index")
		Expect(first.TerminalAfter).To(Equal(5 * time.Second))

		second := script[1]
		Expect(second.Events).To(HaveLen(1))
		Expect(second.Dispatches).To(HaveKeyWithValue(0, scripted.Dispatch{
			Verb:      "fs.write",
			Arguments: map[string]any{"path": "${artifact_path}", "content": "# Plan"},
		}))

		third := script[2]
		Expect(third.Events).To(BeEmpty())
		Expect(third.Terminal.Exit).To(Equal(harness.ProcessExit{Status: -1, Reason: harness.ReasonRefused}))
	})

	It("decodes JSON by extension", func() {
		script, err := scripted.Load("testdata/turns.json")
		Expect(err).NotTo(HaveOccurred())
		Expect(script).To(HaveLen(1))
		Expect(script[0].Terminal.StructuredOutput).To(HaveKeyWithValue("decision", "approve"))
	})

	It("refuses an event kind it does not know", func() {
		_, err := scripted.Load("testdata/unknown-kind.yaml")
		Expect(err).To(MatchError(ContainSubstring(`kind "sings"`)))
	})

	It("refuses a dispatch verb outside the fixed list", func() {
		_, err := scripted.Load("testdata/unknown-dispatch.yaml")
		Expect(err).To(MatchError(ContainSubstring(`dispatch "forge.pr"`)))
	})

	It("refuses an after that is not a valid duration, naming the field", func() {
		_, err := scripted.Load("testdata/bad-after.yaml")
		Expect(err).To(MatchError(ContainSubstring(`after "5x"`)))
	})
})
