package backtest

import (
	"testing"

	"predbot/internal/core"
)

func restBuy(px core.Price, qty core.Qty) core.Intent {
	return core.Intent{Venue: "polymarketus", Side: core.BuyYes, Px: px, Qty: qty, TIF: core.GTC}
}

// Having the best bid is NOT a fill — the market must cross to us. This is the
// mistake that makes maker backtests look like free money.
func TestRestingNotFilledWithoutCross(t *testing.T) {
	joined := core.BookSnapshot{YesBids: []core.Level{{Px: 5000, Qty: 0}}}
	r := NewRestingFill(restBuy(5000, 1000), joined)
	// Book moves around but nobody sells at/below our bid.
	got := r.Observe(core.BookSnapshot{
		YesBids: []core.Level{{Px: 5000, Qty: 5000}},
		YesAsks: []core.Level{{Px: 5100, Qty: 5000}}, // ask stays ABOVE our bid
	})
	if got != 0 || r.Complete() {
		t.Errorf("no cross must mean no fill; got %v filled=%v", got, r.Filled)
	}
}

// A genuine cross fills us — but only after the queue ahead is consumed.
func TestRestingFillsAfterQueue(t *testing.T) {
	// 300 contracts already resting at our price: we are behind them.
	joined := core.BookSnapshot{YesBids: []core.Level{{Px: 5000, Qty: 30000}}}
	r := NewRestingFill(restBuy(5000, 1000), joined) // we want 10 contracts
	if r.QueueAhead != 30000 {
		t.Fatalf("queue ahead = %v; want 30000", r.QueueAhead)
	}
	// 200 contracts sell into the price: all absorbed by the queue ahead.
	if got := r.Observe(core.BookSnapshot{YesAsks: []core.Level{{Px: 5000, Qty: 20000}}}); got != 0 {
		t.Errorf("queue should absorb the first cross; got %v", got)
	}
	// A further 150 contracts cross: 100 finishes the queue, 10 fills us.
	got := r.Observe(core.BookSnapshot{YesAsks: []core.Level{{Px: 5000, Qty: 35000}}})
	if got != 1000 || !r.Complete() {
		t.Errorf("expected our 10 contracts to fill; got %v complete=%v", got, r.Complete())
	}
}

// A resting SELL fills when someone bids at or above our ask.
func TestRestingSellFillsOnBidUp(t *testing.T) {
	joined := core.BookSnapshot{YesAsks: []core.Level{{Px: 6000, Qty: 0}}}
	r := NewRestingFill(core.Intent{Side: core.SellYes, Px: 6000, Qty: 500, TIF: core.GTC}, joined)
	if got := r.Observe(core.BookSnapshot{YesBids: []core.Level{{Px: 6000, Qty: 10000}}}); got != 500 {
		t.Errorf("bid crossing our ask should fill us; got %v", got)
	}
}
