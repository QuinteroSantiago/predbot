package rebalance

import (
	"math"
	"testing"

	"etfbot/internal/market"
)

func TestPlanSellsFirstAndExitsFully(t *testing.T) {
	holdings := map[string]float64{"SPY": 10, "TLT": 5.5}
	prices := map[string]float64{"SPY": 100, "TLT": 50, "GLD": 200}
	// equity = 1000 + 275 + 725 cash = 2000
	got := Plan(holdings, 725, prices, map[string]float64{"SPY": 0.5, "GLD": 0.5}, Params{MinTradeUSD: 1})
	want := []market.Order{
		{Symbol: "TLT", Side: market.Sell, Qty: 5.5},
		{Symbol: "GLD", Side: market.Buy, Notional: 1000},
	}
	if len(got) != len(want) {
		t.Fatalf("orders = %+v; want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("order %d = %+v; want %+v", i, got[i], want[i])
		}
	}
}

func TestPlanSkipsSmallDriftButNotExits(t *testing.T) {
	holdings := map[string]float64{"SPY": 10.02, "IEF": 0.01}
	prices := map[string]float64{"SPY": 100, "IEF": 100}
	got := Plan(holdings, 0, prices, map[string]float64{"SPY": 1}, Params{MinTradeUSD: 25})
	if len(got) != 1 || got[0].Symbol != "IEF" || got[0].Qty != 0.01 {
		t.Fatalf("orders = %+v; want only the IEF exit", got)
	}
}

func TestPlanCashBufferAndTrim(t *testing.T) {
	holdings := map[string]float64{"SPY": 20}
	prices := map[string]float64{"SPY": 100}
	got := Plan(holdings, 0, prices, map[string]float64{"SPY": 0.5}, Params{CashBuffer: 0.01})
	// target 0.5 * 2000 * 0.99 = 990 → sell 1010 worth = 10.1 shares
	if len(got) != 1 || got[0].Side != market.Sell || math.Abs(got[0].Qty-10.1) > 1e-9 {
		t.Fatalf("orders = %+v; want sell 10.1 SPY", got)
	}
}

func TestFitBuys(t *testing.T) {
	buys := []market.Order{{Symbol: "A", Notional: 600}, {Symbol: "B", Notional: 400}}
	got := FitBuys(buys, 500)
	if got[0].Notional != 300 || got[1].Notional != 200 {
		t.Fatalf("fit = %+v; want 300/200", got)
	}
	if got := FitBuys(buys, 5000); got[0].Notional != 600 {
		t.Fatal("buys that fit must be left alone")
	}
}
