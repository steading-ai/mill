package server

import millv1 "github.com/ivy/mill/gen/mill/v1"

// Rate is what a model costs, in dollars per million tokens, split by how a
// token was spent.
type Rate struct {
	Input      float64
	Output     float64
	CacheRead  float64
	CacheWrite float64
}

// Rates is the table mill applies at query time, keyed by resolved model id
// (docs/ledger.md §usage: "Counts, never currency ... Cost is a rate table
// applied at query time").
type Rates map[string]Rate

// price sets Usd and Priced on every entry of totals, from rates. A model
// absent from rates is left unpriced (usd 0, priced false) rather than
// priced at zero, so "mill has no rate for this" is never mistaken for "this
// model is free."
func price(totals []*millv1.ModelTotals, rates Rates) {
	for _, t := range totals {
		rate, ok := rates[t.GetModel()]
		if !ok {
			continue
		}
		t.Usd = float64(t.GetInputTokens())*rate.Input/1e6 +
			float64(t.GetOutputTokens())*rate.Output/1e6 +
			float64(t.GetCacheReadTokens())*rate.CacheRead/1e6 +
			float64(t.GetCacheWriteTokens())*rate.CacheWrite/1e6
		t.Priced = true
	}
}
