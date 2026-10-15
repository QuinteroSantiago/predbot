package strategy

import (
	"math"
	"testing"
	"time"

	"etfbot/internal/market"
)

// series is n daily bars growing at rate per day from 100.
func series(n int, rate float64) []market.Bar {
	out := make([]market.Bar, n)
	d := market.Date(2020, 1, 1)
	px := 100.0
	for i := range out {
		out[i] = market.Bar{Date: d.AddDate(0, 0, i), Close: px}
		px *= 1 + rate
	}
	return out
}

func TestRotationPicksStrongestInUptrend(t *testing.T) {
	r := Rotation{Universe: []string{"A", "B", "C", "D"}, Cash: "BIL", TopN: 2, Lookbacks: []int{20, 40}, SMA: 50}
	hist := map[string][]market.Bar{
		"A": series(60, 0.003), "B": series(60, 0.002), "C": series(60, 0.001),
		"D": series(60, -0.002), "BIL": series(60, 0.0001),
	}
	got, err := r.Targets(hist["A"][59].Date, hist)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["A"] != 0.5 || got["B"] != 0.5 {
		t.Fatalf("targets = %v; want A and B at 0.5", got)
	}
}

func TestRotationFallsBackToCashInDowntrend(t *testing.T) {
	r := Rotation{Universe: []string{"A", "B"}, Cash: "BIL", TopN: 2, Lookbacks: []int{20}, SMA: 50}
	hist := map[string][]market.Bar{
		"A": series(60, 0.002), "B": series(60, -0.001), "BIL": series(60, 0.0001),
	}
	got, _ := r.Targets(hist["A"][59].Date, hist)
	if got["A"] != 0.5 || got["BIL"] != 0.5 || len(got) != 2 {
		t.Fatalf("targets = %v; want A 0.5, BIL 0.5", got)
	}
}

func TestRotationIgnoresFutureBarsAndShortHistory(t *testing.T) {
	r := Rotation{Universe: []string{"A", "NEW"}, Cash: "BIL", TopN: 1, Lookbacks: []int{20}, SMA: 50}
	a := series(60, 0.001)
	// NEW has too little history as of day 59, and a huge gain after it that
	// Targets must not see.
	fut := series(80, 0.05)[50:]
	hist := map[string][]market.Bar{"A": a, "NEW": fut, "BIL": series(60, 0)}
	got, _ := r.Targets(a[59].Date, hist)
	if math.Abs(got["A"]-1) > 1e-12 {
		t.Fatalf("targets = %v; want all in A", got)
	}
}

func TestRotationNoCashFundYetLeavesSlotEmpty(t *testing.T) {
	r := Rotation{Universe: []string{"A"}, Cash: "BIL", TopN: 1, Lookbacks: []int{20}, SMA: 50}
	hist := map[string][]market.Bar{"A": series(60, -0.001)}
	got, _ := r.Targets(time.Date(2020, 3, 1, 0, 0, 0, 0, time.UTC), hist)
	if len(got) != 0 {
		t.Fatalf("targets = %v; want none (all cash)", got)
	}
}
