// Command outcomes records how markets RESOLVED — the data every directional
// (non-arb) strategy needs to compute P&L. Run it on a timer; it is read-only
// against the venues and append-only on disk.
//
// It checks every market in markets.yaml plus any sports game it can discover,
// and records the ones that have settled.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"predbot/internal/config"
	"predbot/internal/outcome"
	"predbot/internal/sports"
)

func main() {
	mktPath := flag.String("markets", "config/markets.yaml", "path to markets.yaml")
	out := flag.String("out", "/var/lib/predbot/data/outcomes.jsonl", "append-only outcomes file")
	withSports := flag.String("sports", "", "also check finished games for these leagues, e.g. mlb")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	_ = os.MkdirAll(filepath.Dir(*out), 0o750)
	store, err := outcome.NewStore(*out)
	if err != nil {
		fmt.Println("store:", err)
		os.Exit(1)
	}
	before := store.Count()

	type target struct{ canonical, kTicker, pSlug string }
	var targets []target

	markets, err := config.LoadMarkets(*mktPath)
	if err != nil {
		fmt.Println("markets:", err)
		os.Exit(1)
	}
	for _, m := range markets {
		targets = append(targets, target{m.Canonical, m.Venues.Kalshi.Ticker, m.Venues.PolymarketUS.Slug})
	}
	// Sports games settle within hours, so they are the fastest source of
	// resolved outcomes — exactly what a directional backtest needs volume of.
	if *withSports != "" {
		c := sports.New()
		for _, lg := range splitCSV(*withSports) {
			k, e1 := c.DiscoverKalshi(ctx, lg)
			p, e2 := c.DiscoverPoly(ctx, lg)
			if e1 != nil || e2 != nil {
				fmt.Println("sports discovery:", e1, e2)
				continue
			}
			for _, m := range sports.MatchGames(k, p) {
				targets = append(targets, target{m.Canonical, m.KalshiTicker, m.PolySlug})
			}
		}
	}

	const kBase = "https://api.elections.kalshi.com/trade-api/v2"
	const pBase = "https://gateway.polymarket.us"
	settled := 0
	for _, t := range targets {
		if t.kTicker != "" && !store.Has("kalshi", t.kTicker) {
			if r, ok, err := outcome.FetchKalshi(ctx, kBase, t.kTicker); err == nil && ok {
				r.Canonical = t.canonical
				if store.Put(r) == nil {
					settled++
					fmt.Printf("SETTLED kalshi %-34s -> %s\n", r.VenueID, r.Outcome)
				}
			}
		}
		if t.pSlug != "" && !store.Has("polymarketus", t.pSlug) {
			if r, ok, err := outcome.FetchPolyUS(ctx, pBase, t.pSlug); err == nil && ok {
				r.Canonical = t.canonical
				if store.Put(r) == nil {
					settled++
					fmt.Printf("SETTLED poly   %-34s -> %s\n", r.VenueID, r.Outcome)
				}
			}
		}
	}
	fmt.Printf("checked %d markets; newly settled %d; total recorded %d (was %d)\n",
		len(targets), settled, store.Count(), before)
	_ = time.Now()
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range filepath.SplitList(s) {
		_ = p
	}
	cur := ""
	for _, r := range s {
		if r == ',' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
