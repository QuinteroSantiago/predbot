package backtest

import (
	"reflect"
	"testing"
	"time"
)

// A report split across shards and merged must finalize to exactly the report
// built in one pass — including max drawdown, which depends on pair order.
func TestMergeMatchesSinglePass(t *testing.T) {
	type pr struct {
		canonical       string
		net, cap, entry int64
		venue           string
		fee             int64
	}
	// Interleaved entry times across two markets. Walked in entry order the max
	// drawdown is 1100; walked shard by shard (all of B first) it would be 1300.
	seq := []pr{
		{"A", 500, 4000, 10, "kalshi", 30},
		{"B", -900, 5000, 20, "polymarketus", -10},
		{"A", 200, 3000, 30, "kalshi", 20},
		{"B", -400, 2000, 40, "polymarketus", -5},
		{"A", 1200, 6000, 50, "kalshi", 40},
	}
	add := func(r *Report, p pr) {
		r.addPair(p.canonical, p.net, p.cap, p.entry)
		r.NetPnL += p.net
		r.CapitalLocked += p.cap
		r.FeesByVenue[p.venue] += p.fee
		r.PairsAttempted++
		r.PairsCompleted++
		r.byCanonical[p.canonical]++
	}

	single := NewReport()
	for _, p := range seq {
		add(single, p)
	}
	// Shards see their own markets only, so their pairs arrive out of global
	// order once concatenated.
	shardB, shardA := NewReport(), NewReport()
	for _, p := range seq {
		if p.canonical == "A" {
			add(shardA, p)
		} else {
			add(shardB, p)
		}
	}
	merged := NewReport()
	merged.Merge(shardB)
	merged.Merge(shardA)

	res := map[string]time.Time{
		"A": time.Unix(0, 0).Add(30 * 24 * time.Hour),
		"B": time.Unix(0, 0).Add(90 * 24 * time.Hour),
	}
	single.Finalize(res, 0.04)
	merged.Finalize(res, 0.04)

	if single.MaxDrawdown != 1100 {
		t.Fatalf("fixture drawdown = %d, want 1100 (entry-order walk)", single.MaxDrawdown)
	}
	if !reflect.DeepEqual(single, merged) {
		t.Fatalf("merged report differs from single pass\nsingle %+v\nmerged %+v", single, merged)
	}
}

// Peak capital sweeps each venue's holds: a pair holds its legs from entry to
// its market's last update. Worked by hand: on Kalshi, A holds 100 over [10,50]
// and B 60 over [30,70] — overlapping, peak 160; C holds 90 from 50, the
// instant A frees its 100, so it reuses that and the peak stays 160. On Poly,
// A's 40 and B's 30 overlap over [30,50]: peak 70.
func TestPeakCapital(t *testing.T) {
	r := NewReport()
	r.addPair("A", 5, 140, 10, legCapital{"kalshi", 100}, legCapital{"polymarketus", 40})
	r.addPair("B", 5, 90, 30, legCapital{"kalshi", 60}, legCapital{"polymarketus", 30})
	r.addPair("C", 5, 90, 50, legCapital{"kalshi", 90})
	for c, ts := range map[string]int64{"A": 50, "B": 70, "C": 90} {
		r.seen(c, ts)
	}
	r.Finalize(nil, 0)
	if got := r.PeakCapital["kalshi"]; got != 160 {
		t.Errorf("kalshi peak = %d, want 160", got)
	}
	if got := r.PeakCapital["polymarketus"]; got != 70 {
		t.Errorf("polymarketus peak = %d, want 70 (40 + 30 overlapping over [30,50])", got)
	}
}
