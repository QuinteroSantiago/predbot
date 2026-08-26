package backtest

import (
	"fmt"
	"strings"
)

// Longshot measures calibration and the favorite–longshot bias: for every
// settled market we have price history for, it takes the market's OPENING implied
// probability (the first two-sided mid we recorded, a pre-game forecast) and
// compares it to whether the market actually resolved YES.
//
// If the market is well calibrated, contracts priced at p resolve YES p of the
// time. The classic favorite–longshot bias is the systematic deviation:
// longshots (low p) resolve YES LESS often than priced (overbet), favorites
// (high p) MORE often (underbet) — a fitted line of realized-on-implied with
// slope > 1 / intercept < 0. Read-only: it never trades.
type Longshot struct {
	nb      int                // buckets across [0,1]
	outcome map[string]int     // "venue|venueID" -> 1 (yes) / 0 (no)
	open    map[string]float64 // "venue|venueID" -> opening implied prob
}

// NewLongshot takes the settled outcomes (key "venue|venueID" -> yes=1/no=0).
func NewLongshot(outcomes map[string]int) *Longshot {
	return &Longshot{nb: 10, outcome: outcomes, open: map[string]float64{}}
}

// ObserveMid records the FIRST implied prob seen for a market that has a known
// outcome — the opening line. prob is in [0,1]. Later observations are ignored,
// so in-game convergence never contaminates the forecast.
func (l *Longshot) ObserveMid(key string, prob float64) {
	if _, ok := l.outcome[key]; !ok {
		return // no settled outcome for this market
	}
	if _, seen := l.open[key]; seen {
		return
	}
	if prob <= 0 || prob >= 1 {
		return // a market already at 0/1 carries no forecast
	}
	l.open[key] = prob
}

type llBucket struct {
	lo, hi       float64
	n            int
	sumImplied   float64
	sumOutcome   float64
}

// Report renders the calibration table and the fitted favorite–longshot line.
func (l *Longshot) Report() string {
	// Collect (implied, outcome) pairs for markets we have both for.
	type pt struct{ p, y float64 }
	var pts []pt
	for key, p := range l.open {
		pts = append(pts, pt{p, float64(l.outcome[key])})
	}
	var b strings.Builder
	fmt.Fprintf(&b, "FAVORITE–LONGSHOT / CALIBRATION  (opening line vs realized)\n")
	fmt.Fprintf(&b, "  a market priced at implied p should resolve YES p of the time.\n\n")
	if len(pts) < 2 {
		fmt.Fprintf(&b, "  only %d settled markets with price history — not enough to analyze.\n", len(pts))
		return b.String()
	}

	// Bucketed calibration table.
	bk := make([]llBucket, l.nb)
	for i := range bk {
		bk[i].lo = float64(i) / float64(l.nb)
		bk[i].hi = float64(i+1) / float64(l.nb)
	}
	var sumP, sumY float64
	for _, q := range pts {
		i := int(q.p * float64(l.nb))
		if i >= l.nb {
			i = l.nb - 1
		}
		bk[i].n++
		bk[i].sumImplied += q.p
		bk[i].sumOutcome += q.y
		sumP += q.p
		sumY += q.y
	}
	n := float64(len(pts))
	fmt.Fprintf(&b, "  settled markets analyzed: %d   mean implied: %.3f   realized YES rate: %.3f\n\n",
		len(pts), sumP/n, sumY/n)
	fmt.Fprintf(&b, "  %-13s %6s %11s %11s %10s\n", "implied", "n", "mean_impl", "realized", "edge")
	for _, c := range bk {
		if c.n == 0 {
			continue
		}
		mi := c.sumImplied / float64(c.n)
		rz := c.sumOutcome / float64(c.n)
		fmt.Fprintf(&b, "  %4.2f–%-4.2f    %6d %11.3f %11.3f %+10.3f\n",
			c.lo, c.hi, c.n, mi, rz, rz-mi)
	}
	fmt.Fprintf(&b, "  (edge = realized − implied; + = the market UNDERpriced this bucket's YES)\n\n")

	// Least-squares fit realized ~ a + b·implied across markets. b>1 & a<0 is the
	// classic favorite–longshot signature; b≈1 & a≈0 is well-calibrated.
	var sx, sy, sxx, sxy float64
	for _, q := range pts {
		sx += q.p
		sy += q.y
		sxx += q.p * q.p
		sxy += q.p * q.y
	}
	den := n*sxx - sx*sx
	fit := ""
	if den != 0 {
		slope := (n*sxy - sx*sy) / den
		intercept := (sy - slope*sx) / n
		fit = fmt.Sprintf("  fitted realized = %.3f + %.3f·implied", intercept, slope)
		switch {
		case slope > 1.1 && intercept < -0.02:
			fit += "   → favorite–longshot bias (longshots overpriced, favorites underpriced)"
		case slope < 0.9:
			fit += "   → reverse bias (favorites overpriced)"
		default:
			fit += "   → roughly calibrated (within noise)"
		}
	}
	fmt.Fprintln(&b, fit)

	// Brier score vs the trivial always-base-rate baseline, as a sanity anchor.
	var brier float64
	for _, q := range pts {
		brier += (q.p - q.y) * (q.p - q.y)
	}
	brier /= n
	base := (sumY / n) * (1 - sumY/n)
	fmt.Fprintf(&b, "  Brier score: %.3f   (base-rate baseline %.3f; lower is a sharper forecast)\n", brier, base)

	fmt.Fprintf(&b, "\n  NOTE: n is small (a few days of recorded games). Read the SIGN, not the\n"+
		"  magnitude — favorite–longshot bias needs hundreds of settled markets to pin down.\n")
	return b.String()
}
