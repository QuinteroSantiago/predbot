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
