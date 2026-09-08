package redact_test

import (
	"bytes"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/workspace/redact"
)

var _ = Describe("The redactor", Label("unit"), func() {
	It("scrubs a seeded value out of a line", func() {
		redactor := redact.New("s3cret-bearer-value")

		Expect(redactor.String("token=s3cret-bearer-value ok")).To(Equal("token=" + redact.Mask + " ok"))
	})

	It("scrubs a value registered after the redactor was made", func() {
		redactor := redact.New()
		Expect(redactor.String("forge-token-abcdef")).To(Equal("forge-token-abcdef"))

		redactor.Add("forge-token-abcdef")

		Expect(redactor.String("forge-token-abcdef")).To(Equal(redact.Mask))
	})

	It("ignores a value too short to be a secret", func() {
		redactor := redact.New("ab")

		Expect(redactor.Values()).To(BeEmpty())
		Expect(redactor.String("ab")).To(Equal("ab"))
	})

	It("masks the longest match, so a value containing another leaves no remainder", func() {
		redactor := redact.New("bearer", "bearer-and-more")

		Expect(redactor.String("bearer-and-more")).To(Equal(redact.Mask))
	})

	It("holds a partial line back until its newline, so a value split across writes is still scrubbed", func() {
		var log bytes.Buffer
		writer := redact.New("split-secret-value").Writer(&log)

		Expect(writer.Write([]byte("token=split-"))).To(Equal(12))
		Expect(log.String()).To(BeEmpty())
		Expect(writer.Write([]byte("secret-value done\n"))).To(Equal(18))

		Expect(log.String()).To(Equal("token=" + redact.Mask + " done\n"))
	})

	It("flushes a trailing line that never got its newline", func() {
		var log bytes.Buffer
		writer := redact.New("split-secret-value").Writer(&log)

		Expect(writer.Write([]byte("token=split-secret-value"))).To(Equal(24))
		Expect(writer.Close()).To(Succeed())

		Expect(log.String()).To(Equal("token=" + redact.Mask))
	})
})
