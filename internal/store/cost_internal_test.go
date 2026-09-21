package store

// dayKey is unexported: Store.Cost's day-bucket grouping key, tested here
// against the UTC boundary directly since nothing in the DB-backed suite
// can backdate usage.recorded_at to prove a multi-day case (see
// internal/cli/read_test.go's "buckets by day" spec for the one-day case
// that suite can prove).

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("dayKey", func() {
	It("is the UTC calendar date, so two wall-clock hours on the same day agree", func() {
		morning := time.Date(2026, 9, 21, 0, 0, 1, 0, time.UTC)
		evening := time.Date(2026, 9, 21, 23, 59, 59, 0, time.UTC)
		Expect(dayKey(morning)).To(Equal(dayKey(evening)))
		Expect(dayKey(morning)).To(Equal("2026-09-21"))
	})

	It("splits at the UTC day boundary", func() {
		before := time.Date(2026, 9, 21, 23, 59, 59, 0, time.UTC)
		after := time.Date(2026, 9, 22, 0, 0, 1, 0, time.UTC)
		Expect(dayKey(before)).NotTo(Equal(dayKey(after)))
	})

	It("converts to UTC before keying, so a timestamp in another zone lands where UTC puts it", func() {
		// 2026-09-21 23:30 UTC+1 is 2026-09-21 22:30 UTC: still the 21st.
		loc := time.FixedZone("UTC+1", 60*60)
		local := time.Date(2026, 9, 21, 23, 30, 0, 0, loc)
		Expect(dayKey(local)).To(Equal("2026-09-21"))
	})
})
