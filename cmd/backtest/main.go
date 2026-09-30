// backtest replays the configured rotation strategy, and two benchmarks, over
// daily history from Yahoo Finance.
//
//	go run ./cmd/backtest -from 2007-01-01
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"etfbot/internal/backtest"
	"etfbot/internal/config"
	"etfbot/internal/strategy"
	"etfbot/internal/yahoo"
)

func main() {
	cfgPath := flag.String("config", "config/config.yaml", "configuration file")
	from := flag.String("from", "2007-01-01", "first day (YYYY-MM-DD); the strategy needs ~1 year of history before it")
	to := flag.String("to", time.Now().Format(time.DateOnly), "last day (YYYY-MM-DD)")
	slip := flag.Float64("slippage-bps", 5, "cost per fill in basis points")
	cache := flag.String("cache", "data/yahoo", "history cache directory")
	years := flag.Bool("years", false, "print calendar-year returns")
	flag.Parse()

	if err := run(*cfgPath, *from, *to, *slip, *cache, *years); err != nil {
		fmt.Fprintln(os.Stderr, "backtest:", err)
		os.Exit(1)
	}
}

func run(cfgPath, from, to string, slip float64, cache string, years bool) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	start, err := time.Parse(time.DateOnly, from)
	if err != nil {
		return err
	}
	end, err := time.Parse(time.DateOnly, to)
	if err != nil {
		return err
	}
	strats := []strategy.Strategy{
		cfg.Rotation(),
		strategy.Fixed{Label: "SPY buy & hold", Weights: map[string]float64{"SPY": 1}},
		strategy.Fixed{Label: "60/40 SPY/IEF", Weights: map[string]float64{"SPY": 0.6, "IEF": 0.4}},
	}
	need := map[string]bool{}
	for _, s := range strats {
		for _, sym := range s.Symbols() {
			need[sym] = true
		}
	}
	var syms []string
	for s := range need {
		syms = append(syms, s)
	}
	sort.Strings(syms)

	src := &yahoo.Client{CacheDir: cache, MaxAge: 20 * time.Hour}
	hist, err := src.Daily(context.Background(), syms, time.Time{}, end)
	if err != nil {
		return err
	}
	var calendar []time.Time // SPY's trading days
	for _, b := range hist["SPY"] {
		calendar = append(calendar, b.Date)
	}
	for _, sym := range syms {
		if b := hist[sym]; len(b) > 0 && b[0].Date.After(start.AddDate(-1, 0, 0)) {
			fmt.Printf("note: %s history starts %s; it joins once it has enough\n", sym, b[0].Date.Format(time.DateOnly))
		}
	}

	bt := backtest.Config{Start: start, End: end, Cash: 10000, SlippageBps: slip, Rebalance: cfg.Params()}
	fmt.Printf("\n%s → %s, $10,000 start, %.0fbp per fill, monthly rebalance\n\n", from, to, slip)
	fmt.Printf("%-18s %8s %8s %7s %8s %9s %9s %10s\n", "", "CAGR", "vol", "Sharpe", "max DD", "worst yr", "turnover", "end value")
	var results []backtest.Result
	for _, s := range strats {
		r, err := backtest.Run(s, hist, calendar, bt)
		if err != nil {
			return err
		}
		results = append(results, r)
		st := backtest.Summarize(r)
		fmt.Printf("%-18s %7.1f%% %7.1f%% %7.2f %7.1f%% %8.1f%% %8.1fx %10s\n", r.Name,
			st.CAGR*100, st.Vol*100, st.Sharpe, -st.MaxDrawdown*100, st.WorstYear*100,
			backtest.TurnoverPerYear(r), dollars(r.Curve[len(r.Curve)-1].Equity))
	}
	if years {
		printYears(results)
	}
	fmt.Printf("\ncurrent holdings (%s): %s\n", results[0].Curve[len(results[0].Curve)-1].Date.Format(time.DateOnly), holdings(results[0]))
	return nil
}

func printYears(results []backtest.Result) {
	fmt.Printf("\n%-6s", "year")
	for _, r := range results {
		fmt.Printf(" %18s", r.Name)
	}
	fmt.Println()
	stats := make([]backtest.Stats, len(results))
	for i, r := range results {
		stats[i] = backtest.Summarize(r)
	}
	var ys []int
	for y := range stats[0].Years {
		ys = append(ys, y)
	}
	sort.Ints(ys)
	for _, y := range ys {
		fmt.Printf("%-6d", y)
		for _, st := range stats {
			fmt.Printf(" %17.1f%%", st.Years[y]*100)
		}
		fmt.Println()
	}
}

func holdings(r backtest.Result) string {
	if len(r.Holdings) == 0 {
		return "none"
	}
	h := r.Holdings[len(r.Holdings)-1]
	var parts []string
	for sym, w := range h {
		parts = append(parts, fmt.Sprintf("%s %.0f%%", sym, w*100))
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

func dollars(x float64) string { return fmt.Sprintf("$%s", commas(int64(x+0.5))) }

func commas(n int64) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
