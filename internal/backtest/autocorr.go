package backtest

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"predbot/internal/core"
)

// Autocorr asks, per venue, whether a price change predicts the next one: at
// each horizon H, regress the mid's change over the next H on its change over
// the previous H (pooled within markets, SE clustered by market). β > 0 is
// momentum, β < 0 reversion. A trade test then follows or fades past moves of
// at least 2¢ or 5¢ over H, taker in and out at the touch, with fees.
//
// Mids sit on a 5s grid, forward-filled causally and valid only while the
// book is fresh. Everything accumulates as per-market sums, so memory is
// O(markets × horizons), not O(archive).
type Autocorr struct {
	fees     Fees
	grid     int64 // seconds
	maxAge   int64 // ns
	clip     int64
	horizons []int64   // seconds, multiples of grid
	thresh   []float64 // cents

	series map[string]*acSeries // "venue|venueID"
	stats  map[acKey]*acAcc     // per venue × horizon × market
	trades map[acTKey]*acTAcc   // per venue × horizon × threshold × market
}

type acKey struct {
	venue  string
	h      int64
	market string
}

type acTKey struct {
	venue  string
	h      int64
	thr    float64
	market string
}

// acAcc holds regression sums of future (y) on past (x) changes.
type acAcc struct {
	n                     int
	sx, sy, sxx, sxy, syy float64
}

// acTAcc holds per-market trade sums, enough for a clustered mean.
type acTAcc struct {
	n            int
	follow, fade float64
}

type acPoint struct {
	mid      float64
	bid, ask core.Price
}

type acSeries struct {
	book     mrBook
	nextGrid int64
	started  bool
	pts      map[int64]acPoint // grid second -> valid sample, trimmed to 2·max horizon
	market   string
	venue    string
}

// NewAutocorr uses a 5s grid, 5s freshness, 10-contract fees, horizons of 5s,
// 30s, 2m and 10m, and trade thresholds of 2¢ and 5¢.
func NewAutocorr(fees Fees) *Autocorr {
	return &Autocorr{fees: fees, grid: 5, maxAge: 5e9, clip: 10,
		horizons: []int64{5, 30, 120, 600}, thresh: []float64{2, 5},
		series: map[string]*acSeries{}, stats: map[acKey]*acAcc{}, trades: map[acTKey]*acTAcc{}}
}

// Observe feeds one snapshot; market is the canonical id (or any stable key).
func (a *Autocorr) Observe(market string, s core.BookSnapshot) {
	k := s.Venue + "|" + s.VenueID
	sr := a.series[k]
	if sr == nil {
		sr = &acSeries{pts: map[int64]acPoint{}, market: market, venue: s.Venue}
		a.series[k] = sr
	}
	sec := s.TsRecv / 1e9
	if !sr.started {
		sr.nextGrid, sr.started = sec+a.grid, true
	}
	for sr.nextGrid <= sec {
		a.sample(sr, sr.nextGrid)
		sr.nextGrid += a.grid
	}
	b := mrBook{ts: s.TsRecv}
	bid, okB := s.BestYesBid()
	ask, okA := s.BestYesAsk()
	if okB && okA && ask.Px > bid.Px {
		b.bid, b.ask, b.ok = bid.Px, ask.Px, true
	}
	sr.book = b
}

func (a *Autocorr) feeC(venueName string, px core.Price) float64 {
	return float64(a.fees.Fee(venueName, px, core.Qty(a.clip*100), false)) / 100 / float64(a.clip)
}

func (a *Autocorr) sample(sr *acSeries, sec int64) {
	maxH := a.horizons[len(a.horizons)-1]
	delete(sr.pts, sec-2*maxH-a.grid)
	if !sr.book.ok || sec*1e9-sr.book.ts > a.maxAge {
		return
	}
	now := acPoint{mid: bookMid(sr.book), bid: sr.book.bid, ask: sr.book.ask}
	sr.pts[sec] = now
	// Evaluate the anchor one horizon back: its past change and what followed.
	for _, h := range a.horizons {
		anchor, okA := sr.pts[sec-h]
		before, okB := sr.pts[sec-2*h]
		if !okA || !okB {
			continue
		}
		past, future := anchor.mid-before.mid, now.mid-anchor.mid
		k := acKey{sr.venue, h, sr.market}
		acc := a.stats[k]
		if acc == nil {
			acc = &acAcc{}
			a.stats[k] = acc
		}
		acc.n++
		acc.sx += past
		acc.sy += future
		acc.sxx += past * past
		acc.sxy += past * future
		acc.syy += future * future
		for _, thr := range a.thresh {
			if math.Abs(past) < thr {
				continue
			}
			d := sign(past)
			var follow, fade float64
			if d > 0 { // price rose: follow = buy at anchor's ask, sell at now's bid
				follow = cents(now.bid) - cents(anchor.ask) - a.feeC(sr.venue, anchor.ask) - a.feeC(sr.venue, now.bid)
				fade = cents(anchor.bid) - cents(now.ask) - a.feeC(sr.venue, anchor.bid) - a.feeC(sr.venue, now.ask)
			} else {
				follow = cents(anchor.bid) - cents(now.ask) - a.feeC(sr.venue, anchor.bid) - a.feeC(sr.venue, now.ask)
				fade = cents(now.bid) - cents(anchor.ask) - a.feeC(sr.venue, anchor.ask) - a.feeC(sr.venue, now.bid)
			}
			tk := acTKey{sr.venue, h, thr, sr.market}
			t := a.trades[tk]
			if t == nil {
				t = &acTAcc{}
				a.trades[tk] = t
			}
			t.n++
			t.follow += follow
			t.fade += fade
		}
	}
}

// AutocorrResult is the price time-series study.
type AutocorrResult struct {
	GridS   int64     `json:"grid_s"`
	ClipQty int64     `json:"clip_contracts"`
	Fits    []ACFit   `json:"fits"`   // per venue × horizon
	Trades  []MoveRow `json:"trades"` // same shape as the overreaction rows
}

// ACFit is the pooled within-market regression of the next change on the
// last at one horizon, for one venue.
type ACFit struct {
	Venue    string  `json:"venue"`
	HorizonS int64   `json:"horizon_s"`
	Pairs    int     `json:"pairs"`
	Markets  int     `json:"markets"`
	Beta     float64 `json:"beta"` // + momentum, − reversion
	BetaSE   float64 `json:"beta_se"`
	PastSD   float64 `json:"past_sd"` // typical |change| over the horizon, cents
}

// clusteredFromSums is clusteredMean computed from per-group sums: group g
// contributes n_g observations summing to s_g.
func clusteredFromSums(ns []int, ss []float64) (mean, se float64) {
	var n, s float64
	for i := range ns {
		n += float64(ns[i])
		s += ss[i]
	}
	if n == 0 {
		return 0, 0
	}
	mean = s / n
	g := float64(len(ns))
	if g < 2 {
		return mean, 0
	}
	var acc float64
	for i := range ns {
		r := ss[i] - float64(ns[i])*mean
		acc += r * r
	}
	return mean, math.Sqrt(g/(g-1)*acc) / n
}

// Result pools the per-market sums.
func (a *Autocorr) Result() AutocorrResult {
	res := AutocorrResult{GridS: a.grid, ClipQty: a.clip}
	type vh struct {
		venue string
		h     int64
	}
	groups := map[vh][]*acAcc{}
	for k, acc := range a.stats {
		groups[vh{k.venue, k.h}] = append(groups[vh{k.venue, k.h}], acc)
	}
	for key, accs := range groups {
		fit := ACFit{Venue: key.venue, HorizonS: key.h}
		var num, den, sxxAll, sxAll float64
		type part struct{ sxxT, sxyT float64 }
		var parts []part
		for _, acc := range accs {
			if acc.n < 10 {
				continue
			}
			n := float64(acc.n)
			sxxT := acc.sxx - acc.sx*acc.sx/n
			sxyT := acc.sxy - acc.sx*acc.sy/n
			if sxxT <= 0 {
				continue
			}
			num += sxyT
			den += sxxT
			parts = append(parts, part{sxxT, sxyT})
			fit.Pairs += acc.n
			sxxAll += acc.sxx
			sxAll += acc.sx
		}
		fit.Markets = len(parts)
		if den > 0 {
			fit.Beta = num / den
			var ss float64
			for _, p := range parts {
				sc := p.sxyT - fit.Beta*p.sxxT
				ss += sc * sc
			}
			if g := float64(len(parts)); g >= 2 {
				fit.BetaSE = math.Sqrt(g/(g-1)*ss) / den
			}
			n := float64(fit.Pairs)
			fit.PastSD = math.Sqrt(math.Max(sxxAll/n-(sxAll/n)*(sxAll/n), 0))
		}
		res.Fits = append(res.Fits, fit)
	}
	sort.Slice(res.Fits, func(i, j int) bool {
		if res.Fits[i].Venue != res.Fits[j].Venue {
			return res.Fits[i].Venue < res.Fits[j].Venue
		}
		return res.Fits[i].HorizonS < res.Fits[j].HorizonS
	})

	type vht struct {
		venue string
		h     int64
		thr   float64
	}
	tg := map[vht][]*acTAcc{}
	for k, t := range a.trades {
		tg[vht{k.venue, k.h, k.thr}] = append(tg[vht{k.venue, k.h, k.thr}], t)
	}
	for key, ts := range tg {
		row := MoveRow{Trigger: "autocorr", Venue: key.venue, Bucket: fmt.Sprintf("≥%.0f¢", key.thr), BucketLo: key.thr, HorizonS: key.h, Markets: len(ts)}
		var ns []int
		var fol, fad []float64
		for _, t := range ts {
			ns = append(ns, t.n)
			fol = append(fol, t.follow)
			fad = append(fad, t.fade)
			row.Events += t.n
		}
		row.Follow, row.FollowSE = clusteredFromSums(ns, fol)
		row.Fade, row.FadeSE = clusteredFromSums(ns, fad)
		res.Trades = append(res.Trades, row)
	}
	sort.Slice(res.Trades, func(i, j int) bool {
		a, b := res.Trades[i], res.Trades[j]
		if a.Venue != b.Venue {
			return a.Venue < b.Venue
		}
		if a.BucketLo != b.BucketLo {
			return a.BucketLo < b.BucketLo
		}
		return a.HorizonS < b.HorizonS
	})
	return res
}

// ACVerdict reads a fit: "momentum" or "reversion" when the 95% range excludes
// zero, else "none".
func ACVerdict(f ACFit) string {
	if f.Markets < 10 {
		return "none"
	}
	switch {
	case f.Beta-1.96*f.BetaSE > 0:
		return "momentum"
	case f.Beta+1.96*f.BetaSE < 0:
		return "reversion"
	}
	return "none"
}

// Report renders the study as text.
func (a *Autocorr) Report() string {
	r := a.Result()
	var b strings.Builder
	fmt.Fprintf(&b, "PRICE MOMENTUM / REVERSION  (next change regressed on the last, %ds grid, pooled within markets)\n", r.GridS)
	fmt.Fprintf(&b, "  β > 0 momentum, β < 0 reversion; ± = 95%% range clustered by market\n\n")
	if len(r.Fits) == 0 {
		b.WriteString("  no series had enough continuous samples.\n")
		return b.String()
	}
	fmt.Fprintf(&b, "  %-13s %-6s %9s %5s %18s %8s  %s\n", "venue", "over", "pairs", "mkts", "β", "|Δ| sd", "reads as")
	for _, f := range r.Fits {
		fmt.Fprintf(&b, "  %-13s %-6s %9d %5d %+8.4f ±%-8.4f %7.2f¢  %s\n", f.Venue, horizonLabel(f.HorizonS), f.Pairs, f.Markets,
			f.Beta, 1.96*f.BetaSE, f.PastSD, ACVerdict(f))
	}
	fmt.Fprintf(&b, "\n  trading it (%d contracts, taker in and out, hold one horizon): net ¢/contract\n", r.ClipQty)
	fmt.Fprintf(&b, "  %-13s %-6s %-5s %8s %5s %16s %16s\n", "venue", "over", "past", "trades", "mkts", "follow", "fade")
	edges := 0
	for _, t := range r.Trades {
		mark := ""
		if s, ok := MoveEdge(t); ok {
			mark = "  <- " + s + " edge"
			edges++
		}
		fmt.Fprintf(&b, "  %-13s %-6s %-5s %8d %5d %+7.2f ±%-7.2f %+7.2f ±%-7.2f%s\n", t.Venue, horizonLabel(t.HorizonS), t.Bucket,
			t.Events, t.Markets, t.Follow, 1.96*t.FollowSE, t.Fade, 1.96*t.FadeSE, mark)
	}
	if edges == 0 {
		b.WriteString("\n  no edge: no rule with 10+ markets clears zero after costs.\n")
	}
	return b.String()
}
