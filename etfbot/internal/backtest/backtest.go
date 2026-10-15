// Package backtest replays a Strategy over daily history. It rebalances on the
// last trading day of each month at that day's close, through the same
// rebalance.Plan the live bot uses, paying slippage on every fill.
//
// Trading at the close the signal was computed from is slightly optimistic;
// the live bot approximates it by running at 15:45 New York time.
package backtest

import (
	"fmt"
	"time"

	"etfbot/internal/market"
	"etfbot/internal/rebalance"
	"etfbot/internal/strategy"
)

// Config sets up one run.
type Config struct {
	Start, End  time.Time
	Cash        float64 // starting dollars
	SlippageBps float64 // cost per fill, in basis points of price
	Rebalance   rebalance.Params
}

// Point is the portfolio's value at one day's close.
type Point struct {
	Date   time.Time
	Equity float64
}

// Result is one run's equity curve and trading activity.
type Result struct {
	Name     string
	Curve    []Point
	Fills    int
	Traded   float64 // dollars bought plus sold
	Holdings []map[string]float64
}

// Run replays s over calendar (the trading days, oldest first) using hist.
func Run(s strategy.Strategy, hist map[string][]market.Bar, calendar []time.Time, cfg Config) (Result, error) {
	res := Result{Name: s.Name()}
	cash := cfg.Cash
	held := map[string]float64{}
	last := map[string]float64{} // latest close seen per symbol
	next := map[string]int{}     // per-symbol index of the next unseen bar
	slip := cfg.SlippageBps / 1e4

	for i, day := range calendar {
		if day.Before(cfg.Start) || day.After(cfg.End) {
			continue
		}
		for sym, bars := range hist {
			j := next[sym]
			for j < len(bars) && !bars[j].Date.After(day) {
				last[sym] = bars[j].Close
				j++
			}
			next[sym] = j
		}
		monthEnd := i+1 < len(calendar) && calendar[i+1].Month() != day.Month() && !calendar[i+1].After(cfg.End)
		if monthEnd {
			targets, err := s.Targets(day, hist)
			if err != nil {
				return res, fmt.Errorf("%s on %s: %w", s.Name(), day.Format(time.DateOnly), err)
			}
			for sym := range targets {
				if _, ok := last[sym]; !ok {
					delete(targets, sym) // no price yet: the fund hasn't launched
				}
			}
			orders := rebalance.Plan(held, cash, last, targets, cfg.Rebalance)
			var buys []market.Order
			for _, o := range orders {
				if o.Side == market.Buy {
					buys = append(buys, o)
					continue
				}
				cash += o.Qty * last[o.Symbol] * (1 - slip)
				res.Traded += o.Qty * last[o.Symbol]
				held[o.Symbol] -= o.Qty
				if held[o.Symbol] < 1e-9 {
					delete(held, o.Symbol)
				}
				res.Fills++
			}
			for _, o := range rebalance.FitBuys(buys, cash) {
				held[o.Symbol] += o.Notional / (last[o.Symbol] * (1 + slip))
				cash -= o.Notional
				res.Traded += o.Notional
				res.Fills++
			}
		}
		eq := cash
		for sym, q := range held {
			eq += q * last[sym]
		}
		res.Curve = append(res.Curve, Point{day, eq})
		if monthEnd {
			snap := map[string]float64{}
			for sym, q := range held {
				snap[sym] = q * last[sym] / eq
			}
			res.Holdings = append(res.Holdings, snap)
		}
	}
	if len(res.Curve) == 0 {
		return res, fmt.Errorf("no trading days between %s and %s", cfg.Start.Format(time.DateOnly), cfg.End.Format(time.DateOnly))
	}
	return res, nil
}
