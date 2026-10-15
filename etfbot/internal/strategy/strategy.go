// Package strategy turns price history into target portfolio weights. The same
// Strategy value drives the backtest and the live bot, so the two cannot drift.
package strategy

import (
	"time"

	"etfbot/internal/market"
)

// Strategy decides what to hold. Targets sees only bars dated on or before
// asOf and returns weights in [0,1] summing to at most 1; the remainder is cash.
type Strategy interface {
	Name() string
	Symbols() []string // every symbol Targets may need history for
	Targets(asOf time.Time, hist map[string][]market.Bar) (map[string]float64, error)
}

// Fixed holds constant weights: the buy-and-hold and 60/40 benchmarks.
type Fixed struct {
	Label   string
	Weights map[string]float64
}

func (f Fixed) Name() string { return f.Label }

func (f Fixed) Symbols() []string {
	out := make([]string, 0, len(f.Weights))
	for s := range f.Weights {
		out = append(out, s)
	}
	return out
}

func (f Fixed) Targets(time.Time, map[string][]market.Bar) (map[string]float64, error) {
	return f.Weights, nil
}
