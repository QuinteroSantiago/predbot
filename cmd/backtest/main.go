// Command backtest is Phase 2 (design §17): replay recorded snapshots through
// the same Book Manager and Strategy as live, with the SimExecutor in place of
// the venues, and print the §10.5 report. Deterministic — no wall-clock, driven
// entirely by the recorded TsRecv stream.
//
// Deliverable: a number, positive or negative, with receipts.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"predbot/internal/backtest"
	"predbot/internal/book"
	"predbot/internal/config"
	"predbot/internal/core"
	"predbot/internal/sports"
	"predbot/internal/strategy"
	"predbot/internal/venue"
)

func main() {
	cfgPath := flag.String("config", "config/config.yaml", "path to config.yaml")
	mktPath := flag.String("markets", "config/markets.yaml", "path to markets.yaml")
	dataDir := flag.String("data", "", "data dir to replay (default: config data_dir)")
	skewMS := flag.Int64("skew", -1, "override max cross-venue skew in ms (-1 = use config)")
	hours := flag.Int("hours", 0, "only replay the most recent N hours (0 = use -days)")
	days := flag.Int("days", 0, "only replay the most recent N days of data (0 = all; large archives need a bound)")
	leadlag := flag.Bool("leadlag", false, "read-only: measure whether one venue's mid leads the other (cross-venue lead-lag), no trading")
	longshot := flag.Bool("longshot", false, "read-only: calibration / favorite-longshot bias from opening price vs settled outcome")
	outcomesPath := flag.String("outcomes", "", "outcomes.jsonl for -longshot (default: <data>/outcomes.jsonl)")
	adverse := flag.Bool("adverse", false, "measure adverse selection: where price goes AFTER a hypothetical maker fill")
	maker := flag.Bool("maker", false, "use the MAKER strategy: rest one leg (earns the Poly rebate) and take the other")
	exportPath := flag.String("export", "", "write detected opportunities to this JSON file")
	spreadPath := flag.String("export-spread", "", "write sampled cross-venue spread to this JSON file")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		slog.Error("config load failed", "err", err)
		os.Exit(1)
	}
	markets, err := config.LoadMarkets(*mktPath)
	if err != nil {
		slog.Error("markets load failed", "err", err)
		os.Exit(1)
	}
	dir := *dataDir
	if dir == "" {
		dir = cfg.DataDir
	}

	window := *hours
	if window == 0 {
		window = *days * 24
	}
	// Streaming: the full archive does not fit in memory (a 1.3 GB load was
	// OOM-killed), so we make ONE forward pass without materialising it.
	// A first pass collects the venue ids present, so sports markets — which are
	// discovered at runtime and never written to markets.yaml — can be mapped.
	var kTickers, pSlugs []string
	seenID := map[string]bool{}
	var nSnaps int
	var firstTs, lastTs int64
	if err := backtest.StreamReplay(dir, window, func(s core.BookSnapshot) error {
		nSnaps++
		if firstTs == 0 {
			firstTs = s.TsRecv
		}
		lastTs = s.TsRecv
		id := s.Venue + "|" + s.VenueID
		if !seenID[id] {
			seenID[id] = true
			switch s.Venue {
			case "kalshi":
				kTickers = append(kTickers, s.VenueID)
			case "polymarketus":
				pSlugs = append(pSlugs, s.VenueID)
			}
		}
		return nil
	}); err != nil {
		slog.Error("replay scan failed", "err", err, "dir", dir)
		os.Exit(1)
	}
	if nSnaps == 0 {
		fmt.Printf("no recorded snapshots under %s — record some data first\n", dir)
		return
	}

	bm := book.NewManager(markets)

	// Sports markets are discovered at runtime and aren't in markets.yaml, so
	// their canonical mappings come from the venue ids seen in the scan above.
	sportsMarkets := sportsToMarkets(sports.ReconstructFromIDs(kTickers, pSlugs))
	bm.AddMarkets(sportsMarkets)
	if len(sportsMarkets) > 0 {
		fmt.Printf("reconstructed %d cross-listed sports markets from recorded data\n", len(sportsMarkets))
	}
	// Lead-lag is a standalone read-only pass: feed each venue's YES mid into the
	// analyzer (canonical via a pure lookup, no book mutation) and print the
	// report. It never touches the trading sim below.
	if *leadlag {
		ll := backtest.NewLeadLag()
		_ = backtest.StreamReplay(dir, window, func(s core.BookSnapshot) error {
			canonical, ok := bm.Canonical(s.Venue, s.VenueID)
			if !ok {
				return nil
			}
			bid, okB := s.BestYesBid()
			ask, okA := s.BestYesAsk()
			if !okB || !okA {
				return nil // one-sided book: no valid mid
			}
			mid := float64(int64(bid.Px)+int64(ask.Px)) / 2 / 100 // cents
			ll.Observe(canonical, s.Venue, mid, s.TsRecv)
			return nil
		})
		span := time.Duration(lastTs-firstTs) * time.Nanosecond
		fmt.Printf("Replayed %d snapshots over %s across %d markets\n\n",
			nSnaps, span.Round(time.Minute), len(bm.Canonicals()))
		fmt.Print(ll.Report())
		return
	}

	// Favorite-longshot / calibration: join settled outcomes to each market's
	// OPENING recorded mid (the first two-sided price = a pre-game forecast), then
	// compare implied prob to realized YES. Standalone read-only pass.
	if *longshot {
		op := *outcomesPath
		if op == "" {
			op = filepath.Join(dir, "outcomes.jsonl")
		}
		outcomes, err := loadOutcomes(op)
		if err != nil {
			slog.Error("outcomes load failed", "err", err, "path", op)
			os.Exit(1)
		}
		if len(outcomes) == 0 {
			fmt.Printf("no settled outcomes in %s — run cmd/outcomes first\n", op)
			return
		}
		ls := backtest.NewLongshot(outcomes)
		_ = backtest.StreamReplay(dir, window, func(s core.BookSnapshot) error {
			bid, okB := s.BestYesBid()
			ask, okA := s.BestYesAsk()
			if !okB || !okA {
				return nil
			}
			prob := float64(int64(bid.Px)+int64(ask.Px)) / 2 / float64(core.OneDollar)
			ls.ObserveMid(s.Venue+"|"+s.VenueID, prob)
			return nil
		})
		fmt.Printf("Loaded %d settled outcomes; replayed %d snapshots\n\n", len(outcomes), nSnaps)
		fmt.Print(ls.Report())
		return
	}

	fees := backtest.Fees{KalshiMult: cfg.Backtest.KalshiFeeMult}
	rep := backtest.NewReport()
	sim := backtest.NewSim(fees, cfg.Backtest.LatencyMS, rep)
	scfg := strategy.Config{
		MinEdge: core.Price(cfg.Backtest.MinEdge),
		MaxClip: core.Qty(cfg.Backtest.MaxClip),
	}
	maxAge := cfg.Backtest.MaxBookAgeMS * int64(time.Millisecond)
	maxSkew := cfg.Backtest.MaxSkewMS * int64(time.Millisecond)
	if *skewMS >= 0 {
		maxSkew = *skewMS * int64(time.Millisecond)
	}

	// These are hold-to-resolution arbs: entering a standing gap locks capital on
	// both venues until the market resolves (design §10.4), so you enter each
	// market's gap ONCE and cannot recycle that capital. Model that — enter the
	// first time a market shows edge, never re-enter (design §9: don't re-emit an
	// opportunity you're already working). Counting per-tick instead would treat
	// one persistent mispricing as thousands of independent trades.
	entered := map[string]bool{}
	// active tracks whether a gap is currently open per market, so we log one
	// Opportunity per episode (gap opens) rather than per tick — "every trade we
	// could have made", distinct from the hold-to-resolution subset `entered`.
	active := map[string]bool{}
	var opps []backtest.Opportunity
	var spread []backtest.SpreadPoint
	// Maker mode: track resting orders and whether the market ever crossed them.
	resting := map[string]*backtest.RestingFill{}
	restTaking := map[string]core.Intent{} // the leg we'll take once the rest fills
	var restsPlaced, restsFilled int
	var adv *backtest.AdverseSelection
	var advTrades []venue.Trade
	var advIdx int
	if *adverse {
		adv = backtest.NewAdverseSelection()
		// Adverse selection is driven by REAL TRADE PRINTS — a book cannot tell a
		// trade from a cancellation, so books alone cannot answer this.
		advTrades, err = backtest.LoadTrades(dir, window)
		if err != nil {
			slog.Warn("trade load failed; adverse selection unavailable", "err", err)
		}
		if len(advTrades) == 0 {
			slog.Warn("NO trade prints recorded in this window — adverse selection cannot be measured. " +
				"The recorder must be running with trade feeds (data/trades/).")
		}
	}
	lastBucket := map[string]int64{} // 5-min sampling per market

	// Resolution dates for the annualized-return calc (§10.4).
	resolution := map[string]time.Time{}
	for _, m := range markets {
		if t, err := time.Parse("2006-01-02", m.ResolutionDate); err == nil {
			resolution[m.Canonical] = t
		}
	}

	// Second streaming pass: the actual replay (design §10.1).
	_ = backtest.StreamReplay(dir, window, func(s core.BookSnapshot) error {
		now := s.TsRecv
		sim.Advance(now, bm) // settle fills whose t+Δ has arrived
		canonical, ok := bm.Update(s)
		if !ok {
			return nil // not in markets.yaml (§12)
		}
		books, ready := bm.Pair(canonical)
		if !ready || staleAny(books, now, maxAge) || skewExceeds(books, maxSkew) {
			return nil
		}
		if *spreadPath != "" {
			if bucket := (now / 1_000_000) / 300_000; lastBucket[canonical] != bucket {
				if g, ok := backtest.CrossGrossCents(books); ok {
					spread = append(spread, backtest.SpreadPoint{
						TsMs: now / 1_000_000, Canonical: canonical,
						Category: backtest.Category(canonical), GrossEdgeCents: g})
				}
				lastBucket[canonical] = bucket
			}
		}
		if adv != nil {
			adv.ObserveBook(canonical, s)
			// Feed any trades that happened up to this snapshot's time.
			for advIdx < len(advTrades) && advTrades[advIdx].TsRecv <= s.TsRecv {
				t := advTrades[advIdx]
				if c, ok2 := bm.Canonical(t.Venue, t.VenueID); ok2 {
					adv.ObserveTrade(c, t)
				}
				advIdx++
			}
		}

		// Advance any resting order for this market against the new book.
		if r, ok := resting[canonical]; ok {
			if r.Observe(books[r.Intent.Venue]) > 0 && r.Complete() {
				restsFilled++
				// The rest actually filled — now book the pair's real economics:
				// maker fee (a rebate on Poly) on the rest, taker on the hedge.
				sim.SettleMakerPair(canonical, r.Intent, restTaking[canonical], r.Filled, bm)
				delete(resting, canonical)
				delete(restTaking, canonical)
			}
		}

		exp := strategy.Exposure{InFlight: entered}
		var legs []core.Intent
		if *maker {
			legs = strategy.MakerCrossVenue(canonical, books, exp, fees, scfg)
		} else {
			legs = strategy.CrossVenue(canonical, books, exp, fees, scfg)
		}
		switch {
		case len(legs) > 0:
			if !active[canonical] { // new episode -> log an opportunity
				o := backtest.BuildOpportunity(legs[0], legs[1], fees, now, resolution[canonical])
				o.Canonical, o.Category = canonical, backtest.Category(canonical)
				opps = append(opps, o)
				active[canonical] = true
			}
			if !entered[canonical] {
				if ri := strategy.RestingLeg(legs); *maker && ri >= 0 {
					// The resting leg does NOT fill on submission — it fills only
					// if the market later crosses it. Track it and count honestly.
					if _, dup := resting[canonical]; !dup {
						resting[canonical] = backtest.NewRestingFill(legs[ri], books[legs[ri].Venue])
						restTaking[canonical] = legs[1-ri] // the other leg is taken on fill
						restsPlaced++
					}
				} else {
					sim.Submit(canonical, legs, now)
				}
				entered[canonical] = true // hold to resolution; never re-enter
			}
		default:
			active[canonical] = false // gap closed; a reopen is a new episode
		}
		for _, l := range strategy.IntraVenue(canonical, books, fees, scfg) {
			sim.Submit(canonical, []core.Intent{l}, now)
		}
		return nil
	})
	sim.Flush(bm)

	if *exportPath != "" {
		if err := backtest.WriteOpportunities(*exportPath, opps); err != nil {
			slog.Warn("opportunity export failed", "err", err)
		} else {
			fmt.Printf("exported %d opportunities to %s\n", len(opps), *exportPath)
		}
	}
	if *spreadPath != "" {
		if err := backtest.WriteSpread(*spreadPath, spread); err != nil {
			slog.Warn("spread export failed", "err", err)
		} else {
			fmt.Printf("exported %d spread samples to %s\n", len(spread), *spreadPath)
		}
	}

	rep.Finalize(resolution, cfg.Backtest.IdleYield)

	span := time.Duration(lastTs-firstTs) * time.Nanosecond
	fmt.Printf("Replayed %d snapshots (streaming) over %s across %d markets\n\n",
		nSnaps, span.Round(time.Minute), len(bm.Canonicals()))
	if adv != nil {
		fmt.Print(adv.Report())
		fmt.Println()
	}
	if *maker {
		pct := 0.0
		if restsPlaced > 0 {
			pct = 100 * float64(restsFilled) / float64(restsPlaced)
		}
		fmt.Printf("MAKER MODE: rests placed %d, rests FILLED %d (%.1f%%)\n"+
			"  (a rest only fills when the market crosses it; unfilled rests earn nothing)\n\n",
			restsPlaced, restsFilled, pct)
	}
	fmt.Print(rep.String())
}

// loadOutcomes reads outcomes.jsonl into "venue|venueID" -> 1 (yes) / 0 (no).
func loadOutcomes(path string) (map[string]int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]int{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 4*1024*1024)
	for sc.Scan() {
		var r struct {
			Venue   string `json:"venue"`
			VenueID string `json:"venue_id"`
			Outcome string `json:"outcome"`
		}
		if json.Unmarshal(sc.Bytes(), &r) != nil || r.VenueID == "" {
			continue
		}
		y := 0
		if r.Outcome == "yes" {
			y = 1
		} else if r.Outcome != "no" {
			continue // only settled yes/no
		}
		out[r.Venue+"|"+r.VenueID] = y
	}
	return out, sc.Err()
}

func staleAny(books map[string]core.BookSnapshot, now, maxAge int64) bool {
	for _, b := range books {
		if now-b.TsRecv > maxAge {
			return true
		}
	}
	return false
}

// skewExceeds reports whether the venues' snapshots were captured too far apart
// in time to trust a cross-venue comparison. With independent REST polling the
// two books can be ~1.5s apart on average, so a gap seen here may be an artifact
// of comparing a fresh book to a stale one — the very staleness WS would fix.
func skewExceeds(books map[string]core.BookSnapshot, maxSkew int64) bool {
	var min, max int64
	first := true
	for _, b := range books {
		if first || b.TsRecv < min {
			min = b.TsRecv
		}
		if first || b.TsRecv > max {
			max = b.TsRecv
		}
		first = false
	}
	return max-min > maxSkew
}

// sportsToMarkets converts reconstructed sports matches into config.Market rows.
func sportsToMarkets(matches []sports.Match) []config.Market {
	out := make([]config.Market, 0, len(matches))
	for _, m := range matches {
		var mk config.Market
		mk.Canonical = m.Canonical
		mk.Venues.Kalshi.Ticker = m.KalshiTicker
		mk.Venues.PolymarketUS.Slug = m.PolySlug
		out = append(out, mk)
	}
	return out
}
