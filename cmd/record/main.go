// Command record is Phase 1 (design §17): stream both venues, write raw and
// normalized frames to disk, export metrics. No trading, no strategy. Runs under
// systemd; SIGHUP closes the current files cleanly (they reopen on next write),
// SIGINT/SIGTERM shut down.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"predbot/internal/backtest"
	"predbot/internal/book"
	"predbot/internal/config"
	"predbot/internal/core"
	"predbot/internal/exec"
	"predbot/internal/journal"
	"predbot/internal/live"
	"predbot/internal/obs"
	"predbot/internal/sports"
	"predbot/internal/strategy"
	"predbot/internal/venue"
	"predbot/internal/venue/kalshi"
	"predbot/internal/venue/polymarketus"
	"predbot/internal/venue/public"
)

func main() {
	cfgPath := flag.String("config", "config/config.yaml", "path to config.yaml")
	mktPath := flag.String("markets", "config/markets.yaml", "path to markets.yaml")
	publicMode := flag.Bool("public", false, "record from PUBLIC REST polling instead of authenticated WS (no credentials needed; design §16 open item #5)")
	sportsLeagues := flag.String("sports", "", "comma-separated leagues to auto-subscribe daily games (e.g. \"mlb\"); WS mode only")
	noNormalized := flag.Bool("no-normalized", false, "don't write the normalized stream (regenerable from raw; saves ~35% disk)")
	keepEdgeCents := flag.Float64("keep-edge-cents", 2.0, "mark an hour as KEEP-FOREVER when any market's cross-venue edge reaches this")
	paperMode := flag.Bool("paper", false, "run live paper trading (SimExecutor on the live feed; no real orders, no money)")
	flag.Parse()

	rootCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	shutdown := obs.Setup(rootCtx)
	defer func() { _ = shutdown(context.Background()) }()

	met, err := obs.NewMetrics()
	if err != nil {
		slog.Error("metric init failed", "err", err)
		os.Exit(1)
	}

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

	rec, err := journal.New(cfg.DataDir)
	if err != nil {
		slog.Error("recorder init failed", "err", err)
		os.Exit(1)
	}

	keeper, err := journal.NewKeeper(cfg.DataDir)
	if err != nil {
		slog.Error("keeper init failed", "err", err)
		os.Exit(1)
	}

	// predbot_data_bytes{stream}: on-disk footprint of what we've recorded.
	if err := obs.RegisterDataSizeGauge(cfg.DataDir); err != nil {
		slog.Warn("data-size gauge disabled", "err", err)
	}

	// Live cross-venue arb surface: the drain loop joins both venues' latest
	// books per market and stores the current gross edge; a gauge publishes the
	// fresh ones so Grafana can render arbs in real time.
	bm := book.NewManager(markets)
	type liveEdge struct {
		cents  float64
		size   float64
		cat    string
		oldest int64
	}
	var edgeMu sync.Mutex
	liveEdges := map[string]liveEdge{}
	// lastTrade[canonical] = TsRecv (nanos) of the most recent actual trade on
	// either venue. It converts a book edge into a real-opportunity signal: a wide
	// edge whose market has not printed in a long time repriced on zero volume.
	lastTrade := map[string]int64{}
	if err := obs.RegisterCrossEdgeGauge(func() []obs.MarketEdge {
		edgeMu.Lock()
		defer edgeMu.Unlock()
		now := time.Now().UnixNano()
		out := make([]obs.MarketEdge, 0, len(liveEdges))
		for mkt, e := range liveEdges {
			if now-e.oldest > int64(20*time.Second) {
				continue // a venue went quiet — drop rather than report a stale edge
			}
			ageSec := float64(obs.NoTradeAgeSec)
			if ts, ok := lastTrade[mkt]; ok {
				ageSec = time.Duration(now - ts).Seconds()
			}
			out = append(out, obs.MarketEdge{Market: mkt, Category: e.cat, Cents: e.cents, Size: e.size, TradeAgeSec: ageSec})
		}
		return out
	}); err != nil {
		slog.Warn("cross-edge gauge disabled", "err", err)
	}

	// Signals: SIGHUP rotates/closes files; SIGINT/SIGTERM shut down.
	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		for s := range sigCh {
			switch s {
			case syscall.SIGHUP:
				slog.Info("SIGHUP: flushing open files")
				if err := rec.Close(); err != nil {
					slog.Warn("flush on SIGHUP failed", "err", err)
				}
			default:
				slog.Info("shutdown signal", "signal", s.String())
				cancel()
				return
			}
		}
	}()

	// Build authenticated dialers. AuthHeaders return errors when creds are
	// missing (design §14); the dialer surfaces them so a mis-provisioned box
	// fails loudly instead of connecting unauthenticated.
	kDial := venue.Dialer(func(ctx context.Context) (venue.Conn, error) {
		h, err := kalshi.AuthHeaders("GET", cfg.Kalshi.Path)
		if err != nil {
			return nil, err
		}
		return venue.DialWS(cfg.Kalshi.WSURL, h)(ctx)
	})
	polyPath := "/v1/ws/markets"
	if u, err := url.Parse(cfg.PolymarketUS.WSURL); err == nil && u.Path != "" {
		polyPath = u.Path
	}
	pDial := venue.Dialer(func(ctx context.Context) (venue.Conn, error) {
		h, err := polymarketus.AuthHeaders("GET", polyPath)
		if err != nil {
			return nil, err
		}
		return venue.DialWS(cfg.PolymarketUS.WSURL, h)(ctx)
	})

	// Total portfolio value (Grafana): poll each venue's available balance on a
	// slow ticker and publish it as predbot_balance_usd{venue}; the dashboard sums
	// the series. This lives in the always-on recorder so the number shows even
	// when the trade-only live bot is idle. Authenticated mode only — public REST
	// polling has no credentials to read an account. Reads happen in this
	// goroutine, never in the metric callback, so venue REST stays off the scrape
	// path; on an error we keep the last-known value rather than blanking it.
	if !*publicMode {
		kSign := func(m, p string) (http.Header, error) { return kalshi.AuthHeaders(m, "/trade-api/v2"+p) }
		pSign := func(m, p string) (http.Header, error) { return polymarketus.AuthHeaders(m, p) }
		accts := []live.AccountClient{
			live.NewKalshiAccount("https://api.elections.kalshi.com/trade-api/v2", kSign),
			live.NewPolyUSAccount("https://api.polymarket.us", pSign),
		}
		var balMu sync.Mutex
		balances := map[string]float64{}
		if err := obs.RegisterBalanceGauge(func() []obs.VenueBalance {
			balMu.Lock()
			defer balMu.Unlock()
			out := make([]obs.VenueBalance, 0, len(balances))
			for v, usd := range balances {
				out = append(out, obs.VenueBalance{Venue: v, USD: usd})
			}
			return out
		}); err != nil {
			slog.Warn("balance gauge disabled", "err", err)
		}
		go func() {
			poll := func() {
				for _, a := range accts {
					bctx, bcancel := context.WithTimeout(rootCtx, 15*time.Second)
					usd, err := a.BalanceUSD(bctx)
					bcancel()
					if err != nil {
						slog.Warn("balance poll failed", "venue", a.Venue(), "err", err)
						continue // keep last-known rather than reporting a false zero
					}
					balMu.Lock()
					balances[a.Venue()] = usd
					balMu.Unlock()
				}
			}
			poll()
			t := time.NewTicker(60 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-rootCtx.Done():
					return
				case <-t.C:
					poll()
				}
			}
		}()
	}

	var ingesters []struct {
		ing     venue.Ingester
		markets []string
	}
	if *publicMode {
		interval := time.Duration(cfg.PollIntervalMS) * time.Millisecond
		slog.Info("recording from PUBLIC REST (no credentials)", "interval", interval)
		ingesters = []struct {
			ing     venue.Ingester
			markets []string
		}{
			{public.NewKalshiREST(cfg.Kalshi.RESTURL, 10, interval, rec, met), config.KalshiTickers(markets)},
			{public.NewPolyUSREST(cfg.PolymarketUS.RESTURL, interval, rec, met), config.PolyUSSlugs(markets)},
		}
	} else {
		ingesters = []struct {
			ing     venue.Ingester
			markets []string
		}{
			{kalshi.New(kDial, rec, met), config.KalshiTickers(markets)},
			{polymarketus.New(pDial, rec, met), config.PolyUSSlugs(markets)},
		}
	}

	// Each ingester emits into a shared channel. A fast drain goroutine updates
	// the book manager, live edge, and book-age metric, then hands the snapshot
	// to a dedicated archival writer: gzip + disk (WriteNormalized) runs off the
	// trading/metric path, so book age reflects feed freshness rather than our
	// own compression backlog. The recorder is concurrency-safe.
	snaps := make(chan core.BookSnapshot, 1024)
	var wg sync.WaitGroup
	for _, it := range ingesters {
		if len(it.markets) == 0 {
			slog.Warn("no markets configured for venue, skipping", "venue", it.ing.Name())
			continue
		}
		wg.Add(1)
		go func(ing venue.Ingester, mkts []string) {
			defer wg.Done()
			if err := ing.Stream(rootCtx, mkts, snaps); err != nil && rootCtx.Err() == nil {
				slog.Error("ingester stopped", "venue", ing.Name(), "err", err)
			}
		}(it.ing, it.markets)
	}

	// Book-age reporter: newest snapshot age per venue (design §13).
	go serveHealth()

	// Trade feeds: books show what is OFFERED, trades show what TRANSACTED. Only
	// trades make maker fills (and thus adverse selection) measurable, and they
	// give real traded volume rather than advertised depth.
	if !*publicMode {
		writeTrade := func(t venue.Trade) {
			// Stamp the market's last-trade time so the cross-edge gauge can flag a
			// wide edge that repriced with no volume behind it (a phantom).
			if canonical, ok := bm.Canonical(t.Venue, t.VenueID); ok {
				edgeMu.Lock()
				lastTrade[canonical] = t.TsRecv
				edgeMu.Unlock()
			}
			line, err := json.Marshal(t)
			if err != nil {
				return
			}
			if err := rec.WriteTrade(t.Venue, line, time.Unix(0, t.TsRecv)); err != nil {
				slog.Warn("trade write failed", "venue", t.Venue, "err", err)
			}
		}
		go venue.NewKalshiTrades(kDial, rec).Run(rootCtx, config.KalshiTickers(markets), writeTrade)
		go venue.NewPolyUSTrades(pDial, rec).Run(rootCtx, config.PolyUSSlugs(markets), writeTrade)
	}

	// Dynamic sports subscription: periodically discover cross-listed daily games
	// and (re)subscribe them, feeding the same pipeline (WS mode only — needs auth).
	if leagues := splitCSV(*sportsLeagues); len(leagues) > 0 && !*publicMode {
		go (&sports.Supervisor{Leagues: leagues, KDial: kDial, PDial: pDial,
			Rec: rec, Met: met, BM: bm, Out: snaps}).Run(rootCtx)
	}

	// Phase 3a paper trading: run the strategy live against the shared Book
	// Manager and simulate fills (no real orders). Reuses the whole live feed.
	if *paperMode {
		fees := backtest.Fees{KalshiMult: cfg.Backtest.KalshiFeeMult}
		pt := exec.NewPaperTrader(fees, cfg.Backtest.LatencyMS, bm, filepath.Join(cfg.DataDir, "paper-fills.jsonl"))
		if err := obs.RegisterPaperGauges(func() obs.PaperStats {
			s := pt.Snapshot()
			return obs.PaperStats{NetUSD: s.NetUSD, GrossUSD: s.GrossUSD, FeesUSD: s.FeesUSD,
				CapitalUSD: s.CapitalUSD, Opportunities: s.Attempted, OpenPositions: s.OpenPositions, Faded: s.Faded}
		}); err != nil {
			slog.Warn("paper gauges disabled", "err", err)
		}
		go pt.Run(rootCtx)
		scfg := strategy.Config{MinEdge: core.Price(cfg.Backtest.MinEdge), MaxClip: core.Qty(cfg.Backtest.MaxClip)}
		maxAge := cfg.Backtest.MaxBookAgeMS * int64(time.Millisecond)
		maxSkew := cfg.Backtest.MaxSkewMS * int64(time.Millisecond)
		go runPaper(rootCtx, bm, fees, scfg, maxAge, maxSkew, pt)
		slog.Info("paper trading ON (live SimExecutor, no real orders)")
	}

	// Archival writer: gzip + disk is the slow step, so it runs on its own
	// goroutine off the trading/metric path. A single writer preserves per-file
	// append order; the deep buffer absorbs venue bursts (e.g. Kalshi stamping
	// many markets at the same ms). If the writer ever falls behind for real,
	// the drain's send blocks and book age rises — an honest signal, no silent
	// data loss (matches the old serial behaviour on a genuine disk stall).
	toWrite := make(chan core.BookSnapshot, 8192)
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for s := range toWrite {
			if err := rec.WriteNormalized(s); err != nil {
				slog.Warn("normalized write failed", "venue", s.Venue, "err", err)
			}
		}
	}()

	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		defer close(toWrite) // stop the writer once no more snapshots will arrive
		for {
			select {
			case <-rootCtx.Done():
				return
			case s := <-snaps:
				age := time.Since(time.Unix(0, s.TsRecv)).Seconds()
				met.BookAge.Record(rootCtx, age, obs.Venue(s.Venue))
				// Update the live cross-venue edge for this market. Done before
				// the archival hand-off so the trading view stays fresh even if
				// the writer momentarily backs up.
				if canonical, ok := bm.Update(s); ok {
					if cents, size, oldest, ok := bm.CrossEdge(canonical); ok {
						edgeMu.Lock()
						liveEdges[canonical] = liveEdge{cents, size, marketCategory(canonical), oldest}
						edgeMu.Unlock()
						// An opportunity (or near-miss) appeared: pin this hour and
						// its neighbours so retention never deletes the evidence.
						if cents >= *keepEdgeCents {
							keeper.MarkWindow(time.Unix(0, s.TsRecv),
								fmt.Sprintf("edge %.2fc on %s", cents, canonical))
						}
					}
				}
				if !*noNormalized {
					// Hand off gzip + disk to the archival writer. Selectable on
					// ctx so a stalled writer can't hang shutdown.
					select {
					case toWrite <- s:
					case <-rootCtx.Done():
						return
					}
				}
			}
		}
	}()

	wg.Wait()      // ingesters exit on ctx cancel
	<-drainDone    // drain loop exits on ctx cancel, closing toWrite
	<-writerDone   // archival writer flushes remaining snapshots, then returns
	if err := rec.Close(); err != nil {
		slog.Warn("final flush failed", "err", err)
	}
	slog.Info("recorder stopped cleanly")
}

// serveHealth exposes a liveness endpoint for the landing page / systemd.
func serveHealth() {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	addr := os.Getenv("PREDBOT_HEALTH_ADDR")
	if addr == "" {
		addr = "127.0.0.1:9487"
	}
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Warn("health server stopped", "err", err)
	}
}

// marketCategory groups a canonical id for the live dashboard.
func marketCategory(canonical string) string {
	switch {
	case strings.HasPrefix(canonical, "BTC-"):
		return "Crypto"
	case strings.HasPrefix(canonical, "EMMY-"):
		return "Culture"
	case strings.HasPrefix(canonical, "FED-"):
		return "Macro"
	case strings.HasPrefix(canonical, "TECH-"):
		return "Tech"
	case strings.HasPrefix(canonical, "HOUSE-"), strings.HasPrefix(canonical, "SENATE-"), strings.HasPrefix(canonical, "GOV-"):
		return "Elections"
	case strings.HasPrefix(canonical, "MLB-"), strings.HasPrefix(canonical, "NFL-"), strings.HasPrefix(canonical, "NBA-"):
		return "Sports"
	default:
		return "Other"
	}
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// runPaper evaluates the cross-venue strategy against the live Book Manager on a
// ticker and submits detected pairs to the paper trader (design §17 Phase 3a).
// Same gates as the backtest (freshness + cross-venue skew) and the same
// hold-to-resolution "enter once" suppression per market.
func runPaper(ctx context.Context, bm *book.Manager, fees strategy.FeeModel, scfg strategy.Config, maxAge, maxSkew int64, pt *exec.PaperTrader) {
	entered := map[string]bool{}
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			now := time.Now().UnixNano()
			for _, canonical := range bm.Canonicals() {
				books, ok := bm.Pair(canonical)
				if !ok || staleAny(books, now, maxAge) || skewExceeds(books, maxSkew) {
					continue
				}
				// Hold-to-resolution, matching the backtest: enter a market's gap
				// once and never re-enter (capital is locked until it resolves).
				if entered[canonical] {
					continue
				}
				if legs := strategy.CrossVenue(canonical, books, strategy.Exposure{InFlight: entered}, fees, scfg); len(legs) > 0 {
					pt.Submit(canonical, legs)
					entered[canonical] = true
				}
			}
		}
	}
}

func staleAny(books map[string]core.BookSnapshot, now, maxAge int64) bool {
	for _, b := range books {
		if now-b.TsRecv > maxAge {
			return true
		}
	}
	return false
}

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
