package backtest

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

// A sample with a built-in favorite–longshot bias: 20%-priced markets win 10%
// of the time and 80%-priced ones win 90%. The result must bucket them, give the
// binomial standard error, and fit a line steeper than 1.
func TestLongshotResult(t *testing.T) {
	outcomes := map[string]int{}
	add := func(prefix string, n, wins int) {
		for i := 0; i < n; i++ {
			key := fmt.Sprintf("kalshi|%s-%d", prefix, i)
			if i < wins {
				outcomes[key] = 1
			} else {
				outcomes[key] = 0
			}
		}
	}
	add("LO", 100, 10)
	add("HI", 100, 90)
	ls := NewLongshot(outcomes)
	for key := range outcomes {
		p := 0.2
		if strings.Contains(key, "HI") {
			p = 0.8
		}
		ls.ObserveMid(key, p)
		ls.ObserveMid(key, 0.5) // only the opening price counts
	}

	r := ls.Result()
	if r.Markets != 200 || len(r.Buckets) != 2 {
		t.Fatalf("markets=%d buckets=%d, want 200 and 2", r.Markets, len(r.Buckets))
	}
	lo, hi := r.Buckets[0], r.Buckets[1]
	if lo.N != 100 || math.Abs(lo.Realized-0.1) > 1e-9 || math.Abs(lo.Edge-(-0.1)) > 1e-9 {
		t.Errorf("low bucket = %+v, want n=100 realized=0.1 edge=-0.1", lo)
	}
	if want := math.Sqrt(0.1 * 0.9 / 100); math.Abs(lo.StdErr-want) > 1e-12 {
		t.Errorf("low bucket stderr = %v, want %v", lo.StdErr, want)
	}
	if hi.N != 100 || math.Abs(hi.Realized-0.9) > 1e-9 {
		t.Errorf("high bucket = %+v, want n=100 realized=0.9", hi)
	}
	if !r.FitOK || r.Slope <= 1.1 || r.Verdict != "favorite-longshot" {
		t.Errorf("fit slope=%v intercept=%v verdict=%q, want a favorite-longshot fit", r.Slope, r.Intercept, r.Verdict)
	}
	if !strings.Contains(ls.Report(), "favorite–longshot bias") {
		t.Error("text report doesn't name the bias the result found")
	}
}
