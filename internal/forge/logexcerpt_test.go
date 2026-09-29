package forge_test

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/mill/internal/forge"
)

var _ = Describe("FilterLog", Label("unit"), func() {
	It("returns empty when no line names a failure", func() {
		log := "Setting up job\nRunning tests\nAll good\nCleaning up\n"

		Expect(forge.FilterLog([]byte(log))).To(Equal(""))
	})

	It("passes a small log through whole when every line is near a match", func() {
		log := "Error: something broke\nsee above\n"

		Expect(forge.FilterLog([]byte(log))).To(Equal("Error: something broke\nsee above"))
	})

	It("keeps only the lines around a match out of a log full of noise", func() {
		var lines []string
		for i := 0; i < 100; i++ {
			lines = append(lines, "noise line that says nothing")
		}
		lines[50] = "run-verbs.spec.ts:67"
		lines[51] = "Error: expect(locator).toBeDisabled() failed"
		log := strings.Join(lines, "\n")

		excerpt := forge.FilterLog([]byte(log))

		Expect(excerpt).To(ContainSubstring("run-verbs.spec.ts:67"))
		Expect(excerpt).To(ContainSubstring("Error: expect(locator).toBeDisabled() failed"))
		Expect(strings.Count(excerpt, "noise line that says nothing")).To(BeNumerically("<", 10))
	})

	It("cuts a filtered excerpt that still exceeds the cap down to its tail, on a rune boundary", func() {
		var lines []string
		for i := 0; i < 4000; i++ {
			lines = append(lines, "Error: repeated failure line number padding the log out lëngthwise")
		}
		log := strings.Join(lines, "\n")

		excerpt := forge.FilterLog([]byte(log))

		Expect(len(excerpt)).To(BeNumerically("<=", forge.MaxExcerptBytes))
		Expect(excerpt).To(HaveSuffix("lëngthwise"))
		Expect(excerpt).NotTo(ContainSubstring("�"), "must not cut a multi-byte rune in half")
	})
})
