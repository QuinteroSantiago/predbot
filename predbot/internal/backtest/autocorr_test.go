package backtest

import (
	"fmt"
	"math"
	"math/rand"
	"testing"
)

// Mid changes simulated as AR(1) on the 5s grid with φ = −0.3: each step's
// change undoes 30% of the last. The pooled 5s fit must recover β ≈ −0.3 and
// read it as reversion.
func TestAutocorrRecoversKnownReversion(t *testing.T) {
	const phi, steps, markets = -0.3, 4000, 20
	rng := rand.New(rand.NewSource(5))
	a := NewAutocorr(Fees{})
	for mk := 0; mk < markets; mk++ {
		name := fmt.Sprintf("MLB-TEST-%02d", mk)
		mid, r := 50.0, 0.0
		for i := 0; i < steps; i++ {
			r = phi*r + rng.NormFloat64()*0.8
			mid = math.Max(5, math.Min(95, mid+r))
			snap := mrSnap("kalshi", int64(i)*5e9+1e9, mid)
			snap.VenueID = name // one series per market
			a.Observe(name, snap)
		}
	}
	var fit ACFit
	for _, f := range a.Result().Fits {
		if f.Venue == "kalshi" && f.HorizonS == 5 {
			fit = f
		}
	}
	if fit.Markets != markets {
		t.Fatalf("5s fit covers %d markets, want %d", fit.Markets, markets)
	}
	if lo, hi := fit.Beta-1.96*fit.BetaSE-0.02, fit.Beta+1.96*fit.BetaSE+0.02; phi < lo || phi > hi {
		t.Errorf("β(5s) = %.3f ± %.3f, true %.1f outside", fit.Beta, 1.96*fit.BetaSE, phi)
	}
	if ACVerdict(fit) != "reversion" {
		t.Errorf("verdict %q, want reversion", ACVerdict(fit))
	}
}

// The sum-based clustered mean must match the one built from raw values.
func TestClusteredFromSumsMatches(t *testing.T) {
	xs := []float64{1, 2, 3, 10, 11, -4}
	gs := []string{"a", "a", "b", "b", "b", "c"}
	wantM, wantSE := clusteredMean(xs, gs)
	gotM, gotSE := clusteredFromSums([]int{2, 3, 1}, []float64{3, 24, -4})
	if math.Abs(gotM-wantM) > 1e-12 || math.Abs(gotSE-wantSE) > 1e-12 {
		t.Fatalf("from sums %v±%v, from values %v±%v", gotM, gotSE, wantM, wantSE)
	}
}
