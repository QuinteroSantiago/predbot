package exec

import (
	"encoding/json"
	"testing"

	"predbot/internal/core"
)

// The side mapping is the most dangerous thing to get wrong: inverting it turns
// an arb into a doubled directional bet. Lock it to the documented semantics.
func TestKalshiSideMapping(t *testing.T) {
	k := NewKalshiExecutor("https://x", nil, "")
	buy := k.body(core.Order{Intent: core.Intent{Side: core.BuyYes, Px: 5600, Qty: 1000, TIF: core.IOC}, ClientID: "c1"})
	if buy.Side != "bid" { // docs: bid = buy YES
		t.Errorf("BuyYes -> %q; want bid", buy.Side)
	}
	sell := k.body(core.Order{Intent: core.Intent{Side: core.SellYes, Px: 5600, Qty: 1000}})
	if sell.Side != "ask" { // docs: ask = sell YES
		t.Errorf("SellYes -> %q; want ask", sell.Side)
	}
}

// Price and count must be fixed-point STRINGS in the documented shapes.
func TestKalshiPriceCountFormat(t *testing.T) {
	k := NewKalshiExecutor("https://x", nil, "")
	b := k.body(core.Order{Intent: core.Intent{Side: core.BuyYes, Px: 5600, Qty: 1000, TIF: core.IOC}})
	if b.Price != "0.5600" {
		t.Errorf("price = %q; want 0.5600 (fixed-point dollars)", b.Price)
	}
	if b.Count != "10.00" { // 1000 * 0.01-contract units = 10 contracts
		t.Errorf("count = %q; want 10.00 contracts", b.Count)
	}
	if b.TimeInForce != "immediate_or_cancel" {
		t.Errorf("tif = %q; want immediate_or_cancel", b.TimeInForce)
	}
	if b.SelfTradePrev == "" {
		t.Error("self_trade_prevention_type is required and must be set")
	}
	// Ensure it serializes with the documented field names.
	raw, _ := json.Marshal(b)
	for _, field := range []string{`"ticker"`, `"side"`, `"count"`, `"price"`, `"time_in_force"`, `"self_trade_prevention_type"`} {
		if !contains(string(raw), field) {
			t.Errorf("payload missing %s: %s", field, raw)
		}
	}
}

func TestPolyIntentMapping(t *testing.T) {
	p := NewPolyUSExecutor("https://x", nil)
	buy := p.body(core.Order{Intent: core.Intent{Side: core.BuyYes, Px: 5500, Qty: 1000, TIF: core.IOC}})
	if buy.Intent != "ORDER_INTENT_BUY_LONG" { // docs: BUY_LONG = buy YES
		t.Errorf("BuyYes -> %q; want ORDER_INTENT_BUY_LONG", buy.Intent)
	}
	sell := p.body(core.Order{Intent: core.Intent{Side: core.SellYes, Px: 5500, Qty: 1000}})
	if sell.Intent != "ORDER_INTENT_SELL_LONG" { // docs: SELL_LONG = sell YES
		t.Errorf("SellYes -> %q; want ORDER_INTENT_SELL_LONG", sell.Intent)
	}
	if buy.Price.Value != "0.5500" || buy.Price.Currency != "USD" {
		t.Errorf("price = %+v; want {0.5500 USD}", buy.Price)
	}
	if buy.Quantity != 10 { // 1000 * 0.01 = 10 contracts
		t.Errorf("quantity = %v; want 10 contracts", buy.Quantity)
	}
	if buy.TIF != "TIME_IN_FORCE_IMMEDIATE_OR_CANCEL" {
		t.Errorf("tif = %q", buy.TIF)
	}
}

// Both venues must map the SAME internal side to the same economic action —
// a cross-venue pair buys YES on one and sells YES on the other.
func TestCrossVenueSideConsistency(t *testing.T) {
	k := NewKalshiExecutor("https://x", nil, "")
	p := NewPolyUSExecutor("https://x", nil)
	// Buy YES on Kalshi, Sell YES on Poly.
	kb := k.body(core.Order{Intent: core.Intent{Side: core.BuyYes, Px: 5000, Qty: 100, TIF: core.IOC}})
	pb := p.body(core.Order{Intent: core.Intent{Side: core.SellYes, Px: 6000, Qty: 100, TIF: core.IOC}})
	if kb.Side != "bid" || pb.Intent != "ORDER_INTENT_SELL_LONG" {
		t.Errorf("cross-venue pair mis-mapped: kalshi=%q poly=%q", kb.Side, pb.Intent)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
