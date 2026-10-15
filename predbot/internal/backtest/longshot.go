package backtest

import (
	"fmt"
	"math"
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

// LongshotResult is the calibration analysis, for the text report and the
// backtest runs page. Probabilities are fractions.
type LongshotResult struct {
	Outcomes     int              `json:"outcomes"` // settled outcomes loaded (set by the caller)
	Markets      int              `json:"markets"`  // settled markets with an opening price
	MeanImplied  float64          `json:"mean_implied"`
	RealizedRate float64          `json:"realized_rate"`
	Buckets      []LongshotBucket `json:"buckets"` // non-empty buckets only, low to high
	// Least-squares fit realized ~ Intercept + Slope·implied across markets.
	// Slope>1 with Intercept<0 is the classic favorite–longshot signature.
	FitOK     bool    `json:"fit_ok"`
	Intercept float64 `json:"intercept"`
	Slope     float64 `json:"slope"`
	Verdict   string  `json:"verdict"` // "favorite-longshot", "reverse", "calibrated", or ""
	Brier     float64 `json:"brier"`
	BrierBase float64 `json:"brier_base"` // always-predict-the-base-rate baseline
}

// LongshotBucket is one implied-probability band.
type LongshotBucket struct {
	Lo          float64 `json:"lo"`
	Hi          float64 `json:"hi"`
	N           int     `json:"n"`
	MeanImplied float64 `json:"mean_implied"`
	Realized    float64 `json:"realized"`
	Edge        float64 `json:"edge"` // realized − implied; + = the market underpriced YES
	// StdErr is the binomial standard error of Realized. ±1.96·StdErr is a rough
	// 95% range: an Edge inside it is indistinguishable from noise.
	StdErr float64 `json:"stderr"`
}

// Result computes the calibration analysis.
func (l *Longshot) Result() LongshotResult {
	var res LongshotResult
	// Collect (implied, outcome) pairs for markets we have both for.
	type pt struct{ p, y float64 }
	var pts []pt
	for key, p := range l.open {
		pts = append(pts, pt{p, float64(l.outcome[key])})
	}
	res.Markets = len(pts)
	if len(pts) < 2 {
		return res
	}

	// Bucketed calibration table.
	type acc struct {
		n                      int
		sumImplied, sumOutcome float64
	}
	bk := make([]acc, l.nb)
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
	res.MeanImplied, res.RealizedRate = sumP/n, sumY/n
	for i, c := range bk {
		if c.n == 0 {
			continue
		}
		mi := c.sumImplied / float64(c.n)
		rz := c.sumOutcome / float64(c.n)
		res.Buckets = append(res.Buckets, LongshotBucket{
			Lo: float64(i) / float64(l.nb), Hi: float64(i+1) / float64(l.nb),
			N: c.n, MeanImplied: mi, Realized: rz, Edge: rz - mi,
			StdErr: math.Sqrt(rz * (1 - rz) / float64(c.n)),
		})
	}

	var sx, sy, sxx, sxy float64
	for _, q := range pts {
		sx += q.p
		sy += q.y
		sxx += q.p * q.p
		sxy += q.p * q.y
	}
	if den := n*sxx - sx*sx; den != 0 {
		res.FitOK = true
		res.Slope = (n*sxy - sx*sy) / den
		res.Intercept = (sy - res.Slope*sx) / n
		switch {
		case res.Slope > 1.1 && res.Intercept < -0.02:
			res.Verdict = "favorite-longshot"
		case res.Slope < 0.9:
			res.Verdict = "reverse"
		default:
			res.Verdict = "calibrated"
		}
	}

	// Brier score vs the trivial always-base-rate baseline, as a sanity anchor.
	for _, q := range pts {
		res.Brier += (q.p - q.y) * (q.p - q.y)
	}
	res.Brier /= n
	res.BrierBase = res.RealizedRate * (1 - res.RealizedRate)
	return res
}

// Report renders the calibration table and the fitted favorite–longshot line.
func (l *Longshot) Report() string {
	res := l.Result()
	var b strings.Builder
	fmt.Fprintf(&b, "FAVORITE–LONGSHOT / CALIBRATION  (opening line vs realized)\n")
	fmt.Fprintf(&b, "  a market priced at implied p should resolve YES p of the time.\n\n")
	if res.Markets < 2 {
		fmt.Fprintf(&b, "  only %d settled markets with price history — not enough to analyze.\n", res.Markets)
		return b.String()
	}
	fmt.Fprintf(&b, "  settled markets analyzed: %d   mean implied: %.3f   realized YES rate: %.3f\n\n",
		res.Markets, res.MeanImplied, res.RealizedRate)
	fmt.Fprintf(&b, "  %-13s %6s %11s %11s %10s\n", "implied", "n", "mean_impl", "realized", "edge")
	for _, c := range res.Buckets {
		fmt.Fprintf(&b, "  %4.2f–%-4.2f    %6d %11.3f %11.3f %+10.3f\n",
			c.Lo, c.Hi, c.N, c.MeanImplied, c.Realized, c.Edge)
	}
	fmt.Fprintf(&b, "  (edge = realized − implied; + = the market UNDERpriced this bucket's YES)\n\n")

	fit := ""
	if res.FitOK {
		fit = fmt.Sprintf("  fitted realized = %.3f + %.3f·implied", res.Intercept, res.Slope)
		switch res.Verdict {
		case "favorite-longshot":
			fit += "   → favorite–longshot bias (longshots overpriced, favorites underpriced)"
		case "reverse":
			fit += "   → reverse bias (favorites overpriced)"
		default:
			fit += "   → roughly calibrated (within noise)"
		}
	}
	fmt.Fprintln(&b, fit)
	fmt.Fprintf(&b, "  Brier score: %.3f   (base-rate baseline %.3f; lower is a sharper forecast)\n", res.Brier, res.BrierBase)

	fmt.Fprintf(&b, "\n  NOTE: n is small (a few days of recorded games). Read the SIGN, not the\n"+
		"  magnitude — favorite–longshot bias needs hundreds of settled markets to pin down.\n")
	return b.String()
}
