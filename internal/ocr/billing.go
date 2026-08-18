package ocr

import (
	"fmt"
	"time"
)

// Currency is stored as billionths of a US dollar. That precision represents
// every baked-in per-token and per-page price exactly without concurrent float
// arithmetic.
type Currency int64

const (
	nanosPerDollar  Currency = 1_000_000_000
	mistralPageCost Currency = 5_000_000
)

type BillingReport struct {
	KnownCost     Currency
	Indeterminate bool
}

func (r *BillingReport) Add(other BillingReport) {
	r.KnownCost += other.KnownCost
	r.Indeterminate = r.Indeterminate || other.Indeterminate
}

func FormatCurrency(cost Currency) string {
	if cost < 0 {
		return "-" + FormatCurrency(-cost)
	}

	// Four decimal places are the user-facing billing precision. Round once at
	// the end so aggregating many small token charges remains exact.
	units := (cost + 50_000) / 100_000
	return fmt.Sprintf("$%d.%04d", units/10_000, units%10_000)
}

func MistralPageCost() Currency {
	return mistralPageCost
}

func MistralCost(pages int) Currency {
	return Currency(pages) * mistralPageCost
}

type GeminiTokenPrices struct {
	Input  Currency
	Output Currency
}

// Gemini's standard paid rates change at the documented UTC pricing cutover.
// Keep estimates and actual usage on the run's single schedule even if workers
// cross midnight. Recheck against https://ai.google.dev/gemini-api/docs/pricing.
func GeminiPrices(at time.Time) GeminiTokenPrices {
	if !at.UTC().Before(time.Date(2027, time.January, 1, 0, 0, 0, 0, time.UTC)) {
		return GeminiTokenPrices{Input: 1_500, Output: 7_500}
	}
	return GeminiTokenPrices{Input: 750, Output: 3_750}
}

func GeminiCost(at time.Time, inputTokens, outputTokens int64) Currency {
	prices := GeminiPrices(at)
	return Currency(inputTokens)*prices.Input + Currency(outputTokens)*prices.Output
}
