package backtest

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"predbot/internal/core"
)

// FairValue estimates each market's fair YES probability from both venues'
// prices together, and tests betting when one venue strays from it.
//
//   - Fair value: P(yes) = σ(a + wK·logit(pK) + wP·logit(pP)), fitted on
//     settled markets. The weights say which venue's price carries more
//     information; SEs are clustered by league-day.
//   - Out of sample: 5 folds by day. Fair value, Kalshi alone, Poly alone and
//     the simple average are scored (log-loss) on days the fit never saw.
//   - Trading, also out of sample: on a held-out day, when a venue's price is
//     off fair by more than its fee plus a margin τ, buy (or sell) YES there at
//     the touch and hold to settlement. At most one trade per market, venue and
//     τ — repeated entries would all ride the same outcome.
//
// Prices are sampled every 10 minutes while both venues quote fresh books.
type FairValue struct {
	fees     Fees
	outcomes map[string]int
	grid     int64 // seconds between samples
	maxAge   int64 // ns
	clip     int64
	margins  []float64 // τ, cents
	markets  map[string]*fvMarket
}

type fvMarket struct {
	k, p       mrBook
	kKey, pKey string
	nextGrid   int64
	started    bool
	samples    []fvSample
}

type fvSample struct {
	sec  int64
	k, p mrBook
}

// NewFairValue samples every 10 minutes (both books updated within the last 2
// minutes — a quiet pregame book can go a while without changing), prices fees
// at a 10-contract clip, and tests margins of 1, 3 and 5¢ beyond fees.
func NewFairValue(fees Fees, outcomes map[string]int) *FairValue {
	return &FairValue{fees: fees, outcomes: outcomes, grid: 600, maxAge: 120e9, clip: 10,
		margins: []float64{1, 3, 5}, markets: map[string]*fvMarket{}}
}

// Observe feeds one snapshot, mapped to its canonical market.
func (f *FairValue) Observe(canonical string, s core.BookSnapshot) {
	if s.Venue != core.VenueKalshi && s.Venue != core.VenuePoly {
		return
	}
	m := f.markets[canonical]
	if m == nil {
		m = &fvMarket{}
		f.markets[canonical] = m
	}
	sec := s.TsRecv / 1e9
	if !m.started {
		m.nextGrid, m.started = sec+f.grid, true
	}
	for m.nextGrid <= sec {
		ns := m.nextGrid * 1e9
		if m.k.ok && m.p.ok && ns-m.k.ts <= f.maxAge && ns-m.p.ts <= f.maxAge {
			m.samples = append(m.samples, fvSample{sec: m.nextGrid, k: m.k, p: m.p})
		}
		m.nextGrid += f.grid
	}
	b := mrBook{ts: s.TsRecv}
	bid, okB := s.BestYesBid()
	ask, okA := s.BestYesAsk()
	if okB && okA && ask.Px > bid.Px {
		b.bid, b.ask, b.ok = bid.Px, ask.Px, true
	}
	key := s.Venue + "|" + s.VenueID
	if s.Venue == core.VenueKalshi {
		m.k, m.kKey = b, key
	} else {
		m.p, m.pKey = b, key
	}
}

func (f *FairValue) feeC(venueName string, px core.Price) float64 {
	return core.Price(f.fees.Fee(venueName, px, core.QtyOf(float64(f.clip)), false)).Cents() / float64(f.clip)
}

// FairValueResult is the fair-value study.
type FairValueResult struct {
	Samples  int     `json:"samples"`
	Markets  int     `json:"markets"`
	Clusters int     `json:"clusters"`
	GridS    int64   `json:"grid_s"`
	ClipQty  int64   `json:"clip_contracts"`
	Note     string  `json:"note,omitempty"`
	A        float64 `json:"a"`
	ASE      float64 `json:"a_se"`
	WK       float64 `json:"w_kalshi"`
	WKSE     float64 `json:"w_kalshi_se"`
	WP       float64 `json:"w_poly"`
	WPSE     float64 `json:"w_poly_se"`
	// Out-of-sample log-loss per sample; lower is better.
	CVN        int     `json:"cv_n"`
	LLFair     float64 `json:"ll_fair"`
	LLKalshi   float64 `json:"ll_kalshi"`
	LLPoly     float64 `json:"ll_poly"`
	LLAverage  float64 `json:"ll_average"`
	GainVsBest float64 `json:"gain_vs_best"` // best single venue − fair, per sample (> 0 = fair is better)
	GainSE     float64 `json:"gain_se"`
	BestSingle string  `json:"best_single"`
	Trades     []FVRow `json:"trades"`
}

// FVRow is one out-of-sample trading rule; SE clustered by league-day.
type FVRow struct {
	MarginC  float64 `json:"margin_cents"`
	Venue    string  `json:"venue"`
	Trades   int     `json:"trades"`
	Markets  int     `json:"markets"`
	PnL      float64 `json:"pnl"` // net cents per contract, held to settlement
	PnLSE    float64 `json:"pnl_se"`
	WinShare float64 `json:"win_share"`
	Edge     float64 `json:"avg_edge"` // average model edge at entry, cents: what the model expected
}

type fvRow struct {
	market, cluster string
	sec             int64
	k, p            mrBook
	y               float64
}

func fvX(r fvRow) []float64 {
	return []float64{1, logOdds(clampP(bookMid(r.k) / 100)), logOdds(clampP(bookMid(r.p) / 100))}
}

// Result fits and tests the model.
func (f *FairValue) Result() FairValueResult {
	res := FairValueResult{GridS: f.grid, ClipQty: f.clip}
	names := make([]string, 0, len(f.markets))
	for n := range f.markets {
		names = append(names, n)
	}
	sort.Strings(names)
	var rows []fvRow
	clusters := map[string]bool{}
	markets := map[string]bool{}
	for _, name := range names {
		m := f.markets[name]
		y, ok := f.outcomes[m.kKey]
		if !ok {
			if y, ok = f.outcomes[m.pKey]; !ok {
				continue
			}
		}
		cluster := name
		if parts := strings.SplitN(name, "-", 3); len(parts) == 3 {
			cluster = parts[0] + "-" + parts[1]
		}
		for _, s := range m.samples {
			pk, pp := bookMid(s.k)/100, bookMid(s.p)/100
			if pk <= 0.02 || pk >= 0.98 || pp <= 0.02 || pp >= 0.98 {
				continue // decided: no forecast left
			}
			rows = append(rows, fvRow{name, cluster, s.sec, s.k, s.p, float64(y)})
			clusters[cluster], markets[name] = true, true
		}
	}
	res.Samples, res.Markets, res.Clusters = len(rows), len(markets), len(clusters)
	if len(markets) < 30 || len(clusters) < 5 {
		res.Note = fmt.Sprintf("too little data: %d settled markets with both venues quoting, over %d league-days (need 30+ over 5+)", len(markets), len(clusters))
		return res
	}
	fit, err := fvFit(rows)
	if err != nil {
		res.Note = "fit: " + err.Error()
		return res
	}
	res.A, res.ASE, res.WK, res.WKSE, res.WP, res.WPSE = fit.Beta[0], fit.SE[0], fit.Beta[1], fit.SE[1], fit.Beta[2], fit.SE[2]
	f.crossValidate(rows, &res)
	return res
}

func fvFit(rows []fvRow) (logitFit, error) {
	X := make([][]float64, len(rows))
	y := make([]float64, len(rows))
	cl := make([]string, len(rows))
	for i, r := range rows {
		X[i], y[i], cl[i] = fvX(r), r.y, r.cluster
	}
	return fitLogit(X, y, cl)
}

func logLoss(p, y float64) float64 {
	p = clampP(p)
	return -(y*math.Log(p) + (1-y)*math.Log(1-p))
}

func (f *FairValue) crossValidate(rows []fvRow, res *FairValueResult) {
	var days []string
	seen := map[string]bool{}
	for _, r := range rows {
		if !seen[r.cluster] {
			seen[r.cluster] = true
			days = append(days, r.cluster)
		}
	}
	sort.Strings(days)
	fold := map[string]int{}
	for i, d := range days {
		fold[d] = i % 5
	}
	var ll [4]float64 // fair, kalshi, poly, average
	var gainK, gainP []float64
	var groups []string
	type tKey struct {
		margin float64
		venue  string
	}
	type trade struct {
		market, cluster string
		pnl, edge       float64
	}
	trades := map[tKey][]trade{}
	for k := 0; k < 5; k++ {
		var train, test []fvRow
		for _, r := range rows {
			if fold[r.cluster] == k {
				test = append(test, r)
			} else {
				train = append(train, r)
			}
		}
		if len(test) == 0 || len(train) < 30 {
			continue
		}
		fit, err := fvFit(train)
		if err != nil {
			continue
		}
		taken := map[string]bool{} // market|venue|margin
		for _, r := range test {
			fair := fit.predict(fvX(r))
			pk, pp := bookMid(r.k)/100, bookMid(r.p)/100
			lf, lk, lp, la := logLoss(fair, r.y), logLoss(pk, r.y), logLoss(pp, r.y), logLoss((pk+pp)/2, r.y)
			ll[0], ll[1], ll[2], ll[3] = ll[0]+lf, ll[1]+lk, ll[2]+lp, ll[3]+la
			gainK, gainP, groups = append(gainK, lk-lf), append(gainP, lp-lf), append(groups, r.cluster)
			res.CVN++
			F := fair * 100
			for _, v := range []struct {
				name string
				b    mrBook
			}{{core.VenueKalshi, r.k}, {core.VenuePoly, r.p}} {
				for _, tau := range f.margins {
					id := fmt.Sprintf("%s|%s|%g", r.market, v.name, tau)
					if taken[id] {
						continue
					}
					buyEdge := F - v.b.ask.Cents() - f.feeC(v.name, v.b.ask)
					sellEdge := v.b.bid.Cents() - F - f.feeC(v.name, v.b.bid)
					var pnl, edge float64
					switch {
					case buyEdge >= tau:
						pnl, edge = 100*r.y-v.b.ask.Cents()-f.feeC(v.name, v.b.ask), buyEdge
					case sellEdge >= tau:
						pnl, edge = v.b.bid.Cents()-100*r.y-f.feeC(v.name, v.b.bid), sellEdge
					default:
						continue
					}
					taken[id] = true
					trades[tKey{tau, v.name}] = append(trades[tKey{tau, v.name}], trade{r.market, r.cluster, pnl, edge})
				}
			}
		}
	}
	if res.CVN == 0 {
		return
	}
	n := float64(res.CVN)
	res.LLFair, res.LLKalshi, res.LLPoly, res.LLAverage = ll[0]/n, ll[1]/n, ll[2]/n, ll[3]/n
	if res.LLKalshi <= res.LLPoly {
		res.BestSingle = core.VenueKalshi
		res.GainVsBest, res.GainSE = clusteredMean(gainK, groups)
	} else {
		res.BestSingle = core.VenuePoly
		res.GainVsBest, res.GainSE = clusteredMean(gainP, groups)
	}
	for _, tau := range f.margins {
		for _, v := range []string{core.VenueKalshi, core.VenuePoly} {
			ts := trades[tKey{tau, v}]
			row := FVRow{MarginC: tau, Venue: v, Trades: len(ts)}
			var xs []float64
			var gs []string
			mk := map[string]bool{}
			wins := 0
			var edge float64
			for _, t := range ts {
				xs, gs = append(xs, t.pnl), append(gs, t.cluster)
				mk[t.market] = true
				edge += t.edge
				if t.pnl > 0 {
					wins++
				}
			}
			row.Markets = len(mk)
			row.PnL, row.PnLSE = clusteredMean(xs, gs)
			if len(ts) > 0 {
				row.WinShare, row.Edge = float64(wins)/float64(len(ts)), edge/float64(len(ts))
			}
			res.Trades = append(res.Trades, row)
		}
	}
}

// FVEdge is the usual bar: 10+ markets, 30+ trades, 95% range above zero.
func FVEdge(r FVRow) bool { return r.Markets >= 10 && r.Trades >= 30 && r.PnL-1.96*r.PnLSE > 0 }

// Report renders the study as text.
func (f *FairValue) Report() string {
	r := f.Result()
	var b strings.Builder
	fmt.Fprintf(&b, "FAIR VALUE  (P(yes) = σ(a + wK·logit(pK) + wP·logit(pP)) from both venues; %ds samples; SE clustered by league-day)\n", r.GridS)
	if r.Note != "" {
		fmt.Fprintf(&b, "  %s\n", r.Note)
		return b.String()
	}
	fmt.Fprintf(&b, "  %d samples from %d settled markets over %d league-days\n", r.Samples, r.Markets, r.Clusters)
	fmt.Fprintf(&b, "  weights: Kalshi %.3f ± %.3f, Polymarket %.3f ± %.3f  (a %+.3f ± %.3f)\n\n", r.WK, 1.96*r.WKSE, r.WP, 1.96*r.WPSE, r.A, 1.96*r.ASE)
	fmt.Fprintf(&b, "  out of sample (5 folds by day, %d samples), log-loss: fair %.4f | Kalshi %.4f | Polymarket %.4f | average %.4f\n",
		r.CVN, r.LLFair, r.LLKalshi, r.LLPoly, r.LLAverage)
	fmt.Fprintf(&b, "  fair vs best single venue (%s): %+.4f ± %.4f per sample (> 0 = fair value predicts better)\n\n", r.BestSingle, r.GainVsBest, 1.96*r.GainSE)
	fmt.Fprintf(&b, "  trading it out of sample: buy/sell where a venue strays past fair by fee + margin, hold to settlement (%d contracts)\n", r.ClipQty)
	fmt.Fprintf(&b, "  %-7s %-13s %7s %5s %18s %6s %9s\n", "margin", "venue", "trades", "mkts", "net ¢/contract", "win", "exp. edge")
	edges := 0
	for _, t := range r.Trades {
		mark := ""
		if FVEdge(t) {
			mark = "  <- edge"
			edges++
		}
		fmt.Fprintf(&b, "  %4.0f¢   %-13s %7d %5d %+8.2f ±%-8.2f %5.0f%% %8.2f¢%s\n", t.MarginC, t.Venue, t.Trades, t.Markets,
			t.PnL, 1.96*t.PnLSE, 100*t.WinShare, t.Edge, mark)
	}
	if edges == 0 {
		b.WriteString("\n  no edge: no rule with 10+ markets has a held-to-settlement P&L whose 95% range clears zero.\n")
	}
	return b.String()
}
