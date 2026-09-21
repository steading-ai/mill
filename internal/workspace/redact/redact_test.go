package redact_test

import (
	"bytes"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/structpb"

	millv1 "github.com/ivy/mill/gen/mill/v1"
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

	It("redacts a value nested inside a google.protobuf.Struct field", func() {
		redactor := redact.New("struct-secret-value")
		arguments, err := structpb.NewStruct(map[string]any{
			"path":  "value=struct-secret-value",
			"count": float64(3),
		})
		Expect(err).NotTo(HaveOccurred())
		call := &millv1.ToolCall{CallId: "1", Name: "read", Arguments: arguments}

		redactor.Message(call)

		Expect(call.GetArguments().GetFields()["path"].GetStringValue()).To(Equal("value=" + redact.Mask))
		Expect(call.GetArguments().GetFields()["count"].GetNumberValue()).To(Equal(float64(3)))
	})

	Describe("Chunker", func() {
		It("holds a value split across two writes back until the chunk that completes it", func() {
			chunker := redact.NewChunker(redact.New("chunk-secret-value"))

			Expect(chunker.Write([]byte("token=chunk-"))).To(BeNil())
			Expect(chunker.Write([]byte("secret-value done\n"))).To(Equal([]byte("token=" + redact.Mask + " done\n")))
		})

		It("flushes a trailing chunk that never got its newline", func() {
			chunker := redact.NewChunker(redact.New("chunk-secret-value"))

			Expect(chunker.Write([]byte("token=chunk-secret-value"))).To(BeNil())
			Expect(chunker.Close()).To(Equal([]byte("token=" + redact.Mask)))
		})
	})

	Describe("DeltaHoldback", func() {
		It("never shows a value split across two deltas in an intermediate release", func() {
			holdback := redact.NewDeltaHoldback(redact.New("delta-secret-value"))

			first := holdback.Add("block-1", "token=delta-", false)
			Expect(first).To(BeEmpty(), "the partial secret is held back, not released early")

			// Enough trailing text follows the secret to push it out of the
			// hold window, so this call's release proves the redaction, not
			// just the next one.
			second := holdback.Add("block-1", "secret-value and then quite a bit more text after it", false)
			Expect(first + second).NotTo(ContainSubstring("delta-secret-value"))
			Expect(first + second).To(ContainSubstring(redact.Mask))
		})

		It("releases everything held on final", func() {
			holdback := redact.NewDeltaHoldback(redact.New("delta-secret-value"))

			first := holdback.Add("block-1", "token=delta-", false)
			last := holdback.Add("block-1", "secret-value", true)

			Expect(first + last).To(Equal("token=" + redact.Mask))
		})

		It("releases a delta immediately when nothing is registered to hold back for", func() {
			holdback := redact.NewDeltaHoldback(redact.New())

			first := holdback.Add("block-1", "Hello", false)
			second := holdback.Add("block-1", ", world!", false)

			Expect(first).To(Equal("Hello"), "no registered value means no boundary to protect, so the delta streams straight through")
			Expect(second).To(Equal(", world!"))
		})
	})
})
