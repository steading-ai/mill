package contract_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("harness.proto", func() {
	var c contract

	BeforeEach(func() {
		c = parse(harnessProto)
		Expect(c.messages).NotTo(BeEmpty())
	})

	It("defines the requests, the events, and the terminal record", func() {
		for _, m := range []string{"Start", "Resume", "Cancel", "Event", "Terminal",
			"Usage", "TokenCounts", "ProcessExit"} {
			Expect(c.messages).To(HaveKey(m))
		}

		// An Event is one of the four things a turn produces.
		Expect(c.field("Event", "body").typ).To(Equal("oneof"))
		for _, member := range []string{"text", "tool_call", "tool_result", "usage"} {
			_, ok := c.lookup("Event", member)
			Expect(ok).To(BeTrue(), "Event has no %s member", member)
		}

		// A Terminal is the final message, the structured output, the usage,
		// and how the process ended.
		Expect(c.field("Terminal", "final_message").typ).To(Equal("string"))
		Expect(c.field("Terminal", "structured_output").typ).To(Equal("google.protobuf.Struct"))
		Expect(c.field("Terminal", "usage").typ).To(Equal("Usage"))
		Expect(c.field("Terminal", "exit").typ).To(Equal("ProcessExit"))
	})

	It("keys usage by model name with four integer token counts", func() {
		Expect(c.field("Usage", "by_model").typ).To(Equal("map<string, TokenCounts>"))

		for _, name := range []string{"input", "output", "cache_read", "cache_write"} {
			f := c.field("TokenCounts", name)
			Expect(f.typ).To(Equal("int64"), "TokenCounts.%s", name)
			Expect(f.comment).NotTo(BeEmpty(), "TokenCounts.%s", name)
		}

		// Counts, never currency: nothing on the wire prices a token.
		for _, f := range c.fields {
			Expect(f.name).NotTo(ContainSubstring("cost"), "%s.%s", f.message, f.name)
			Expect(f.name).NotTo(ContainSubstring("price"), "%s.%s", f.message, f.name)
		}
	})

	It("documents a status of -1 as never ran", func() {
		comment := c.field("ProcessExit", "status").comment
		Expect(comment).To(ContainSubstring("-1"))
		Expect(comment).To(ContainSubstring("never ran"))
		Expect(comment).To(ContainSubstring("reason"))

		// The reason enum is what "never ran" is explained with.
		Expect(c.enums).To(HaveKey("ExitReason"))
		var names []string
		for _, v := range c.enums["ExitReason"] {
			names = append(names, v.name)
		}
		Expect(names).To(ConsistOf(
			"EXIT_REASON_UNSPECIFIED", "EXIT_REASON_REFUSED", "EXIT_REASON_POLICY",
			"EXIT_REASON_TIMEOUT", "EXIT_REASON_CANCELLED", "EXIT_REASON_STALLED",
			"EXIT_REASON_UPSTREAM_REFUSED"))
	})

	It("records that harness settings come from the step", func() {
		Expect(c.messages["Settings"]).To(ContainSubstring("they are on the step"))
	})
})
