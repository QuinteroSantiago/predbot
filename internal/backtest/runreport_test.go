package backtest

import (
	"encoding/json"
	"math"
	"sort"
	"testing"
	"time"

	"predbot/internal/strategy"
)

// The JSON report must say the same thing as the Report it's built from: the
// runs page does its own arithmetic (waterfall, equity curve, per-market bars)
// on these numbers.
func TestRunReportAgreesWithReport(t *testing.T) {
	dir, markets := writeTradingArchive(t, 30, 15_000)
	res := map[string]time.Time{}
	for _, m := range markets {
		res[m.Canonical], _ = time.Parse("2006-01-02", m.ResolutionDate)
	}
	for _, maker := range []bool{false, true} {
		r, err := runTrading(dir, 0, markets, TradingOpts{
			Fees: Fees{KalshiMult: 0.07}, LatencyMS: 150,
			Strategy: strategy.Config{MinEdge: 100, MaxClip: 5000},
			MaxAge:   int64(5 * time.Second), MaxSkew: int64(time.Second),
			Resolution: res, Maker: maker,
		}, 4)
		if err != nil {
			t.Fatal(err)
		}
		r.Report.Finalize(res, 0.04)
		rr := NewRunReport(RunMeta{ID: "x"}, r, maker)

		near := func(what string, got, want float64) {
			t.Helper()
			if math.Abs(got-want) > 1e-6 {
				t.Errorf("maker=%v %s = %v, want %v", maker, what, got, want)
			}
		}
		rep := rr.Report
		near("net", rep.NetPnLUSD, usd(r.Report.NetPnL))
		var fees, marketNet, pairNet float64
		for _, f := range rep.FeesByVenueUSD {
			fees += f
		}
		near("fees total", rep.FeesUSD, fees)
		// The waterfall: gross − fees − legging = net.
		near("waterfall", rep.GrossUSD-rep.FeesUSD-rep.LeggingCostUSD, rep.NetPnLUSD)
		for _, m := range rr.Markets {
			marketNet += m.NetUSD
		}
		for _, p := range rr.Pairs {
			pairNet += p.NetUSD
		}
		near("sum of market nets", marketNet, rep.NetPnLUSD)
		near("sum of pair nets", pairNet, rep.NetPnLUSD)

		if len(rr.Pairs) != rep.SettledPairs || len(rr.Pairs) == 0 {
			t.Errorf("maker=%v: %d pairs, %d settled", maker, len(rr.Pairs), rep.SettledPairs)
		}
		if !sort.SliceIsSorted(rr.Pairs, func(i, j int) bool { return rr.Pairs[i].EntryMs < rr.Pairs[j].EntryMs }) {
			t.Errorf("maker=%v: pairs not in entry order", maker)
		}
		if !sort.SliceIsSorted(rr.Markets, func(i, j int) bool { return rr.Markets[i].NetUSD > rr.Markets[j].NetUSD }) {
			t.Errorf("maker=%v: markets not best-first", maker)
		}
		if (rr.Maker != nil) != maker {
			t.Errorf("maker=%v: maker section present = %v", maker, rr.Maker != nil)
		}
		if _, err := json.Marshal(rr); err != nil {
			t.Fatalf("marshal: %v", err)
		}
	}
}
