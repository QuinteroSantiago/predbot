package backtest

import (
	"testing"

	"predbot/internal/core"
)

func TestFillBuyWalksBook(t *testing.T) {
	// Asks: 100@0.50, 100@0.51. Buy 150 with limit 0.51 -> avg over both levels.
	b := core.BookSnapshot{YesAsks: []core.Level{{Px: 5000, Qty: 10000}, {Px: 5100, Qty: 10000}}}
	px, qty := fillBuy(b, 5100, 15000)
	if qty != 15000 {
		t.Fatalf("filled qty = %d; want 15000", qty)
	}
	// notional = 5000*100 + 5100*50 = 500000+255000 = 755000; /150 = 5033.
	if px != 5033 {
		t.Errorf("avg px = %d; want 5033", px)
	}
}

func TestFillBuyRespectsLimit(t *testing.T) {
	// Only the first level is within the 0.50 limit.
	b := core.BookSnapshot{YesAsks: []core.Level{{Px: 5000, Qty: 10000}, {Px: 5100, Qty: 10000}}}
	px, qty := fillBuy(b, 5000, 15000)
	if qty != 10000 || px != 5000 {
		t.Errorf("limit-capped fill = %d @ %d; want 10000 @ 5000", qty, px)
	}
}

func TestFillSellWalksBids(t *testing.T) {
	b := core.BookSnapshot{YesBids: []core.Level{{Px: 6000, Qty: 10000}, {Px: 5900, Qty: 10000}}}
	px, qty := fillSell(b, 5900, 15000)
	if qty != 15000 {
		t.Fatalf("filled = %d; want 15000", qty)
	}
	// notional = 6000*100 + 5900*50 = 600000+295000=895000; /150=5966.
	if px != 5966 {
		t.Errorf("avg px = %d; want 5966", px)
	}
}
