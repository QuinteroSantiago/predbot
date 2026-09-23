package backtest

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"predbot/internal/core"
)

// OutcomeReg is the outcome regression: a logistic regression of whether a
// market resolved YES on its opening price (as log-odds) and other features,
// with standard errors clustered by league and date — mirror markets of one
// game, and all games on a day, share news, so that's the honest unit.
//
//   - Calibration, per venue: P(yes) = σ(a + b·logit(p)). b = 1 and a = 0 is a
//     perfectly calibrated price; b > 1 means favorites win more than priced
//     (the favorite–longshot bias), b < 1 the reverse.
//   - Full model: adds venue, venue × price, league and the opening bid/ask
//     spread, to see whether anything shifts the odds beyond the price.
//   - Out of sample: 5-fold cross-validation grouped by day; the model's
//     log-loss and Brier score on held-out days against the market price's
//     own. A model that can't beat the price on unseen days has found nothing.
type OutcomeReg struct {
	outcomes map[string]int
	obs      map[string]*orObs // "venue|venueID" -> opening quote
}

type orObs struct {
	venue, cluster string
	p, spread      float64 // implied prob; bid/ask width in cents
	y              float64
	league         string
}

func NewOutcomeReg(outcomes map[string]int) *OutcomeReg {
	return &OutcomeReg{outcomes: outcomes, obs: map[string]*orObs{}}
}

// Observe records each settled market's first two-sided quote.
func (o *OutcomeReg) Observe(canonical string, s core.BookSnapshot) {
	key := s.Venue + "|" + s.VenueID
	y, ok := o.outcomes[key]
	if !ok || o.obs[key] != nil {
		return
	}
	bid, okB := s.BestYesBid()
	ask, okA := s.BestYesAsk()
	if !okB || !okA || ask.Px <= bid.Px {
		return
	}
	p := (float64(bid.Px) + float64(ask.Px)) / 2 / float64(core.OneDollar)
	if p <= 0.02 || p >= 0.98 {
		return // a market already decided carries no forecast
	}
	league, cluster := "?", canonical
	if parts := strings.SplitN(canonical, "-", 3); len(parts) == 3 {
		league, cluster = parts[0], parts[0]+"-"+parts[1]
	}
	o.obs[key] = &orObs{venue: s.Venue, cluster: cluster, league: league, p: p,
		spread: cents(ask.Px) - cents(bid.Px), y: float64(y)}
}

// OutcomeRegResult is the outcome regression, for the text report and the
// backtest runs page.
type OutcomeRegResult struct {
	Obs      int       `json:"obs"`
	Clusters int       `json:"clusters"` // league-days
	YesRate  float64   `json:"yes_rate"`
	Calib    []ORCalib `json:"calibration"`
	Coefs    []ORCoef  `json:"coefficients"` // the full model
	CV       ORCV      `json:"cv"`
	Points   []ORPoint `json:"points"` // binned data, for plotting against the fits
	Note     string    `json:"note,omitempty"`
}

// ORCalib is σ(a + b·logit(p)) for one venue.
type ORCalib struct {
	Venue     string  `json:"venue"`
	N         int     `json:"n"`
	Intercept float64 `json:"intercept"`
	InterSE   float64 `json:"intercept_se"`
	Slope     float64 `json:"slope"`
	SlopeSE   float64 `json:"slope_se"`
}

// ORCoef is one full-model coefficient, in log-odds.
type ORCoef struct {
	Name string  `json:"name"`
	Beta float64 `json:"beta"`
	SE   float64 `json:"se"`
	What string  `json:"what"` // plain-language reading
}

// ORCV is the grouped cross-validation. Diff = market − model: > 0 means the
// model predicted held-out days better than the price did.
type ORCV struct {
	Folds       int     `json:"folds"`
	N           int     `json:"n"`
	MarketLL    float64 `json:"market_logloss"`
	ModelLL     float64 `json:"model_logloss"`
	LLDiff      float64 `json:"logloss_diff"`
	LLDiffSE    float64 `json:"logloss_diff_se"`
	MarketBrier float64 `json:"market_brier"`
	ModelBrier  float64 `json:"model_brier"`
	BrierDiff   float64 `json:"brier_diff"`
	BrierDiffSE float64 `json:"brier_diff_se"`
}

// ORPoint is one price bin: markets priced in [Lo, Hi) and how often YES won.
type ORPoint struct {
	Venue    string  `json:"venue"`
	Lo       float64 `json:"lo"`
	Hi       float64 `json:"hi"`
	N        int     `json:"n"`
	MeanP    float64 `json:"mean_p"`
	Realized float64 `json:"realized"`
}

func logOdds(p float64) float64 { return math.Log(p / (1 - p)) }

func clampP(p float64) float64 { return math.Min(math.Max(p, 0.005), 0.995) }

// Result fits everything. Markets are sorted first so the fit is deterministic.
func (o *OutcomeReg) Result() OutcomeRegResult {
	keys := make([]string, 0, len(o.obs))
	for k := range o.obs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	rows := make([]*orObs, len(keys))
	for i, k := range keys {
		rows[i] = o.obs[k]
	}
	res := OutcomeRegResult{Obs: len(rows)}
	clusters := map[string]bool{}
	var yes float64
	for _, r := range rows {
		clusters[r.cluster] = true
		yes += r.y
	}
	res.Clusters = len(clusters)
	if len(rows) < 30 || len(clusters) < 5 {
		res.Note = fmt.Sprintf("too little data: %d settled markets over %d league-days (need 30+ over 5+)", len(rows), len(clusters))
		return res
	}
	res.YesRate = yes / float64(len(rows))

	// Calibration per venue.
	for _, v := range []string{"kalshi", "polymarketus"} {
		var X [][]float64
		var y []float64
		var cl []string
		for _, r := range rows {
			if r.venue == v {
				X, y, cl = append(X, []float64{1, logOdds(r.p)}), append(y, r.y), append(cl, r.cluster)
			}
		}
		if len(X) < 30 {
			continue
		}
		if f, err := fitLogit(X, y, cl); err == nil {
			res.Calib = append(res.Calib, ORCalib{Venue: v, N: len(X), Intercept: f.Beta[0], InterSE: f.SE[0], Slope: f.Beta[1], SlopeSE: f.SE[1]})
		}
	}

	// Full model; columns that don't vary in this sample are left out.
	cols := o.columns(rows)
	X, y, cl := o.design(rows, cols)
	if f, err := fitLogit(X, y, cl); err == nil {
		for i, c := range cols {
			res.Coefs = append(res.Coefs, ORCoef{Name: c.name, Beta: f.Beta[i], SE: f.SE[i], What: c.what})
		}
	} else {
		res.Note = "full model: " + err.Error()
	}

	res.CV = o.crossValidate(rows, cols, 5)
	res.Points = o.points(rows)
	return res
}

type orCol struct {
	name, what string
	f          func(*orObs) float64
}

func (o *OutcomeReg) columns(rows []*orObs) []orCol {
	cols := []orCol{
		{"intercept", "baseline log-odds at a 50/50 price", func(*orObs) float64 { return 1 }},
		{"log-odds of price", "1 = the price is right; > 1 favorites underpriced", func(r *orObs) float64 { return logOdds(r.p) }},
	}
	venues, leagues := map[string]bool{}, map[string]bool{}
	for _, r := range rows {
		venues[r.venue], leagues[r.league] = true, true
	}
	if venues["kalshi"] && venues["polymarketus"] {
		poly := func(r *orObs) float64 {
			if r.venue == "polymarketus" {
				return 1
			}
			return 0
		}
		cols = append(cols,
			orCol{"Polymarket", "shift in YES odds on Polymarket vs Kalshi at the same price", poly},
			orCol{"Polymarket × price", "how much Polymarket's calibration slope differs", func(r *orObs) float64 { return poly(r) * logOdds(r.p) }})
	}
	if leagues["NFL"] && len(leagues) > 1 {
		cols = append(cols, orCol{"NFL", "shift in YES odds for NFL vs other leagues", func(r *orObs) float64 {
			if r.league == "NFL" {
				return 1
			}
			return 0
		}})
	}
	cols = append(cols, orCol{"opening spread (¢)", "shift per cent of bid/ask width at open", func(r *orObs) float64 { return r.spread }})
	// A feature that doesn't vary here duplicates the intercept and makes the
	// fit singular: leave it out.
	kept := []orCol{cols[0]}
	for _, c := range cols[1:] {
		lo, hi := math.Inf(1), math.Inf(-1)
		for _, r := range rows {
			v := c.f(r)
			lo, hi = math.Min(lo, v), math.Max(hi, v)
		}
		if hi-lo > 1e-6 { // float dust (e.g. a 1¢ spread computed two ways) isn't variation
			kept = append(kept, c)
		}
	}
	return kept
}

func (o *OutcomeReg) design(rows []*orObs, cols []orCol) ([][]float64, []float64, []string) {
	X := make([][]float64, len(rows))
	y := make([]float64, len(rows))
	cl := make([]string, len(rows))
	for i, r := range rows {
		X[i] = make([]float64, len(cols))
		for j, c := range cols {
			X[i][j] = c.f(r)
		}
		y[i], cl[i] = r.y, r.cluster
	}
	return X, y, cl
}

// crossValidate holds out whole league-days: fit on the rest, score the
// held-out markets with the model and with the market's own price.
func (o *OutcomeReg) crossValidate(rows []*orObs, cols []orCol, folds int) ORCV {
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
		fold[d] = i % folds
	}
	cv := ORCV{Folds: folds}
	var llDiff, brDiff []float64
	var groups []string
	for f := 0; f < folds; f++ {
		var train, test []*orObs
		for _, r := range rows {
			if fold[r.cluster] == f {
				test = append(test, r)
			} else {
				train = append(train, r)
			}
		}
		if len(test) == 0 || len(train) < 30 {
			continue
		}
		X, y, cl := o.design(train, cols)
		fit, err := fitLogit(X, y, cl)
		if err != nil {
			continue
		}
		Xt, _, _ := o.design(test, cols)
		for i, r := range test {
			pm, pf := clampP(r.p), clampP(fit.predict(Xt[i]))
			llm := -(r.y*math.Log(pm) + (1-r.y)*math.Log(1-pm))
			llf := -(r.y*math.Log(pf) + (1-r.y)*math.Log(1-pf))
			bm, bf := (pm-r.y)*(pm-r.y), (pf-r.y)*(pf-r.y)
			cv.MarketLL += llm
			cv.ModelLL += llf
			cv.MarketBrier += bm
			cv.ModelBrier += bf
			cv.N++
			llDiff, brDiff, groups = append(llDiff, llm-llf), append(brDiff, bm-bf), append(groups, r.cluster)
		}
	}
	if cv.N > 0 {
		n := float64(cv.N)
		cv.MarketLL, cv.ModelLL, cv.MarketBrier, cv.ModelBrier = cv.MarketLL/n, cv.ModelLL/n, cv.MarketBrier/n, cv.ModelBrier/n
		cv.LLDiff, cv.LLDiffSE = clusteredMean(llDiff, groups)
		cv.BrierDiff, cv.BrierDiffSE = clusteredMean(brDiff, groups)
	}
	return cv
}

func (o *OutcomeReg) points(rows []*orObs) []ORPoint {
	type acc struct{ n, sp, sy float64 }
	bins := map[string][]acc{}
	for _, r := range rows {
		if bins[r.venue] == nil {
			bins[r.venue] = make([]acc, 10)
		}
		i := int(r.p * 10)
		if i > 9 {
			i = 9
		}
		b := &bins[r.venue][i]
		b.n++
		b.sp += r.p
		b.sy += r.y
	}
	var out []ORPoint
	for _, v := range []string{"kalshi", "polymarketus"} {
		for i, b := range bins[v] {
			if b.n > 0 {
				out = append(out, ORPoint{Venue: v, Lo: float64(i) / 10, Hi: float64(i+1) / 10, N: int(b.n), MeanP: b.sp / b.n, Realized: b.sy / b.n})
			}
		}
	}
	return out
}

// CalibVerdict reads a calibration slope against the ideal of 1.
func CalibVerdict(c ORCalib) string {
	switch {
	case c.Slope-1.96*c.SlopeSE > 1:
		return "favorites underpriced, longshots overpriced"
	case c.Slope+1.96*c.SlopeSE < 1:
		return "favorites overpriced, longshots underpriced"
	}
	return "calibrated within noise"
}

// Report renders the regression as text.
func (o *OutcomeReg) Report() string {
	r := o.Result()
	var b strings.Builder
	fmt.Fprintf(&b, "OUTCOME REGRESSION  (logistic: did YES win, on opening price and features; SE clustered by league-day)\n")
	if r.Note != "" && len(r.Coefs) == 0 {
		fmt.Fprintf(&b, "  %s\n", r.Note)
		return b.String()
	}
	fmt.Fprintf(&b, "  %d settled markets over %d league-days, YES won %.1f%%\n\n", r.Obs, r.Clusters, 100*r.YesRate)
	fmt.Fprintf(&b, "  calibration  P(yes) = σ(a + b·logit(price));  b = 1, a = 0 is a correct price\n")
	for _, c := range r.Calib {
		fmt.Fprintf(&b, "    %-13s n=%-5d a = %+.3f ± %.3f   b = %.3f ± %.3f   -> %s\n", c.Venue, c.N,
			c.Intercept, 1.96*c.InterSE, c.Slope, 1.96*c.SlopeSE, CalibVerdict(c))
	}
	fmt.Fprintf(&b, "\n  full model (log-odds):\n")
	for _, c := range r.Coefs {
		fmt.Fprintf(&b, "    %-22s %+8.3f ± %-7.3f  %s\n", c.Name, c.Beta, 1.96*c.SE, c.What)
	}
	cv := r.CV
	fmt.Fprintf(&b, "\n  out of sample (%d folds by day, %d markets): log-loss market %.4f vs model %.4f; Brier %.4f vs %.4f\n",
		cv.Folds, cv.N, cv.MarketLL, cv.ModelLL, cv.MarketBrier, cv.ModelBrier)
	verdict := "the model does NOT beat the market price on unseen days — no exploitable structure"
	if cv.LLDiff-1.96*cv.LLDiffSE > 0 {
		verdict = "the model beats the market price on unseen days — there's structure the price misses"
	} else if cv.LLDiff+1.96*cv.LLDiffSE < 0 {
		verdict = "the model does worse than the market price on unseen days — the price already knows more"
	}
	fmt.Fprintf(&b, "  market − model log-loss: %+.4f ± %.4f  -> %s\n", cv.LLDiff, 1.96*cv.LLDiffSE, verdict)
	return b.String()
}
