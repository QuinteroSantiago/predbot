package backtest

import (
	"fmt"
	"math"
	"math/rand"
	"testing"

	"predbot/internal/core"
)

// orWorld simulates settled markets over 60 days on both venues. Outcomes are
// drawn from σ(slope·logit(price)): slope 1 is a correctly priced world.
func orWorld(t *testing.T, slope float64, seed int64) OutcomeRegResult {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	outcomes := map[string]int{}
	type q struct {
		venue, id, canonical string
		p                    float64
	}
	var qs []q
	for d := 0; d < 60; d++ {
		for g := 0; g < 15; g++ {
			p := 0.2 + 0.6*rng.Float64()
			yes := 0
			if rng.Float64() < sigmoid(slope*logOdds(p)) {
				yes = 1
			}
			canonical := fmt.Sprintf("MLB-2026%04d-T%02d", 800+d, g)
			for _, v := range []string{"kalshi", "polymarketus"} {
				id := fmt.Sprintf("%s-%d-%d", v, d, g)
				outcomes[v+"|"+id] = yes
				qs = append(qs, q{v, id, canonical, p})
			}
		}
	}
	o := NewOutcomeReg(outcomes)
	for i, x := range qs {
		px := core.Price(math.Round(x.p * 10000))
		o.Observe(x.canonical, core.BookSnapshot{Venue: x.venue, VenueID: x.id, TsRecv: int64(i),
			YesBids: []core.Level{{Px: px - 50, Qty: 1000}}, YesAsks: []core.Level{{Px: px + 50, Qty: 1000}}})
	}
	return o.Result()
}

func TestOutcomeRegFindsBiasOnlyWhereItExists(t *testing.T) {
	fair := orWorld(t, 1.0, 1)
	biased := orWorld(t, 1.5, 2)
	if fair.Obs != 1800 || len(fair.Calib) != 2 {
		t.Fatalf("fair world: %d obs, %d calibration fits; want 1800 and 2", fair.Obs, len(fair.Calib))
	}
	for _, c := range fair.Calib {
		if CalibVerdict(c) != "calibrated within noise" {
			t.Errorf("fair world, %s: slope %.2f ± %.2f read as %q", c.Venue, c.Slope, 1.96*c.SlopeSE, CalibVerdict(c))
		}
	}
	for _, c := range biased.Calib {
		if CalibVerdict(c) != "favorites underpriced, longshots overpriced" {
			t.Errorf("biased world, %s: slope %.2f ± %.2f read as %q", c.Venue, c.Slope, 1.96*c.SlopeSE, CalibVerdict(c))
		}
	}
	// Out of sample the model only earns its keep where there's bias to learn.
	if cv := fair.CV; cv.LLDiff-1.96*cv.LLDiffSE > 0 {
		t.Errorf("fair world: model 'beats' the price out of sample (%+.4f ± %.4f)", cv.LLDiff, 1.96*cv.LLDiffSE)
	}
	if cv := biased.CV; cv.LLDiff <= 0 {
		t.Errorf("biased world: model doesn't beat the price out of sample (%+.4f)", cv.LLDiff)
	}
}
