package backtest

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"predbot/internal/config"
	"predbot/internal/core"
	"predbot/internal/strategy"
)

// writeTradingArchive lays out a normalized archive that actually trades, unlike
// writeSyntheticArchive, whose books never cross. Markets have staggered
// lifetimes like a daily slate, open and close cross-venue gaps at different
// times, sometimes cross within one venue, and carry varying depth so some
// pairs leg or fade. It returns the data dir and the markets that map it.
func writeTradingArchive(t testing.TB, nMarkets, nSnaps int) (string, []config.Market) {
	t.Helper()
	dir := t.TempDir()
	norm := filepath.Join(dir, "normalized")
	if err := os.MkdirAll(norm, 0o755); err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(11))
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	const step = 5 * time.Millisecond
	span := time.Duration(nSnaps) * step

	markets := make([]config.Market, nMarkets)
	type live struct {
		from, to time.Duration
		mid, gap int
	}
	state := make([]live, nMarkets)
	for m := range markets {
		markets[m].Canonical = fmt.Sprintf("TEST-M%02d", m)
		markets[m].ResolutionDate = "2026-12-31"
		markets[m].Venues.Kalshi.Ticker = fmt.Sprintf("K-M%02d", m)
		markets[m].Venues.PolymarketUS.Slug = fmt.Sprintf("p-m%02d", m)
		from := time.Duration(rng.Int63n(int64(span * 2 / 3)))
		state[m] = live{from: from, to: from + span/4 + time.Duration(rng.Int63n(int64(span/4))),
			mid: 3000 + rng.Intn(4000)}
	}

	writers := map[string]*gzip.Writer{}
	defer func() {
		for _, w := range writers {
			w.Close()
		}
	}()
	writer := func(v string, ts int64) *gzip.Writer {
		name := fmt.Sprintf("%s-%s.jsonl.gz", v, time.Unix(0, ts).UTC().Format("2006-01-02T15"))
		if w, ok := writers[name]; ok {
			return w
		}
		f, err := os.Create(filepath.Join(norm, name))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
		w := gzip.NewWriter(f)
		writers[name] = w
		return w
	}
	depth := []core.Qty{300, 1500, 5000, 12000}
	for i := 0; i < nSnaps; i++ {
		at := time.Duration(i) * step
		var active []int
		for m, st := range state {
			if at >= st.from && at < st.to {
				active = append(active, m)
			}
		}
		if len(active) == 0 {
			continue
		}
		m := active[rng.Intn(len(active))]
		st := &state[m]
		st.mid = max(1000, min(9000, st.mid+rng.Intn(61)-30))
		switch r := rng.Float64(); {
		case r < 0.01:
			st.gap = []int{300, 500, 900, -400, -700}[rng.Intn(5)]
		case r < 0.05:
			st.gap = 0
		}
		v, id, half := "kalshi", markets[m].Venues.Kalshi.Ticker, -st.gap/2
		if rng.Intn(2) == 0 {
			v, id, half = "polymarketus", markets[m].Venues.PolymarketUS.Slug, st.gap/2
		}
		c := st.mid + half
		spread := []int{100, 200, 300}[rng.Intn(3)]
		bid, ask := c-spread/2, c+spread/2
		if rng.Float64() < 0.003 {
			bid, ask = ask+100, bid+100 // crossed within the venue
		}
		ts := base.Add(at).UnixNano()
		s := core.BookSnapshot{
			Venue: v, VenueID: id,
			YesBids:  []core.Level{{Px: core.Price(bid), Qty: depth[rng.Intn(4)]}, {Px: core.Price(bid - 100), Qty: depth[rng.Intn(4)]}},
			YesAsks:  []core.Level{{Px: core.Price(ask), Qty: depth[rng.Intn(4)]}, {Px: core.Price(ask + 100), Qty: depth[rng.Intn(4)]}},
			TsServer: ts - int64(time.Millisecond), TsRecv: ts, Seq: uint64(i), State: core.StateOpen,
		}
		line, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer(v, ts).Write(append(line, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	for _, w := range writers {
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return dir, markets
}

// The acceptance gate for sharding: any shard count must produce exactly the
// single-shard result, and the fixture must exercise every outcome so the
// comparison can't pass on an empty report.
func TestShardedTradingMatchesSerial(t *testing.T) {
	dir, markets := writeTradingArchive(t, 30, 15_000)
	res := map[string]time.Time{}
	for _, m := range markets {
		res[m.Canonical], _ = time.Parse("2006-01-02", m.ResolutionDate)
	}
	for _, maker := range []bool{false, true} {
		opts := TradingOpts{
			Fees:       Fees{KalshiMult: 0.07},
			LatencyMS:  150,
			Strategy:   strategy.Config{MinEdge: 100, MaxClip: 5000},
			MaxAge:     int64(5 * time.Second),
			MaxSkew:    int64(time.Second),
			Resolution: res,
			Maker:      maker,
			Spread:     true,
		}
		run := func(n int) *TradingResult {
			t.Helper()
			r, err := runTrading(dir, 0, markets, opts, n)
			if err != nil {
				t.Fatalf("maker=%v shards=%d: %v", maker, n, err)
			}
			r.Report.Finalize(res, 0.04)
			return r
		}
		want := run(1)
		rep := want.Report
		if rep.PairsCompleted == 0 || rep.PairsLegged == 0 || rep.IntraCrossings == 0 || len(want.Opps) == 0 {
			t.Fatalf("maker=%v: fixture too tame: completed=%d legged=%d intra=%d opps=%d",
				maker, rep.PairsCompleted, rep.PairsLegged, rep.IntraCrossings, len(want.Opps))
		}
		if maker && want.RestsFilled == 0 {
			t.Fatalf("fixture filled no rests (placed %d)", want.RestsPlaced)
		}
		if !maker && rep.PairsFaded == 0 {
			t.Fatal("fixture faded no taker pairs")
		}
		for _, n := range []int{2, 5, 16} {
			if got := run(n); !reflect.DeepEqual(got, want) {
				t.Errorf("maker=%v shards=%d differs from serial\n got %s\nwant %s",
					maker, n, got.Report, want.Report)
			}
		}
	}
}
