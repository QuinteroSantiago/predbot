package backtest

import (
	"fmt"
	"math"
	"math/rand"
	"testing"

	"predbot/internal/core"
)

func mrSnap(venueName string, ns int64, midCents float64) core.BookSnapshot {
	px := func(c float64) core.Price { return core.Price(math.Round(c * 100)) }
	return core.BookSnapshot{Venue: venueName, VenueID: "M",
		YesBids: []core.Level{{Px: px(midCents - 0.5), Qty: 1000}},
		YesAsks: []core.Level{{Px: px(midCents + 0.5), Qty: 1000}},
		TsRecv:  ns, State: core.StateOpen}
}

// Gaps simulated as AR(1) with a known pull-back b = −0.1 per 5s step, i.e. a
// half-life of 5·ln2/−ln(0.9) ≈ 32.9s. The pooled fit must recover it inside
// its own 95% range.
func TestSpreadMRRecoversKnownHalfLife(t *testing.T) {
	const b, steps, markets = -0.1, 3000, 20
	rng := rand.New(rand.NewSource(3))
	m := NewSpreadMR(Fees{})
	for mk := 0; mk < markets; mk++ {
		name := fmt.Sprintf("MLB-TEST-%02d", mk)
		gap, level := 0.0, rng.Float64()*2-1 // each market has its own offset
		for i := 0; i < steps; i++ {
			gap += b*gap + rng.NormFloat64()*1.5
			ns := int64(i)*5e9 + 1e9
			m.Observe(name, mrSnap("kalshi", ns, 50+(level+gap)/2))
			m.Observe(name, mrSnap("polymarketus", ns+1e8, 50-(level+gap)/2))
		}
	}
	r := m.Result()
	if r.Markets != markets {
		t.Fatalf("fitted %d markets, want %d", r.Markets, markets)
	}
	want := halfLife(b, 5)
	if math.Abs(want-32.9) > 0.1 {
		t.Fatalf("halfLife(-0.1, 5s) = %v, want ~32.9s", want)
	}
	// Prices round to 0.01¢, so allow the fit a little slack around b.
	if lo, hi := r.PooledB-1.96*r.PooledBSE-0.01, r.PooledB+1.96*r.PooledBSE+0.01; b < lo || b > hi {
		t.Errorf("pooled b = %.4f ± %.4f, true b %.2f outside the range", r.PooledB, 1.96*r.PooledBSE, b)
	}
	if math.Abs(r.HalfLifeS-want)/want > 0.15 {
		t.Errorf("pooled half-life %.1fs, want %.1fs within 15%%", r.HalfLifeS, want)
	}
	if r.MedianAbsGap < 0.1 {
		t.Errorf("median |level| %.2f¢: the per-market offsets went missing", r.MedianAbsGap)
	}
}

// A convergence trade worked by hand: the gap was wide (Kalshi rich), so sell
// Kalshi at its 55¢ bid and buy Poly at its 46¢ ask; close by buying Kalshi at
// 52¢ and selling Poly at 48¢. Gross 3 + 2 = 5¢, less four taker fees.
func TestSpreadMRTradeMath(t *testing.T) {
	m := NewSpreadMR(Fees{KalshiMult: 0.07})
	b := func(bid, ask float64) mrBook {
		return mrBook{bid: core.Price(bid * 100), ask: core.Price(ask * 100), ok: true}
	}
	tr := &mrTrade{dir: 1, k0: b(55, 56), p0: b(45, 46)}
	got := m.closeTrade(tr, b(51, 52), b(48, 49))
	want := 5 - m.feeC("kalshi", 5500) - m.feeC("kalshi", 5200) - m.feeC("polymarketus", 4600) - m.feeC("polymarketus", 4800)
	if math.Abs(got-want) > 1e-9 || want >= 5 {
		t.Fatalf("closeTrade = %v, want %v (5¢ gross less fees)", got, want)
	}
	// The mirror image: gap too narrow, buy Kalshi and sell Poly.
	tr = &mrTrade{dir: -1, k0: b(44, 45), p0: b(54, 55)}
	got = m.closeTrade(tr, b(48, 49), b(51, 52))
	want = (48 - 45) + (54 - 52) - m.feeC("kalshi", 4500) - m.feeC("kalshi", 4800) - m.feeC("polymarketus", 5400) - m.feeC("polymarketus", 5200)
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("mirror closeTrade = %v, want %v", got, want)
	}
}
