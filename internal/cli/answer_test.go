package cli_test

import (
	"bytes"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/cli"
	"github.com/ivy/mill/internal/run"
)

// ask is a filed gate as the terminal is asked to render it.
var ask = run.Ask{
	NodeRef: "mill#16",
	GateRef: "mill#17",
	Title:   "Merge mill#16?",
	Body:    "mill#16 — Gates, the circuit breaker, and restart — is ready to merge.",
}

var _ = Describe("Answering a gate at the terminal", Label("unit"), func() {
	var out bytes.Buffer

	BeforeEach(func() { out.Reset() })

	answer := func(typed string) bool {
		GinkgoHelper()
		approved, err := cli.NewTerminalAnswerer(strings.NewReader(typed), &out).
			Answer(GinkgoT().Context(), ask)
		Expect(err).NotTo(HaveOccurred())
		return approved
	}

	It("shows the ask and the gate it belongs to", func() {
		answer("n\n")
		Expect(out.String()).To(ContainSubstring(ask.GateRef))
		Expect(out.String()).To(ContainSubstring(ask.Title))
		Expect(out.String()).To(ContainSubstring(ask.Body))
	})

	DescribeTable("reads the answer",
		func(typed string, approved bool) {
			Expect(answer(typed)).To(Equal(approved))
		},
		Entry("y approves", "y\n", true),
		Entry("yes approves", "YES\n", true),
		Entry("n denies", "n\n", false),
		Entry("anything else denies", "maybe later\n", false),
		// A gate nobody answered is not a gate that was approved.
		Entry("end of input denies", "", false),
	)
})
