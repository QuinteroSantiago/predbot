package backtest

import (
	"math"
	"testing"
	"time"

	"etfbot/internal/market"
	"etfbot/internal/rebalance"
	"etfbot/internal/strategy"
)

// days returns n consecutive calendar days from 2020-01-01 as trading days.
func days(n int) []time.Time {
	out := make([]time.Time, n)
	for i := range out {
		out[i] = market.Date(2020, 1, 1).AddDate(0, 0, i)
	}
	return out
}

func bars(cal []time.Time, px func(i int) float64) []market.Bar {
	out := make([]market.Bar, len(cal))
	for i, d := range cal {
		out[i] = market.Bar{Date: d, Close: px(i)}
	}
	return out
}

func TestBuyAndHoldTracksPriceMinusSlippage(t *testing.T) {
	cal := days(400)
	hist := map[string][]market.Bar{"SPY": bars(cal, func(i int) float64 { return 100 + float64(i) })}
	cfg := Config{Start: cal[0], End: cal[len(cal)-1], Cash: 10000, SlippageBps: 10}
	r, err := Run(strategy.Fixed{Label: "spy", Weights: map[string]float64{"SPY": 1}}, hist, cal, cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Bought on 2020-01-31 (i=30, px 130) with 10bp slippage; first month is cash.
	shares := 10000 / (130 * 1.001)
	want := shares * 499
	if got := r.Curve[len(r.Curve)-1].Equity; math.Abs(got-want) > 0.01 {
		t.Fatalf("final equity = %.2f; want %.2f", got, want)
	}
	if r.Curve[0].Equity != 10000 {
		t.Fatal("equity before the first month end must be the starting cash")
	}
}

func TestRebalanceRestoresWeights(t *testing.T) {
	cal := days(70)
	hist := map[string][]market.Bar{
		"A": bars(cal, func(i int) float64 { return 100 * math.Pow(1.01, float64(i)) }),
		"B": bars(cal, func(int) float64 { return 100 }),
	}
	cfg := Config{Start: cal[0], End: cal[len(cal)-1], Cash: 1000, Rebalance: rebalance.Params{MinTradeUSD: 1}}
	r, err := Run(strategy.Fixed{Label: "50/50", Weights: map[string]float64{"A": 0.5, "B": 0.5}}, hist, cal, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range r.Holdings {
		if math.Abs(h["A"]-0.5) > 1e-4 || math.Abs(h["B"]-0.5) > 1e-4 { // orders round to cents
			t.Fatalf("after a rebalance holdings = %v; want 50/50", h)
		}
	}
	if len(r.Holdings) != 2 {
		t.Fatalf("rebalances = %d; want 2 (Jan and Feb month ends)", len(r.Holdings))
	}
}

func TestSummarize(t *testing.T) {
	r := Result{Curve: []Point{
		{market.Date(2020, 1, 1), 100}, {market.Date(2020, 7, 1), 120},
		{market.Date(2020, 12, 31), 90}, {market.Date(2021, 12, 31), 99},
	}}
	st := Summarize(r)
	if math.Abs(st.MaxDrawdown-0.25) > 1e-9 {
		t.Errorf("max drawdown = %v; want 0.25", st.MaxDrawdown)
	}
	if math.Abs(st.Years[2020]+0.10) > 1e-9 || math.Abs(st.Years[2021]-0.10) > 1e-9 {
		t.Errorf("years = %v; want 2020 -10%%, 2021 +10%%", st.Years)
	}
}
