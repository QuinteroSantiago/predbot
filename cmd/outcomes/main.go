// Command outcomes records how markets RESOLVED — the data every directional
// (non-arb) strategy needs to compute P&L. Run it on a timer; it is read-only
// against the venues and append-only on disk.
//
// It checks every market in markets.yaml plus any sports game it can discover,
// and records the ones that have settled.
package main

import (
	"bufio"
	"compress/gzip"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"predbot/internal/core"
	"regexp"
	"strings"
	"time"

	"predbot/internal/config"
	"predbot/internal/outcome"
	"predbot/internal/sports"
)

func main() {
	mktPath := flag.String("markets", "config/markets.yaml", "path to markets.yaml")
	out := flag.String("out", "/var/lib/predbot/data/outcomes.jsonl", "append-only outcomes file")
	withSports := flag.String("sports", "", "also check finished games for these leagues, e.g. mlb")
	dataDir := flag.String("data", "/var/lib/predbot/data", "recorded data dir; Poly slugs of settled games are reconstructed from it")
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
	// Sports games settle within hours — the fastest source of resolved outcomes.
	// SETTLED games (status=settled) are the ones we can record; the old code
	// discovered status=open games, which are never settled, so it recorded
	// nothing. Their results come back inline from one call, so we store them
	// directly rather than re-fetching each ticker.
	var settledSports []sports.SettledResult
	polySlug := map[string]string{} // canonical -> Poly moneyline slug
	if *withSports != "" {
		c := sports.New()
		for _, lg := range splitCSV(*withSports) {
			sr, err := c.SettledKalshi(ctx, lg)
			if err != nil {
				fmt.Println("settled-sports discovery:", err)
				continue
			}
			settledSports = append(settledSports, sr...)
		}
		// Poly slugs for settled games come from the RECORDED data, not live
		// discovery: Poly's forward-looking listing has rolled a game off by the
		// time Kalshi marks it settled, and Poly's own market stays OPEN (no
		// resolved outcome exposed) for ~2 weeks. The recorded slugs were matched
		// to Kalshi at record time on agreeing dates, so their canonicals line up
		// with the settled Kalshi canonicals; we record the co-listed Kalshi
		// result against the real Poly slug (same MLB game, same away=YES polarity).
		polySlug = scanRecordedPolySlugs(*dataDir, splitCSV(*withSports))
	}

	const kBase = config.KalshiAPI
	const pBase = config.PolyGateway
	settled := 0
	for _, t := range targets {
		if t.kTicker != "" && !store.Has(core.VenueKalshi, t.kTicker) {
			if r, ok, err := outcome.FetchKalshi(ctx, kBase, t.kTicker); err == nil && ok {
				r.Canonical = t.canonical
				if store.Put(r) == nil {
					settled++
					fmt.Printf("SETTLED kalshi %-34s -> %s\n", r.VenueID, r.Outcome)
				}
			}
		}
		if t.pSlug != "" && !store.Has(core.VenuePoly, t.pSlug) {
			if r, ok, err := outcome.FetchPolyUS(ctx, pBase, t.pSlug); err == nil && ok {
				r.Canonical = t.canonical
				if store.Put(r) == nil {
					settled++
					fmt.Printf("SETTLED poly   %-34s -> %s\n", r.VenueID, r.Outcome)
				}
			}
		}
	}
	// Settled sports outcomes carry their result inline from discovery, so store
	// them directly (append-only store dedups on the ticker).
	now := time.Now().UTC().Format(time.RFC3339)
	for _, sr := range settledSports {
		if !store.Has(core.VenueKalshi, sr.Ticker) {
			r := outcome.Result{
				Canonical: sr.Canonical, Venue: core.VenueKalshi, VenueID: sr.Ticker,
				Outcome: sr.Result, Status: sr.Status, CloseTime: sr.CloseTime,
				RecordedAt: now,
			}
			if store.Put(r) == nil {
				settled++
				fmt.Printf("SETTLED kalshi %-34s -> %s\n", sr.Ticker, sr.Result)
			}
		}
		// Co-listed Poly market resolves to the same game (away=YES on both), so
		// record the Kalshi result against the Poly slug. Only away-team
		// canonicals match — Poly lists just the away-team YES instrument.
		if slug, ok := polySlug[sr.Canonical]; ok && !store.Has(core.VenuePoly, slug) {
			r := outcome.Result{
				Canonical: sr.Canonical, Venue: core.VenuePoly, VenueID: slug,
				Outcome: sr.Result, Status: "kalshi-colisted", RecordedAt: now,
			}
			if store.Put(r) == nil {
				settled++
				fmt.Printf("SETTLED poly   %-34s -> %s (via co-listed Kalshi)\n", slug, sr.Result)
			}
		}
	}
	fmt.Printf("checked %d static + %d settled sports; newly settled %d; total recorded %d (was %d)\n",
		len(targets), len(settledSports), settled, store.Count(), before)
	_ = time.Now()
}

// scanRecordedPolySlugs reads the recorded Polymarket book files and returns
// canonical -> moneyline slug for every game we have data for. It regex-scans
// the first chunk of each hourly file (distinct slugs repeat immediately, so a
// bounded read finds them all) rather than fully parsing the archive — this runs
// hourly and Poly's stream is large.
func scanRecordedPolySlugs(dir string, leagues []string) map[string]string {
	out := map[string]string{}
	res := map[string]*regexp.Regexp{}
	for _, lg := range leagues {
		res[lg] = regexp.MustCompile(`aec-` + regexp.QuoteMeta(lg) + `-[a-z]+-[a-z]+-\d{4}-\d{2}-\d{2}`)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "raw", "polymarketus-*.jsonl.gz"))
	for _, f := range files {
		if strings.Contains(filepath.Base(f), "-trades-") {
			continue // book files only
		}
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		gz, err := gzip.NewReader(fh)
		if err != nil {
			fh.Close()
			continue
		}
		sc := bufio.NewScanner(gz)
		sc.Buffer(make([]byte, 1024*1024), 8*1024*1024)
		for lines := 0; lines < 20000 && sc.Scan(); lines++ {
			line := sc.Bytes()
			for lg, re := range res {
				if m := re.Find(line); m != nil {
					if canon, ok := sports.CanonicalFromPolySlug(lg, string(m)); ok {
						out[canon] = string(m)
					}
				}
			}
		}
		gz.Close()
		fh.Close()
	}
	return out
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
