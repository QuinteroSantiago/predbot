package backtest

import (
	"fmt"
	"math"
	"math/rand"
	"testing"

	"predbot/internal/core"
)

// fvWorld: each market has a true chance q (and resolves YES with it). Every
// minute for three hours each venue quotes q plus its own noise, ±0.5¢ wide.
func fvWorld(t *testing.T, kNoise, pNoise float64, seed int64) FairValueResult {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	outcomes := map[string]int{}
	f := NewFairValue(Fees{KalshiMult: 0.07}, outcomes)
	snap := func(v, id string, ns int64, mid float64) core.BookSnapshot {
		mid = math.Max(0.05, math.Min(0.95, mid))
		px := core.Price(math.Round(mid * 10000))
		return core.BookSnapshot{Venue: v, VenueID: id, TsRecv: ns,
			YesBids: []core.Level{{Px: px - 50, Qty: 1000}}, YesAsks: []core.Level{{Px: px + 50, Qty: 1000}}}
	}
	for d := 0; d < 60; d++ {
		for g := 0; g < 15; g++ {
			q := 0.2 + 0.6*rng.Float64()
			yes := 0
			if rng.Float64() < q {
				yes = 1
			}
			canonical := fmt.Sprintf("MLB-2026%04d-T%02d", 800+d, g)
			kid, pid := fmt.Sprintf("k-%d-%d", d, g), fmt.Sprintf("p-%d-%d", d, g)
			outcomes["kalshi|"+kid], outcomes["polymarketus|"+pid] = yes, yes
			base := int64(d*86400+g*20000) * 1e9
			// Each venue's error is persistent within a game (a mispriced line),
			// plus a little tick-to-tick jitter.
			ke, pe := rng.NormFloat64()*kNoise, rng.NormFloat64()*pNoise
			for i := 0; i < 180; i++ {
				ns := base + int64(i)*60e9
				f.Observe(canonical, snap("kalshi", kid, ns, q+ke+rng.NormFloat64()*0.002))
				f.Observe(canonical, snap("polymarketus", pid, ns+1e9, q+pe+rng.NormFloat64()*0.002))
			}
		}
	}
	return f.Result()
}

func TestFairValueWeighsTheBetterVenue(t *testing.T) {
	r := fvWorld(t, 0.01, 0.10, 4)
	if r.Note != "" {
		t.Fatalf("note: %s", r.Note)
	}
	if !(r.WK-1.96*r.WKSE > r.WP+1.96*r.WPSE) {
		t.Errorf("weights Kalshi %.2f±%.2f vs Poly %.2f±%.2f: the accurate venue should clearly dominate", r.WK, 1.96*r.WKSE, r.WP, 1.96*r.WPSE)
	}
	if r.BestSingle != "kalshi" || r.LLKalshi >= r.LLPoly {
		t.Errorf("best single venue %q (ll K %.4f, P %.4f), want kalshi", r.BestSingle, r.LLKalshi, r.LLPoly)
	}
	edge := false
	for _, row := range r.Trades {
		if row.Venue == "polymarketus" && FVEdge(row) {
			edge = true
		}
	}
	if !edge {
		t.Errorf("no out-of-sample edge trading the noisy venue's mispricings: %+v", r.Trades)
	}
}

func TestFairValueNoEdgeWhenBothPricesAreRight(t *testing.T) {
	r := fvWorld(t, 0.0, 0.0, 6)
	for _, row := range r.Trades {
		if FVEdge(row) {
			t.Errorf("edge %+v in a world where both prices are the truth", row)
		}
	}
}
