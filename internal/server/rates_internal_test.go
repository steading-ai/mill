package server

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
)

var _ = Describe("price", func() {
	It("prices each model in a total by its own rate", func() {
		totals := []*millv1.ModelTotals{
			{Model: "sonnet", InputTokens: 1_000_000, OutputTokens: 500_000, CacheReadTokens: 2_000_000, CacheWriteTokens: 250_000},
			{Model: "opus", InputTokens: 100_000, OutputTokens: 100_000},
		}
		rates := Rates{
			"sonnet": {Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75},
			"opus":   {Input: 15, Output: 75},
		}

		price(totals, rates)

		Expect(totals[0].GetPriced()).To(BeTrue())
		Expect(totals[0].GetUsd()).To(BeNumerically("~", 3*1+15*0.5+0.3*2+3.75*0.25, 0.0001))
		Expect(totals[1].GetPriced()).To(BeTrue())
		Expect(totals[1].GetUsd()).To(BeNumerically("~", 15*0.1+75*0.1, 0.0001))
	})

	It("leaves a model absent from the table unpriced rather than free", func() {
		totals := []*millv1.ModelTotals{
			{Model: "unknown-model", InputTokens: 1_000_000, OutputTokens: 1_000_000},
		}

		price(totals, Rates{"sonnet": {Input: 3, Output: 15}})

		Expect(totals[0].GetPriced()).To(BeFalse())
		Expect(totals[0].GetUsd()).To(Equal(0.0))
	})
})
