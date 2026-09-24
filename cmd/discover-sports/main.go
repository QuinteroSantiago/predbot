// Command discover-sports runs the cross-venue sports discovery live and prints
// the matched games. It's the inspection tool for the dynamic-subscription
// engine: run it at gametime (when Poly US opens daily moneylines) to see which
// games are cross-listed and ready to record.
package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	"predbot/internal/sports"
)

func main() {
	league := flag.String("league", "mlb", "league: mlb|nfl|nba|wnba|cfb")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c := sports.New()

	kalshi, err := c.DiscoverKalshi(ctx, *league)
	if err != nil {
		fmt.Println("kalshi discovery error:", err)
	}
	poly, err := c.DiscoverPoly(ctx, *league)
	if err != nil {
		fmt.Println("poly discovery error:", err)
	}
	fmt.Printf("%s: Kalshi games=%d, Poly moneyline games=%d\n", *league, len(kalshi), len(poly))

	matches := sports.MatchGames(kalshi, poly)
	fmt.Printf("cross-listed outcomes: %d\n", len(matches))
	for _, m := range matches {
		fmt.Printf("  %-20s %s  ⇄  %s\n", m.Canonical, m.KalshiTicker, m.PolySlug)
	}
	if len(poly) == 0 {
		fmt.Println("\n(0 Poly moneylines — they open near gametime; re-run this afternoon/evening ET)")
	}
}
