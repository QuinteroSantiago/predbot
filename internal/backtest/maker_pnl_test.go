package backtest

import (
	"testing"

	"predbot/internal/core"
)

// The whole maker thesis in one test: a 2c gross spread LOSES money as
// taker/taker (3.25c hurdle at p=0.5) but MAKES money when one leg rests and
// collects the Polymarket rebate instead of paying the taker fee.
func TestMakerRebateFlipsAMarginalSpread(t *testing.T) {
	f := Fees{KalshiMult: 0.07}
	qty := core.Qty(10000) // 100 contracts
	// 2c gross at ~p=0.5: buy 0.49, sell 0.51.
	gross := 0.02 * 100 // $2.00 over 100 contracts

	takerFees := (float64(f.Fee("kalshi", 4900, qty, false)) +
		float64(f.Fee("polymarketus", 5100, qty, false))) / 1e4
	makerFees := (float64(f.Fee("kalshi", 4900, qty, false)) +
		float64(f.Fee("polymarketus", 5100, qty, true))) / 1e4

	takerNet := gross - takerFees
	makerNet := gross - makerFees
	t.Logf("2c spread on 100 contracts: taker net $%.2f | poly-maker net $%.2f", takerNet, makerNet)

	if takerNet >= 0 {
		t.Errorf("a 2c spread should NOT be profitable taker/taker, got $%.2f", takerNet)
	}
	if makerNet <= 0 {
		t.Errorf("the maker rebate should make it profitable, got $%.2f", makerNet)
	}
	if makerNet <= takerNet {
		t.Error("maker must beat taker")
	}
}
