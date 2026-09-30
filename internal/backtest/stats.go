package backtest

import (
	"math"
	"time"
)

// Stats summarizes an equity curve.
type Stats struct {
	CAGR        float64 // compound annual growth
	Vol         float64 // annualized volatility of daily returns
	Sharpe      float64 // mean daily return / its stdev, annualized, no risk-free rate
	MaxDrawdown float64 // worst peak-to-trough fall, as a positive fraction
	WorstYear   float64
	Years       map[int]float64 // calendar-year returns
}

// Summarize computes Stats for r.
func Summarize(r Result) Stats {
	c := r.Curve
	st := Stats{Years: map[int]float64{}}
	if len(c) < 2 {
		return st
	}
	yrs := c[len(c)-1].Date.Sub(c[0].Date).Hours() / 24 / 365.25
	st.CAGR = math.Pow(c[len(c)-1].Equity/c[0].Equity, 1/yrs) - 1

	var sum, sq float64
	peak := c[0].Equity
	yearStart := c[0].Equity
	for i := 1; i < len(c); i++ {
		ret := c[i].Equity/c[i-1].Equity - 1
		sum += ret
		sq += ret * ret
		peak = math.Max(peak, c[i].Equity)
		st.MaxDrawdown = math.Max(st.MaxDrawdown, 1-c[i].Equity/peak)
		if i == len(c)-1 || c[i+1].Date.Year() != c[i].Date.Year() {
			st.Years[c[i].Date.Year()] = c[i].Equity/yearStart - 1
			yearStart = c[i].Equity
		}
	}
	n := float64(len(c) - 1)
	mean := sum / n
	sd := math.Sqrt(sq/n - mean*mean)
	st.Vol = sd * math.Sqrt(252)
	if sd > 0 {
		st.Sharpe = mean / sd * math.Sqrt(252)
	}
	st.WorstYear = math.Inf(1)
	for _, v := range st.Years {
		st.WorstYear = math.Min(st.WorstYear, v)
	}
	return st
}

// TurnoverPerYear is dollars traded per year as a multiple of average equity.
func TurnoverPerYear(r Result) float64 {
	c := r.Curve
	if len(c) < 2 {
		return 0
	}
	var avg float64
	for _, p := range c {
		avg += p.Equity
	}
	avg /= float64(len(c))
	yrs := c[len(c)-1].Date.Sub(c[0].Date) / (time.Hour * 24)
	return r.Traded / avg / (float64(yrs) / 365.25)
}
