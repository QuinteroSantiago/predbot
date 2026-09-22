package backtest

import "testing"
import "predbot/internal/core"

// Property tests from design §10.3.
func TestFeeProperties(t *testing.T) {
	f := Fees{KalshiMult: 0.07}
	for _, venue := range []string{"kalshi", "polymarketus"} {
		// Taker fee >= 0.
		for _, px := range []core.Price{100, 2500, 5000, 7500, 9900} {
			if fee := f.Fee(venue, px, 10000, false); fee < 0 {
				t.Errorf("%s taker fee negative: px=%d fee=%d", venue, px, fee)
			}
		}
		// |fee| monotone non-decreasing in qty.
		var prev int64
		for _, q := range []core.Qty{100, 1000, 5000, 20000} {
			fee := f.Fee(venue, 5000, q, false)
			if fee < prev {
				t.Errorf("%s fee not monotone in qty: %d < %d", venue, fee, prev)
			}
			prev = fee
		}
		// Symmetric in p <-> 1-p (fee at 0.30 == fee at 0.70).
		lo := f.Fee(venue, 3000, 10000, false)
		hi := f.Fee(venue, 7000, 10000, false)
		if lo != hi {
			t.Errorf("%s fee not symmetric p<->1-p: %d vs %d", venue, lo, hi)
		}
	}
}

// Polymarket maker fee is a rebate (negative); Kalshi maker is a positive fee.
func TestMakerSigns(t *testing.T) {
	f := Fees{KalshiMult: 0.07}
	if poly := f.Fee("polymarketus", 5000, 10000, true); poly >= 0 {
		t.Errorf("poly maker should be a rebate (negative), got %d", poly)
	}
	if k := f.Fee("kalshi", 5000, 10000, true); k <= 0 {
		t.Errorf("kalshi maker should be a positive fee, got %d", k)
	}
}

// At p=0.5, Poly taker fee = 0.0695 * C * 0.25 = 0.017375/contract. 100 contracts -> $1.7375 -> $1.74.
func TestPolyFeeMagnitude(t *testing.T) {
	f := Fees{}
	fee := f.Fee("polymarketus", 5000, 10000, false) // 100 contracts at $0.50
	// $1.74 in $0.0001 units = 17400.
	if fee != 17400 {
		t.Errorf("poly fee at p=0.5, 100 contracts = %d; want 17400 ($1.74)", fee)
	}
}
