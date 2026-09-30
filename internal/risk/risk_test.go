package risk

import (
	"errors"
	"testing"

	"tradekit/killswitch"

	"etfbot/internal/config"
	"etfbot/internal/market"
)

func gate() *Gate { return New(config.Default(), killswitch.New("")) }

func TestCheck(t *testing.T) {
	buy := func(sym string, usd float64) market.Order {
		return market.Order{Symbol: sym, Side: market.Buy, Notional: usd}
	}
	for _, c := range []struct {
		name    string
		targets map[string]float64
		orders  []market.Order
		want    error
	}{
		{"ok", map[string]float64{"SPY": 1.0 / 3, "BIL": 2.0 / 3}, []market.Order{buy("SPY", 100)}, nil},
		{"cash fund may hold everything", map[string]float64{"BIL": 1}, nil, nil},
		{"over weight", map[string]float64{"SPY": 0.5}, nil, ErrWeight},
		{"outside universe", map[string]float64{"TSLA": 0.1}, nil, ErrUniverse},
		{"buy outside universe", nil, []market.Order{buy("TSLA", 10)}, ErrUniverse},
		{"exit outside universe is fine", nil, []market.Order{{Symbol: "TSLA", Side: market.Sell, Qty: 1}}, nil},
		{"order too big", nil, []market.Order{buy("SPY", 5001)}, ErrOrderSize},
		{"over invested", map[string]float64{"BIL": 0.9, "SPY": 0.3}, nil, ErrOverInvest},
	} {
		if err := gate().Check(c.targets, c.orders); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v; want %v", c.name, err, c.want)
		}
	}
}

func TestDrawdownKills(t *testing.T) {
	g := gate()
	if g.Drawdown(8000, 10000) {
		t.Fatal("a 20% drawdown is inside the 25% limit")
	}
	if !g.Drawdown(7400, 10000) || !g.Kill.Killed() {
		t.Fatal("a 26% drawdown must engage the kill switch")
	}
	if err := g.Check(nil, nil); !errors.Is(err, ErrKilled) {
		t.Fatalf("after a kill every plan must be refused, got %v", err)
	}
}
