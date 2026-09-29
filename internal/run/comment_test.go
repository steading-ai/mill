package run

// Tests commentCreatedAt's ULID-prefix decode against known-good vectors
// and its best-effort fallback on malformed input.

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("commentCreatedAt", Label("unit"), func() {
	It("decodes a comment id's leading ULID timestamp", func() {
		cases := map[string]time.Time{
			"0000000000":                 time.UnixMilli(0).UTC(),
			"0000000001":                 time.UnixMilli(1).UTC(),
			"01F4QRCJ000000000000000000": time.UnixMilli(1620000000000).UTC(),
			"01JGFJJZ3V0000000000000000": time.UnixMilli(1735689600123).UTC(),
			"7ZZZZZZZZZ0000000000000000": time.UnixMilli(281474976710655).UTC(),
		}
		for id, want := range cases {
			Expect(commentCreatedAt(id)).To(Equal(want), "id %s", id)
		}
	})

	It("yields the zero time for an id too short to carry a timestamp, without panicking", func() {
		Expect(commentCreatedAt("01F4QRC")).To(Equal(time.Time{}))
		Expect(commentCreatedAt("")).To(Equal(time.Time{}))
	})

	It("yields the zero time for a character outside the Crockford alphabet, without panicking", func() {
		Expect(commentCreatedAt("01F4QRCIOU")).To(Equal(time.Time{}), "I, L, O, U are not in the alphabet")
	})
})
