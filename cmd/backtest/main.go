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
	"sync/atomic"
	"time"

	"predbot/internal/backtest"
	"predbot/internal/book"
	"predbot/internal/config"
	"predbot/internal/core"
	"predbot/internal/progress"
	"predbot/internal/sports"
	"predbot/internal/strategy"
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
	showProgress := flag.Bool("progress", true, "show progress on stderr: a live bar on a terminal, a status line every 30s when redirected")
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
	scan := scanBar(*showProgress, dir, window)
	if err := backtest.StreamReplay(dir, window, func(s core.BookSnapshot) error {
		scan.observe(s.TsRecv)
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
		scan.bar.Finish()
		slog.Error("replay scan failed", "err", err, "dir", dir)
		os.Exit(1)
	}
	scan.bar.Finish()
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
		bar := countBar(*showProgress, "pass 2/2 lead-lag", nSnaps)
		_ = backtest.StreamReplay(dir, window, func(s core.BookSnapshot) error {
			bar.Add(1)
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
		bar.Finish()
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
		bar := countBar(*showProgress, "pass 2/2 longshot", nSnaps)
		_ = backtest.StreamReplay(dir, window, func(s core.BookSnapshot) error {
			bar.Add(1)
			bid, okB := s.BestYesBid()
			ask, okA := s.BestYesAsk()
			if !okB || !okA {
				return nil
			}
			prob := float64(int64(bid.Px)+int64(ask.Px)) / 2 / float64(core.OneDollar)
			ls.ObserveMid(s.Venue+"|"+s.VenueID, prob)
			return nil
		})
		bar.Finish()
		fmt.Printf("Loaded %d settled outcomes; replayed %d snapshots\n\n", len(outcomes), nSnaps)
		fmt.Print(ls.Report())
		return
	}

	fees := backtest.Fees{KalshiMult: cfg.Backtest.KalshiFeeMult}
	maxSkew := cfg.Backtest.MaxSkewMS * int64(time.Millisecond)
	if *skewMS >= 0 {
		maxSkew = *skewMS * int64(time.Millisecond)
	}
	opts := backtest.TradingOpts{
		Fees:      fees,
		LatencyMS: cfg.Backtest.LatencyMS,
		Strategy: strategy.Config{
			MinEdge: core.Price(cfg.Backtest.MinEdge),
			MaxClip: core.Qty(cfg.Backtest.MaxClip),
		},
		MaxAge:     cfg.Backtest.MaxBookAgeMS * int64(time.Millisecond),
		MaxSkew:    maxSkew,
		Resolution: map[string]time.Time{},
		Maker:      *maker,
		Spread:     *spreadPath != "",
	}
	if *adverse {
		opts.Adverse = backtest.NewAdverseSelection()
		// Adverse selection is driven by REAL TRADE PRINTS — a book cannot tell a
		// trade from a cancellation, so books alone cannot answer this.
		opts.Trades, err = backtest.LoadTrades(dir, window)
		if err != nil {
			slog.Warn("trade load failed; adverse selection unavailable", "err", err)
		}
		if len(opts.Trades) == 0 {
			slog.Warn("NO trade prints recorded in this window — adverse selection cannot be measured. " +
				"The recorder must be running with trade feeds (data/trades/).")
		}
	}

	// Resolution dates for the annualized-return calc (§10.4).
	for _, m := range markets {
		if t, err := time.Parse("2006-01-02", m.ResolutionDate); err == nil {
			opts.Resolution[m.Canonical] = t
		}
	}

	// Second streaming pass: the actual replay (design §10.1).
	allMarkets := append(append([]config.Market{}, markets...), sportsMarkets...)
	bar := countBar(*showProgress, "pass 2/2 replay", nSnaps)
	opts.Progress = bar.Counter()
	res, err := backtest.RunTrading(dir, window, allMarkets, opts)
	bar.Finish()
	if err != nil {
		slog.Warn("trading replay stopped early", "err", err)
	}
	opps, spread, rep := res.Opps, res.Spread, res.Report
	adv := opts.Adverse
	restsPlaced, restsFilled := res.RestsPlaced, res.RestsFilled

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

	rep.Finalize(opts.Resolution, cfg.Backtest.IdleYield)

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

// scanProgress tracks the id-scan pass. Its snapshot count isn't known until
// the pass ends, so the bar measures how far through the archive's time span
// the replay has got.
type scanProgress struct {
	bar    *progress.Bar
	fromNs int64
	seen   atomic.Int64
}

func scanBar(on bool, dir string, window int) *scanProgress {
	sp := &scanProgress{}
	if !on {
		return sp
	}
	from, to, ok, err := backtest.ArchiveSpan(dir, window)
	if err != nil || !ok {
		return sp
	}
	sp.fromNs = from.UnixNano()
	sp.bar = progress.Start("pass 1/2 scan  ", to.UnixNano()-sp.fromNs, func(done, _ int64, elapsed time.Duration) string {
		n := sp.seen.Load()
		return fmt.Sprintf("through %s  %s snapshots  %s/s",
			time.Unix(0, sp.fromNs+done).UTC().Format("2006-01-02 15:04"),
			progress.Count(n), progress.Count(int64(float64(n)/elapsed.Seconds())))
	})
	return sp
}

func (sp *scanProgress) observe(tsRecv int64) {
	if sp.bar == nil {
		return
	}
	sp.seen.Add(1)
	sp.bar.Set(tsRecv - sp.fromNs)
}

// countBar shows a pass over a known number of snapshots; nil when off.
func countBar(on bool, label string, total int) *progress.Bar {
	if !on {
		return nil
	}
	return progress.Start(label, int64(total), func(done, total int64, elapsed time.Duration) string {
		return fmt.Sprintf("%s/%s snapshots  %s/s",
			progress.Count(done), progress.Count(total), progress.Count(int64(float64(done)/elapsed.Seconds())))
	})
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
