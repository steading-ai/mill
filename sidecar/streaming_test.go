package sidecar_test

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	harnessv1 "github.com/steading-ai/mill/gen/harness/v1"
)

// The events a session's text, thinking and subagent activity forward as
// under includePartialMessages/forwardSubagentText: deltas as they arrive,
// a parent_call_id folding a subagent's own events under the Agent call
// that started it, and a tool result forwarded with no limit of the
// sidecar's own (docs/activity.md §"Streaming text", §Subagents).
var _ = Describe("Streaming", Label("unit"), func() {
	It("forwards a text block's deltas, then an empty final record with the same block id", func() {
		s := run(fakeEntrypoint, map[string]string{"MILL_FAKE_SCENARIO": "streamed-text"}, startRequest("stream-text-1", nil))

		var texts []*harnessv1.Text
		for _, event := range s.events() {
			if text := event.GetText(); text != nil {
				texts = append(texts, text)
			}
		}
		Expect(texts).To(HaveLen(3), "two deltas and the closing final record")

		var whole strings.Builder
		blockID := texts[0].GetBlockId()
		Expect(blockID).NotTo(BeEmpty())
		for _, text := range texts[:len(texts)-1] {
			Expect(text.GetBlockId()).To(Equal(blockID))
			Expect(text.GetDelta()).To(BeTrue())
			Expect(text.GetFinal()).To(BeFalse())
			whole.WriteString(text.GetText())
		}
		Expect(whole.String()).To(Equal("Hello, world!"), "every delta's text concatenates to the block's whole text exactly once")

		final := texts[len(texts)-1]
		Expect(final.GetBlockId()).To(Equal(blockID))
		Expect(final.GetFinal()).To(BeTrue())
		Expect(final.GetText()).To(BeEmpty(), "the closing record carries no text of its own")
	})

	It("forwards a thinking block's deltas the same way as text", func() {
		s := run(fakeEntrypoint, map[string]string{"MILL_FAKE_SCENARIO": "streamed-thinking"}, startRequest("stream-thinking-1", nil))

		var thoughts []*harnessv1.Thinking
		for _, event := range s.events() {
			if thinking := event.GetThinking(); thinking != nil {
				thoughts = append(thoughts, thinking)
			}
		}
		Expect(thoughts).To(HaveLen(3))

		var whole strings.Builder
		for _, thinking := range thoughts[:len(thoughts)-1] {
			Expect(thinking.GetDelta()).To(BeTrue())
			whole.WriteString(thinking.GetText())
		}
		Expect(whole.String()).To(Equal("weighing the options"))

		final := thoughts[len(thoughts)-1]
		Expect(final.GetFinal()).To(BeTrue())
		Expect(final.GetText()).To(BeEmpty())
	})

	It("folds a subagent's text, calls, results and usage under the Agent call that started it", func() {
		s := run(fakeEntrypoint, map[string]string{"MILL_FAKE_SCENARIO": "subagent"}, startRequest("subagent-1", nil))

		events := s.events()
		var agentCallID string
		for _, event := range events {
			if call := event.GetToolCall(); call != nil && call.GetName() == "Agent" {
				agentCallID = call.GetCallId()
			}
		}
		Expect(agentCallID).NotTo(BeEmpty())

		var subagentEvents, topLevelEvents int
		var sawSubagentText, sawSubagentToolCall, sawSubagentToolResult, sawSubagentUsage bool
		for _, event := range events {
			switch {
			case event.GetParentCallId() == agentCallID:
				subagentEvents++
				switch body := event.GetBody().(type) {
				case *harnessv1.Event_Text:
					sawSubagentText = true
				case *harnessv1.Event_ToolCall:
					if body.ToolCall.GetName() == "Read" {
						sawSubagentToolCall = true
					}
				case *harnessv1.Event_ToolResult:
					if body.ToolResult.GetCallId() == "toolu_sub_read" {
						sawSubagentToolResult = true
					}
				case *harnessv1.Event_Usage:
					sawSubagentUsage = true
				}
			default:
				topLevelEvents++
				// The Agent call itself, and the tool_result that answers
				// it, belong to the top-level turn, not the subagent's.
				if result := event.GetToolResult(); result != nil {
					Expect(result.GetCallId()).NotTo(Equal("toolu_sub_read"))
				}
			}
		}
		Expect(sawSubagentText).To(BeTrue(), "the subagent's own text carries the Agent call's id")
		Expect(sawSubagentToolCall).To(BeTrue(), "a tool the subagent called carries the Agent call's id")
		Expect(sawSubagentToolResult).To(BeTrue(), "the result of a tool the subagent called carries the Agent call's id")
		Expect(sawSubagentUsage).To(BeTrue(), "the subagent's own turn's usage carries the Agent call's id")
		Expect(topLevelEvents).NotTo(BeZero(), "the top-level session's own events carry no parent_call_id")
	})

	It("records a subagent still open when the turn that started it ends", func() {
		s := run(fakeEntrypoint, map[string]string{"MILL_FAKE_SCENARIO": "subagent-open"}, startRequest("subagent-open-1", nil))

		Expect(s.terminal().GetFinalMessage()).To(ContainSubstring("subagent call(s) still open"))
		Expect(s.terminal().GetFinalMessage()).To(ContainSubstring("toolu_agent_open"))
	})

	It("forwards a several-hundred-KB tool result whole, with no limit of its own", func() {
		s := run(fakeEntrypoint, map[string]string{"MILL_FAKE_SCENARIO": "large-result"}, startRequest("large-result-1", nil))

		var found bool
		for _, event := range s.events() {
			if result := event.GetToolResult(); result != nil && result.GetCallId() == "toolu_large" {
				found = true
				Expect(result.GetContent()).To(HaveLen(300 * 1024))
			}
		}
		Expect(found).To(BeTrue())
	})
})
