package backtest

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"predbot/internal/core"
)

// LeadLag measures whether one venue's YES mid moves BEFORE the other's, per
// canonical market. It resamples each venue's mid onto a fixed 1-second grid and
// computes the cross-correlation of the two venues' 1-step returns across a range
// of lags. A correlation peak at lag τ>0 — corr(ΔKalshi[t], ΔPoly[t+τ]) — means
// KALSHI LEADS Polymarket by τ seconds (Kalshi's move shows up in Poly τ seconds
// later); τ<0 means Poly leads. A peak at 0 means they move together (no lead).
//
// Read-only analysis: it never trades and never writes. It answers "does one
// venue lead the other, and by how much" — the predictive-signal question the
// pure-arb strategy ignores.
type LeadLag struct {
	gridSec   int64 // resample interval, seconds
	maxLag    int   // scan ±maxLag grid steps
	minPoints int   // ignore markets with fewer aligned grid points
	corrFloor float64
	m         map[string]*llMarket
}

type llMarket struct {
	k map[int64]float64 // gridSecond -> last Kalshi YES mid (cents) in that second
	p map[int64]float64 // gridSecond -> last Poly   YES mid (cents) in that second
}

// NewLeadLag builds an analyzer: 1s grid, ±20s scan, ≥120 aligned points, and a
// 0.10 correlation floor below which a market is reported as "no signal".
func NewLeadLag() *LeadLag {
	return &LeadLag{gridSec: 1, maxLag: 20, minPoints: 120, corrFloor: 0.10,
		m: map[string]*llMarket{}}
}

// Observe records one venue's YES mid (cents) at receive time tsRecvNs. The
// caller skips one-sided books (no valid mid). Last write within a grid second
// wins — we want the venue's state at the end of each second.
func (l *LeadLag) Observe(canonical, venue string, midCents float64, tsRecvNs int64) {
	sec := tsRecvNs / 1_000_000_000 / l.gridSec
	mm := l.m[canonical]
	if mm == nil {
		mm = &llMarket{k: map[int64]float64{}, p: map[int64]float64{}}
		l.m[canonical] = mm
	}
	switch venue {
	case core.VenueKalshi:
		mm.k[sec] = midCents
	case core.VenuePoly:
		mm.p[sec] = midCents
	}
}

type llResult struct {
	canonical          string
	n                  int
	peakLag            int     // grid steps; + = Kalshi leads
	peakCorr, zeroCorr float64 // corr at peak lag and at lag 0
}

// forwardFill materialises a venue's per-second map into a dense slice over
// [lo,hi], carrying the last observed value forward across gaps.
func forwardFill(vals map[int64]float64, lo, hi int64) []float64 {
	out := make([]float64, 0, hi-lo+1)
	var last float64
	have := false
	for s := lo; s <= hi; s++ {
		if v, ok := vals[s]; ok {
			last, have = v, true
		}
		if !have {
			out = append(out, math.NaN())
		} else {
			out = append(out, last)
		}
		_ = s
	}
	return out
}

// diffs returns 1-step differences; a NaN on either side yields a NaN return.
func diffs(x []float64) []float64 {
	if len(x) < 2 {
		return nil
	}
	d := make([]float64, len(x)-1)
	for i := 1; i < len(x); i++ {
		d[i-1] = x[i] - x[i-1]
	}
	return d
}

// pearsonLag computes the Pearson correlation of a[t] against b[t+lag], over the
// overlapping range, skipping any pair with a NaN. Returns 0 if too few pairs or
// either side is flat (zero variance) — a flat book carries no lead information.
func pearsonLag(a, b []float64, lag int) float64 {
	var sx, sy, sxx, syy, sxy float64
	var n int
	for t := 0; t < len(a); t++ {
		u := t + lag
		if u < 0 || u >= len(b) {
			continue
		}
		x, y := a[t], b[u]
		if math.IsNaN(x) || math.IsNaN(y) {
			continue
		}
		sx += x
		sy += y
		sxx += x * x
		syy += y * y
		sxy += x * y
		n++
	}
	if n < 30 {
		return 0
	}
	cov := sxy - sx*sy/float64(n)
	vx := sxx - sx*sx/float64(n)
	vy := syy - sy*sy/float64(n)
	if vx <= 0 || vy <= 0 {
		return 0
	}
	return cov / math.Sqrt(vx*vy)
}

func (l *LeadLag) analyze(canonical string, mm *llMarket) (llResult, bool) {
	if len(mm.k) == 0 || len(mm.p) == 0 {
		return llResult{}, false // needs both venues
	}
	// Common window: from the first second BOTH venues have been seen, to the
	// last second EITHER was seen.
	lo := int64(math.MaxInt64)
	hi := int64(math.MinInt64)
	firstK, firstP := int64(math.MaxInt64), int64(math.MaxInt64)
	for s := range mm.k {
		if s < firstK {
			firstK = s
		}
		if s > hi {
			hi = s
		}
	}
	for s := range mm.p {
		if s < firstP {
			firstP = s
		}
		if s > hi {
			hi = s
		}
	}
	lo = firstK
	if firstP > lo {
		lo = firstP
	}
	if hi-lo+1 < int64(l.minPoints) {
		return llResult{}, false
	}
	k := forwardFill(mm.k, lo, hi)
	p := forwardFill(mm.p, lo, hi)
	dk, dp := diffs(k), diffs(p)

	res := llResult{canonical: canonical, n: len(dk)}
	res.zeroCorr = pearsonLag(dk, dp, 0)
	best := 0.0
	for lag := -l.maxLag; lag <= l.maxLag; lag++ {
		c := pearsonLag(dk, dp, lag)
		if math.Abs(c) > math.Abs(best) {
			best, res.peakLag, res.peakCorr = c, lag, c
		}
	}
	return res, true
}

// Report runs the analysis over every market and renders a text summary.
// LeadLagResult is the lead-lag analysis, for the text report and the backtest
// runs page.
type LeadLagResult struct {
	GridSec   int64   `json:"grid_s"`     // resample interval
	MaxLagS   int64   `json:"max_lag_s"`  // lags scanned: ±MaxLagS
	CorrFloor float64 `json:"corr_floor"` // |corr| below this counts as no signal
	Analyzed  int     `json:"analyzed"`
	// Of the markets with signal: which venue moved first, or neither.
	KalshiLeads     int `json:"kalshi_leads"`
	PolyLeads       int `json:"poly_leads"`
	Contemporaneous int `json:"contemporaneous"`
	NoSignal        int `json:"no_signal"`
	// Over markets with a lagged signal; 0 when there are none.
	MedianLeadS    float64 `json:"median_lead_s"` // + = Kalshi leads
	MedianPeakCorr float64 `json:"median_peak_corr"`
	// Every analyzed market, strongest |peak corr| first.
	Markets []LeadLagMarket `json:"markets"`
}

// LeadLagMarket is one market's cross-correlation of YES-mid changes.
type LeadLagMarket struct {
	Canonical string  `json:"canonical"`
	PeakLagS  int64   `json:"peak_lag_s"` // + = Kalshi leads Poly
	PeakCorr  float64 `json:"peak_corr"`
	ZeroCorr  float64 `json:"lag0_corr"`
	N         int     `json:"n"` // aligned grid points
	Signal    bool    `json:"signal"`
}

// Result computes the lead-lag analysis over every market with enough overlap.
func (l *LeadLag) Result() LeadLagResult {
	res := LeadLagResult{GridSec: l.gridSec, MaxLagS: int64(l.maxLag) * l.gridSec, CorrFloor: l.corrFloor}
	var results []llResult
	for c, mm := range l.m {
		if r, ok := l.analyze(c, mm); ok {
			results = append(results, r)
		}
	}
	res.Analyzed = len(results)
	sort.Slice(results, func(i, j int) bool {
		a, b := math.Abs(results[i].peakCorr), math.Abs(results[j].peakCorr)
		if a != b {
			return a > b
		}
		return results[i].canonical < results[j].canonical
	})
	// Tally direction only among markets with a real peak (|corr| >= floor and a
	// non-zero lag). A peak at lag 0 is "contemporaneous" — neither leads.
	var leadLags []float64 // signed peak lag (seconds) for markets with signal
	var peakCorrs []float64
	for _, r := range results {
		signal := math.Abs(r.peakCorr) >= l.corrFloor
		res.Markets = append(res.Markets, LeadLagMarket{
			Canonical: r.canonical, PeakLagS: int64(r.peakLag) * l.gridSec,
			PeakCorr: r.peakCorr, ZeroCorr: r.zeroCorr, N: r.n, Signal: signal,
		})
		if !signal {
			res.NoSignal++
			continue
		}
		peakCorrs = append(peakCorrs, math.Abs(r.peakCorr))
		switch {
		case r.peakLag > 0:
			res.KalshiLeads++
			leadLags = append(leadLags, float64(r.peakLag)*float64(l.gridSec))
		case r.peakLag < 0:
			res.PolyLeads++
			leadLags = append(leadLags, float64(r.peakLag)*float64(l.gridSec))
		default:
			res.Contemporaneous++
		}
	}
	if len(leadLags) > 0 {
		res.MedianLeadS, res.MedianPeakCorr = median(leadLags), median(peakCorrs)
	}
	return res
}

func (l *LeadLag) Report() string {
	res := l.Result()
	var b strings.Builder
	fmt.Fprintf(&b, "LEAD-LAG  (Kalshi vs Polymarket US, %ds grid, ±%ds scan)\n",
		l.gridSec, l.maxLag)
	fmt.Fprintf(&b, "  + lag = Kalshi leads Poly ;  - lag = Poly leads Kalshi ;  0 = move together\n\n")
	if res.Analyzed == 0 {
		b.WriteString("  no markets had enough two-sided, two-venue overlap to analyze.\n")
		return b.String()
	}
	kLead, pLead, together := res.KalshiLeads, res.PolyLeads, res.Contemporaneous
	fmt.Fprintf(&b, "  markets analyzed: %d   (with signal |corr|≥%.2f: %d, contemporaneous: %d, no signal: %d)\n",
		res.Analyzed, l.corrFloor, kLead+pLead, together, res.NoSignal)
	fmt.Fprintf(&b, "  direction (signal + lagged): Kalshi leads %d | Poly leads %d\n", kLead, pLead)
	if kLead+pLead > 0 {
		fmt.Fprintf(&b, "  median lead lag: %+.0fs   median peak |corr|: %.2f\n",
			res.MedianLeadS, res.MedianPeakCorr)
	}
	b.WriteString("\n")

	// Per-market detail, strongest signal first.
	fmt.Fprintf(&b, "  %-34s %8s %10s %10s %9s\n", "market", "peak_lag", "peak_corr", "lag0_corr", "n")
	shown := 0
	for _, r := range res.Markets {
		if !r.Signal {
			continue
		}
		fmt.Fprintf(&b, "  %-34s %+7ds %10.3f %10.3f %9d\n",
			trim(r.Canonical, 34), r.PeakLagS, r.PeakCorr, r.ZeroCorr, r.N)
		shown++
		if shown >= 40 {
			fmt.Fprintf(&b, "  … (%d more with signal not shown)\n", kLead+pLead+together-shown)
			break
		}
	}
	if shown == 0 {
		b.WriteString("  (no market cleared the correlation floor — no exploitable lead detected)\n")
	}
	return b.String()
}

func median(x []float64) float64 {
	if len(x) == 0 {
		return 0
	}
	c := append([]float64(nil), x...)
	sort.Float64s(c)
	n := len(c)
	if n%2 == 1 {
		return c[n/2]
	}
	return (c[n/2-1] + c[n/2]) / 2
}

func trim(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
