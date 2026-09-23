package backtest

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"predbot/internal/core"
)

// SpreadMR models each market's cross-venue gap, s = Kalshi mid − Poly mid
// (cents), as a mean-reverting (Ornstein–Uhlenbeck) process sampled on a fixed
// grid, and tests trading it:
//
//   - Fit: Δs = a + b·s(t−Δ) + ε per market. The pull-back speed is
//     θ = −ln(1+b)/Δ, the half-life ln2/θ, and the gap's typical excursion (its
//     stationary spread) σ_eq = sd(ε)/√(1−(1+b)²). A pooled fit with a
//     per-market level (within estimator) gives one half-life for all markets,
//     with a standard error clustered by market.
//   - Trade: when the gap strays k·σ from its trailing (EWMA) level, sell the
//     rich venue's YES at its bid and buy the cheap venue's at its ask; close
//     both at the touch when the gap is back within 0.5σ, or after a max hold.
//     The level and σ are causal — only the past — so there's no look-ahead.
//
// Streaming: regression sums and one open trade per strategy per market, no
// stored series.
type SpreadMR struct {
	fees     Fees
	grid     int64   // sampling interval, seconds
	maxAge   int64   // ns; a venue's book older than this makes the sample invalid
	clip     int64   // contracts per trade, for fee rounding
	halfLife float64 // EWMA half-life of the trailing level, seconds
	warmup   int     // samples before a market may trade
	exitZ    float64
	strats   []mrStrat

	markets map[string]*mrMarket
}

type mrStrat struct {
	k       float64 // entry at |z| ≥ k
	maxHold int64   // seconds
}

type mrBook struct {
	bid, ask core.Price
	ts       int64 // ns
	ok       bool
}

type mrMarket struct {
	k, p     mrBook // latest Kalshi and Poly books
	nextGrid int64  // next grid time, seconds
	started  bool

	prevOK  bool
	prev    float64 // previous valid sample's gap
	prevSec int64

	// Regression sums for Δs on s(t−Δ).
	n                     int
	sx, sy, sxx, sxy, syy float64
	gapSum, gapSumSq      float64
	gapN                  int

	// Trailing level and variance (EWMA) for the trading z-score.
	ewm, ewv float64
	seen     int

	open   []*mrTrade // one slot per strategy
	trades [][]float64
	holds  [][]float64
}

type mrTrade struct {
	sec    int64
	dir    int // +1 = gap too wide (Kalshi rich): sell Kalshi, buy Poly
	k0, p0 mrBook
	entryZ float64
}

// NewSpreadMR samples every 5s, needs both books fresher than 5s, trades 10
// contracts, and tests entries at 1.5/2/3σ with 5- and 30-minute max holds.
func NewSpreadMR(fees Fees) *SpreadMR {
	m := &SpreadMR{fees: fees, grid: 5, maxAge: 5e9, clip: 10, halfLife: 1800, warmup: 60, exitZ: 0.5,
		markets: map[string]*mrMarket{}}
	for _, k := range []float64{1.5, 2, 3} {
		for _, h := range []int64{300, 1800} {
			m.strats = append(m.strats, mrStrat{k, h})
		}
	}
	return m
}

func (m *SpreadMR) market(canonical string) *mrMarket {
	mk := m.markets[canonical]
	if mk == nil {
		n := len(m.strats)
		mk = &mrMarket{open: make([]*mrTrade, n), trades: make([][]float64, n), holds: make([][]float64, n)}
		m.markets[canonical] = mk
	}
	return mk
}

// Observe feeds one snapshot, already mapped to its canonical market.
func (m *SpreadMR) Observe(canonical string, s core.BookSnapshot) {
	if s.Venue != "kalshi" && s.Venue != "polymarketus" {
		return
	}
	mk := m.market(canonical)
	sec := s.TsRecv / 1e9
	if !mk.started {
		mk.nextGrid, mk.started = sec+m.grid, true
	}
	// Sample every grid point this snapshot passed, from the books as they
	// stood then (before applying it): a causal forward fill.
	for mk.nextGrid <= sec {
		m.sample(mk, mk.nextGrid)
		mk.nextGrid += m.grid
	}
	b := mrBook{ts: s.TsRecv}
	bid, okB := s.BestYesBid()
	ask, okA := s.BestYesAsk()
	if okB && okA && ask.Px > bid.Px {
		b.bid, b.ask, b.ok = bid.Px, ask.Px, true
	}
	if s.Venue == "kalshi" {
		mk.k = b
	} else {
		mk.p = b
	}
}

func bookMid(b mrBook) float64 { return (cents(b.bid) + cents(b.ask)) / 2 }

func (m *SpreadMR) sample(mk *mrMarket, sec int64) {
	ns := sec * 1e9
	fresh := func(b mrBook) bool { return b.ok && ns-b.ts <= m.maxAge }
	if !fresh(mk.k) || !fresh(mk.p) {
		mk.prevOK = false
		return
	}
	gap := bookMid(mk.k) - bookMid(mk.p)
	mk.gapSum += gap
	mk.gapSumSq += gap * gap
	mk.gapN++
	if mk.prevOK && mk.prevSec == sec-m.grid {
		x, y := mk.prev, gap-mk.prev
		mk.n++
		mk.sx += x
		mk.sy += y
		mk.sxx += x * x
		mk.sxy += x * y
		mk.syy += y * y
	}
	mk.prev, mk.prevSec, mk.prevOK = gap, sec, true

	// Trailing level and variance.
	alpha := 1 - math.Exp(-math.Ln2*float64(m.grid)/m.halfLife)
	if mk.seen == 0 {
		mk.ewm, mk.ewv = gap, 0
	} else {
		d := gap - mk.ewm
		mk.ewm += alpha * d
		mk.ewv = (1 - alpha) * (mk.ewv + alpha*d*d)
	}
	mk.seen++
	sd := math.Sqrt(mk.ewv)
	if mk.seen < m.warmup || sd < 0.25 {
		return // not warmed up, or the gap barely moves: z would be noise
	}
	z := (gap - mk.ewm) / sd
	for i, st := range m.strats {
		if t := mk.open[i]; t != nil {
			if math.Abs(z) <= m.exitZ || sec-t.sec >= st.maxHold {
				mk.trades[i] = append(mk.trades[i], m.closeTrade(t, mk.k, mk.p))
				mk.holds[i] = append(mk.holds[i], float64(sec-t.sec))
				mk.open[i] = nil
			}
			continue
		}
		if math.Abs(z) >= st.k {
			mk.open[i] = &mrTrade{sec: sec, dir: sign(z), k0: mk.k, p0: mk.p, entryZ: z}
		}
	}
}

// feeC is the per-contract taker fee in cents at a clip of m.clip contracts.
func (m *SpreadMR) feeC(venueName string, px core.Price) float64 {
	return float64(m.fees.Fee(venueName, px, core.Qty(m.clip*100), false)) / 100 / float64(m.clip)
}

// closeTrade is the per-contract-pair P&L in cents, both legs taker, in and out.
func (m *SpreadMR) closeTrade(t *mrTrade, k1, p1 mrBook) float64 {
	if t.dir > 0 { // gap wide: sold Kalshi at bid, bought Poly at ask; now buy Kalshi back, sell Poly
		return cents(t.k0.bid) - cents(k1.ask) + cents(p1.bid) - cents(t.p0.ask) -
			m.feeC("kalshi", t.k0.bid) - m.feeC("kalshi", k1.ask) - m.feeC("polymarketus", t.p0.ask) - m.feeC("polymarketus", p1.bid)
	}
	return cents(k1.bid) - cents(t.k0.ask) + cents(t.p0.bid) - cents(p1.ask) -
		m.feeC("kalshi", t.k0.ask) - m.feeC("kalshi", k1.bid) - m.feeC("polymarketus", t.p0.bid) - m.feeC("polymarketus", p1.ask)
}

// SpreadMRResult is the mean-reversion study, for the text report and the
// backtest runs page. Gaps and P&L are in cents; times in seconds.
type SpreadMRResult struct {
	GridS   int64 `json:"grid_s"`
	ClipQty int64 `json:"clip_contracts"`
	Markets int   `json:"markets"` // with enough two-venue samples to fit
	Unfit   int   `json:"unfit"`   // markets seen but too thin to fit
	// Pooled within-market fit: one pull-back coefficient for all markets.
	PooledB      float64 `json:"pooled_b"`
	PooledBSE    float64 `json:"pooled_b_se"`
	HalfLifeS    float64 `json:"half_life_s"`    // from PooledB; 0 if not mean-reverting
	HalfLifeLoS  float64 `json:"half_life_lo_s"` // 95% range, from b ± 1.96·SE
	HalfLifeHiS  float64 `json:"half_life_hi_s"` // 0 = unbounded (can't rule out no reversion)
	MedianHLS    float64 `json:"median_half_life_s"`
	MedianSigma  float64 `json:"median_sigma_eq"`  // typical excursion of the gap
	MedianAbsGap float64 `json:"median_abs_level"` // typical |mean gap| (a persistent venue offset)
	HalfLifeHist []MRBin `json:"half_life_hist"`
	Strategies   []MRRow `json:"strategies"`
	MarketFits   []MRMkt `json:"market_fits"` // per market, most samples first
}

// MRBin counts markets whose half-life falls in [LoS, HiS).
type MRBin struct {
	Label string  `json:"label"`
	LoS   float64 `json:"lo_s"`
	HiS   float64 `json:"hi_s"` // 0 = open-ended
	N     int     `json:"n"`
}

// MRRow is one trading rule's results; SE clustered by market.
type MRRow struct {
	K        float64 `json:"k"`
	MaxHoldS int64   `json:"max_hold_s"`
	Trades   int     `json:"trades"`
	Markets  int     `json:"markets"`
	PnL      float64 `json:"pnl"` // net cents per contract pair
	PnLSE    float64 `json:"pnl_se"`
	WinShare float64 `json:"win_share"`
	AvgHoldS float64 `json:"avg_hold_s"`
}

// MRMkt is one market's own fit.
type MRMkt struct {
	Canonical string  `json:"canonical"`
	N         int     `json:"n"`
	B         float64 `json:"b"`
	HalfLifeS float64 `json:"half_life_s"` // 0 = no reversion measured
	SigmaEq   float64 `json:"sigma_eq"`
	MeanGap   float64 `json:"mean_gap"`
	SDGap     float64 `json:"sd_gap"`
}

// halfLife converts an AR coefficient on a Δ grid to a half-life in seconds;
// 0 when b gives no reversion (b ≥ 0) or overshoots (b ≤ −1).
func halfLife(b float64, grid int64) float64 {
	if b >= 0 || b <= -1 {
		return 0
	}
	return math.Ln2 / (-math.Log(1+b) / float64(grid))
}

var hlBins = []MRBin{
	{Label: "< 10s", LoS: 0, HiS: 10}, {Label: "10–30s", LoS: 10, HiS: 30}, {Label: "30s–1m", LoS: 30, HiS: 60},
	{Label: "1–5m", LoS: 60, HiS: 300}, {Label: "5–30m", LoS: 300, HiS: 1800}, {Label: "30m+", LoS: 1800},
	{Label: "no reversion", LoS: -1, HiS: -1},
}

// Result fits the model and summarises the trades.
func (m *SpreadMR) Result() SpreadMRResult {
	res := SpreadMRResult{GridS: m.grid, ClipQty: m.clip}
	var num, den float64 // pooled within estimator
	type fitted struct {
		name       string
		sxxT, sxyT float64
	}
	var fits []fitted
	var hls, sigmas, levels []float64
	bins := append([]MRBin{}, hlBins...)
	for name, mk := range m.markets {
		if mk.n < 60 {
			if mk.gapN > 0 {
				res.Unfit++
			}
			continue
		}
		n := float64(mk.n)
		sxxT := mk.sxx - mk.sx*mk.sx/n // centred sums
		sxyT := mk.sxy - mk.sx*mk.sy/n
		syyT := mk.syy - mk.sy*mk.sy/n
		if sxxT <= 0 {
			res.Unfit++
			continue
		}
		res.Markets++
		b := sxyT / sxxT
		num += sxyT
		den += sxxT
		fits = append(fits, fitted{name, sxxT, sxyT})
		resid := math.Max(syyT-b*sxyT, 0) / math.Max(n-2, 1)
		sig := 0.0
		if phi := 1 + b; phi*phi < 1 {
			sig = math.Sqrt(resid / (1 - phi*phi))
		}
		hl := halfLife(b, m.grid)
		gm := mk.gapSum / float64(mk.gapN)
		res.MarketFits = append(res.MarketFits, MRMkt{Canonical: name, N: mk.n, B: b, HalfLifeS: hl, SigmaEq: sig,
			MeanGap: gm, SDGap: math.Sqrt(math.Max(mk.gapSumSq/float64(mk.gapN)-gm*gm, 0))})
		if hl > 0 {
			hls = append(hls, hl)
			sigmas = append(sigmas, sig)
		}
		levels = append(levels, math.Abs(gm))
		for i := range bins {
			if (hl == 0 && bins[i].LoS < 0) || (hl > 0 && bins[i].LoS >= 0 && hl >= bins[i].LoS && (bins[i].HiS == 0 || hl < bins[i].HiS)) {
				bins[i].N++
				break
			}
		}
	}
	res.HalfLifeHist = bins
	if den > 0 {
		res.PooledB = num / den
		var ss float64
		for _, f := range fits {
			score := f.sxyT - res.PooledB*f.sxxT
			ss += score * score
		}
		g := float64(len(fits))
		if g >= 2 {
			res.PooledBSE = math.Sqrt(g/(g-1)*ss) / den
		}
		res.HalfLifeS = halfLife(res.PooledB, m.grid)
		// A faster pull-back (more negative b) is a shorter half-life.
		res.HalfLifeLoS = halfLife(res.PooledB-1.96*res.PooledBSE, m.grid)
		res.HalfLifeHiS = halfLife(res.PooledB+1.96*res.PooledBSE, m.grid)
	}
	res.MedianHLS, res.MedianSigma, res.MedianAbsGap = median(hls), median(sigmas), median(levels)
	sort.Slice(res.MarketFits, func(i, j int) bool {
		if res.MarketFits[i].N != res.MarketFits[j].N {
			return res.MarketFits[i].N > res.MarketFits[j].N
		}
		return res.MarketFits[i].Canonical < res.MarketFits[j].Canonical
	})

	for i, st := range m.strats {
		row := MRRow{K: st.k, MaxHoldS: st.maxHold}
		var xs, holds []float64
		var gs []string
		for name, mk := range m.markets {
			for j, p := range mk.trades[i] {
				xs = append(xs, p)
				gs = append(gs, name)
				holds = append(holds, mk.holds[i][j])
			}
		}
		row.Trades = len(xs)
		row.Markets = len(uniq(gs))
		row.PnL, row.PnLSE = clusteredMean(xs, gs)
		wins := 0
		var hsum float64
		for j, p := range xs {
			if p > 0 {
				wins++
			}
			hsum += holds[j]
		}
		if len(xs) > 0 {
			row.WinShare, row.AvgHoldS = float64(wins)/float64(len(xs)), hsum/float64(len(xs))
		}
		res.Strategies = append(res.Strategies, row)
	}
	return res
}

func uniq(xs []string) map[string]bool {
	u := map[string]bool{}
	for _, x := range xs {
		u[x] = true
	}
	return u
}

// MREdge applies the same bar as the other studies: 10+ markets, 30+ trades,
// and a 95% range that clears zero after costs.
func MREdge(r MRRow) bool {
	return r.Markets >= 10 && r.Trades >= 30 && r.PnL-1.96*r.PnLSE > 0
}

func fmtDur(s float64) string {
	switch {
	case s <= 0:
		return "—"
	case s < 90:
		return fmt.Sprintf("%.0fs", s)
	case s < 5400:
		return fmt.Sprintf("%.1fm", s/60)
	default:
		return fmt.Sprintf("%.1fh", s/3600)
	}
}

// Report renders the study as text.
func (m *SpreadMR) Report() string {
	r := m.Result()
	var b strings.Builder
	fmt.Fprintf(&b, "SPREAD MEAN-REVERSION  (Kalshi − Poly mid gap as an OU process, %ds grid)\n", r.GridS)
	if r.Markets == 0 {
		fmt.Fprintf(&b, "  no market had enough two-venue samples to fit (%d seen).\n", r.Unfit)
		return b.String()
	}
	fmt.Fprintf(&b, "  markets fitted: %d  (too thin: %d)\n", r.Markets, r.Unfit)
	hi := fmtDur(r.HalfLifeHiS)
	if r.HalfLifeHiS == 0 {
		hi = "∞"
	}
	fmt.Fprintf(&b, "  pooled pull-back b = %+.4f ± %.4f per %ds  ->  half-life %s (95%%: %s – %s)\n",
		r.PooledB, 1.96*r.PooledBSE, r.GridS, fmtDur(r.HalfLifeS), fmtDur(r.HalfLifeLoS), hi)
	fmt.Fprintf(&b, "  per market: median half-life %s, median excursion σ_eq %.2f¢, median |level| %.2f¢\n\n",
		fmtDur(r.MedianHLS), r.MedianSigma, r.MedianAbsGap)
	fmt.Fprintf(&b, "  half-life  ")
	for _, bin := range r.HalfLifeHist {
		fmt.Fprintf(&b, "%s: %d   ", bin.Label, bin.N)
	}
	fmt.Fprintf(&b, "\n\n  trading the gap (%d contracts, taker both legs in and out, trailing level, exit at |z|≤%.1f):\n", r.ClipQty, m.exitZ)
	fmt.Fprintf(&b, "  %-6s %-8s %7s %5s %18s %6s %8s\n", "entry", "max hold", "trades", "mkts", "net ¢/pair", "win", "avg hold")
	edges := 0
	for _, s := range r.Strategies {
		mark := ""
		if MREdge(s) {
			mark = "  <- edge"
			edges++
		}
		fmt.Fprintf(&b, "  %4.1fσ   %-8s %7d %5d %+8.2f ±%-8.2f %5.0f%% %8s%s\n",
			s.K, fmtDur(float64(s.MaxHoldS)), s.Trades, s.Markets, s.PnL, 1.96*s.PnLSE, 100*s.WinShare, fmtDur(s.AvgHoldS), mark)
	}
	if edges == 0 {
		b.WriteString("\n  no edge: no rule with 10+ markets has a net P&L whose 95% range clears zero.\n")
	}
	return b.String()
}
