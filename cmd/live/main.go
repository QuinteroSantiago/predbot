// Command live is Phase 4: REAL ORDERS WITH REAL MONEY (design §8, §11, §17).
//
// Safety design — read before running:
//   - It is a SEPARATE binary from the recorder so live trading can never be
//     switched on by accident.
//   - It refuses to run without --i-understand-this-places-real-orders.
//   - --dry-run (the default) places NOTHING: it runs the full pipeline and logs
//     the orders it WOULD place. Always run dry first.
//   - Startup reconciliation must succeed before any intent is accepted (§8).
//   - Every order passes risk.Engine.CheckAndReserve; a KILLED marker file
//     survives restarts and must be removed by a human.
//   - Fills come from the venues' private streams, never assumed.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"predbot/internal/backtest"
	"predbot/internal/book"
	"predbot/internal/config"
	"predbot/internal/core"
	"predbot/internal/exec"
	"predbot/internal/live"
	"predbot/internal/obs"
	"predbot/internal/risk"
	"predbot/internal/sports"
	"predbot/internal/strategy"
	"predbot/internal/venue"
	"predbot/internal/venue/kalshi"
	"predbot/internal/venue/polymarketus"
)

// build identifies the source state this binary was compiled from. Set at link
// time by scripts/trading.sh deploy-live via
//
//	-ldflags "-X main.build=<short-sha>"
//
// It defaults to "dev" for a hand-built binary. This exists so a journal line
// can name its own source: `start` runs whatever is already on the host, so
// without a stamp there is nothing tying a trade to the code that placed it.
var build = "dev"

func main() {
	cfgPath := flag.String("config", "config/config.yaml", "path to config.yaml")
	mktPath := flag.String("markets", "config/markets.yaml", "path to markets.yaml")
	dryRun := flag.Bool("dry-run", true, "log orders instead of placing them (default TRUE)")
	arm := flag.Bool("i-understand-this-places-real-orders", false, "required to place real orders")
	adoptPositions := flag.Bool("adopt-positions", false,
		"start with positions already on the venues, treating them as the bot's own (default: refuse)")
	adoptFile := flag.String("adopt-marker", "/var/lib/predbot/ADOPT",
		"one-shot adoption marker; present = adopt, and it is CONSUMED on read")
	maxOrderContracts := flag.Float64("max-order-contracts", 5, "hard cap: contracts per order")
	maxUnhedgedContracts := flag.Float64("max-unhedged-contracts", 10, "hard cap: total unhedged contracts")
	pnlFloor := flag.Float64("pnl-floor-usd", -25, "kill if realized P&L drops below this")
	minDepthContracts := flag.Float64("min-depth-contracts", 10, "require this much top-of-book depth on BOTH legs (prevents phantom edges against empty books)")
	sportsLeagues := flag.String("sports", "", "also trade cross-listed daily games, e.g. \"mlb\" (fast + liquid; more chances to fire)")
	flag.Parse()

	if !*dryRun && !*arm {
		fmt.Fprintln(os.Stderr, "REFUSING TO RUN: live mode requires --i-understand-this-places-real-orders")
		os.Exit(2)
	}

	rootCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	shutdown := obs.Setup(rootCtx)
	defer func() { _ = shutdown(context.Background()) }()

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
	met, err := obs.NewMetrics()
	if err != nil {
		slog.Error("metrics init failed", "err", err)
		os.Exit(1)
	}

	// --- venue plumbing -----------------------------------------------------
	kSign := func(m, p string) (http.Header, error) { return kalshi.AuthHeaders(m, "/trade-api/v2"+p) }
	pSign := func(m, p string) (http.Header, error) { return polymarketus.AuthHeaders(m, p) }

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
	// The PRIVATE order stream is a DIFFERENT endpoint from market data:
	// /v1/ws/private, not /v1/ws/markets. Passing the market-data dialer here
	// was the root cause of every naked leg through 2026-08-25 — the socket
	// connects and authenticates, accepts a SUBSCRIPTION_TYPE_ORDER subscribe,
	// and then simply never emits an execution, so the bot filled orders it
	// could not see. Verified 2026-08-25: on /v1/ws/private the same
	// subscription delivers EXECUTION_TYPE_FILL within ~1ms.
	const polyPrivatePath = "/v1/ws/private"
	polyPrivateURL := cfg.PolymarketUS.WSURL
	if u, err := url.Parse(cfg.PolymarketUS.WSURL); err == nil {
		u.Path = polyPrivatePath
		polyPrivateURL = u.String()
	}
	pPrivDial := venue.Dialer(func(ctx context.Context) (venue.Conn, error) {
		h, err := polymarketus.AuthHeaders("GET", polyPrivatePath)
		if err != nil {
			return nil, err
		}
		return venue.DialWS(polyPrivateURL, h)(ctx)
	})

	// --- risk engine: HARD CAPS --------------------------------------------
	killFile := "/var/lib/predbot/KILLED"

	// Read REAL buying power from both venues before constructing the engine.
	// Starting at zero would make every order fail the buying-power check — the
	// bot would run forever and never trade.
	kAcctBP := live.NewKalshiAccount("https://api.elections.kalshi.com/trade-api/v2", kSign)
	pAcctBP := live.NewPolyUSAccount("https://api.polymarket.us", pSign)
	bpCtx, bpCancel := context.WithTimeout(rootCtx, 30*time.Second)
	kBal, kErr := kAcctBP.BalanceUSD(bpCtx)
	pBal, pErr := pAcctBP.BalanceUSD(bpCtx)
	bpCancel()
	if kErr != nil || pErr != nil {
		slog.Error("could not read venue buying power — refusing to trade blind",
			"kalshi_err", kErr, "polymarketus_err", pErr)
		os.Exit(1)
	}
	slog.Info("venue buying power", "kalshi_usd", kBal, "polymarketus_usd", pBal)
	if kBal <= 0 || pBal <= 0 {
		slog.Error("a venue has no buying power — a two-leg arb needs funds on BOTH",
			"kalshi_usd", kBal, "polymarketus_usd", pBal)
		os.Exit(1)
	}

	engine := risk.New(risk.Config{
		MaxOrderQty:   core.Qty(*maxOrderContracts * 100),
		DefaultMaxPos: core.Qty(*maxOrderContracts * 100 * 4),
		// Keep a small reserve so we never spend a venue down to exactly zero.
		MinBP:       map[string]int64{"kalshi": 10000, "polymarketus": 10000}, // $1
		MaxUnhedged: core.Qty(*maxUnhedgedContracts * 100),
	}, map[string]int64{
		"kalshi":       int64(kBal * 1e4), // dollars -> $0.0001 units
		"polymarketus": int64(pBal * 1e4),
	}, killFile)
	if engine.Killed() {
		slog.Error("KILLED marker present — a human must remove it", "file", killFile)
		os.Exit(1)
	}

	// --- startup reconciliation (design §8) ---------------------------------
	kAcct := live.NewKalshiAccount("https://api.elections.kalshi.com/trade-api/v2", kSign)
	pAcct := live.NewPolyUSAccount("https://api.polymarket.us", pSign)
	sup := live.NewSupervisor([]live.AccountClient{kAcct, pAcct}, engine, nil)
	sup.PnLFloorUSD = *pnlFloor

	// One-shot adoption, mirroring the KILLED marker in reverse: a human puts the
	// file there deliberately, and it is CONSUMED here so it cannot survive into
	// the next start. Adoption is a decision about one specific set of positions
	// that a human has just looked at — it must never become a standing setting,
	// which is exactly what carrying -adopt-positions in the unit would make it.
	adopt := *adoptPositions
	if _, err := os.Stat(*adoptFile); err == nil {
		adopt = true
		if err := os.Remove(*adoptFile); err != nil {
			// If it cannot be consumed it would silently adopt on every restart.
			slog.Error("adoption marker could not be removed — refusing to adopt on a permission it cannot give back",
				"file", *adoptFile, "err", err)
			os.Exit(1)
		}
		slog.Warn("one-shot adoption marker consumed — this start may adopt; the next may not",
			"file", *adoptFile)
	}
	sup.AdoptExisting = adopt

	recCtx, recCancel := context.WithTimeout(rootCtx, 45*time.Second)
	if err := sup.Reconcile(recCtx); err != nil {
		recCancel()
		slog.Error("RECONCILIATION FAILED — refusing to trade", "err", err)
		os.Exit(1)
	}
	recCancel()

	// --- executors ----------------------------------------------------------
	var kEx, pEx exec.Executor = exec.NopExecutor{}, exec.NopExecutor{}
	if !*dryRun {
		kx := exec.NewKalshiExecutor("https://api.elections.kalshi.com/trade-api/v2", kSign, "")
		// Exchange-side backstop (design §8): a Kalshi order group whose rolling
		// 15-second contract limit cuts us off even if THIS process misbehaves.
		// Sized to the unhedged cap. Refuse to arm without it.
		ogCtx, ogCancel := context.WithTimeout(rootCtx, 15*time.Second)
		gid, err := kx.CreateOrderGroup(ogCtx, int64(*maxUnhedgedContracts))
		ogCancel()
		if err != nil {
			slog.Error("could not create Kalshi order group — refusing to arm without the exchange-side backstop", "err", err)
			os.Exit(1)
		}
		kx.SetOrderGroup(gid)
		slog.Info("exchange-side backstop armed", "order_group_id", gid,
			"contracts_limit_per_15s", int64(*maxUnhedgedContracts))
		kEx = kx
		pEx = exec.NewPolyUSExecutor("https://api.polymarket.us", pSign)
		slog.Warn("LIVE MODE ARMED — real orders will be placed",
			"build", build,
			"max_order_contracts", *maxOrderContracts,
			"max_unhedged_contracts", *maxUnhedgedContracts,
			"pnl_floor_usd", *pnlFloor)
	} else {
		slog.Info("DRY RUN — no orders will be placed (NopExecutor)")
	}

	bm := book.NewManager(markets)
	trader := live.NewTrader(live.Executors{"kalshi": kEx, "polymarketus": pEx}, engine, bm, 2*time.Second)

	// A fill stream can fail silently, so the venue's own position is the final
	// authority: the trader checks it when no fill arrives, and the drift monitor
	// polls it continuously and kills on anything unexplained.
	verify := func(vctx context.Context, venueName, venueID string) (float64, error) {
		var a live.AccountClient = kAcct
		if venueName == "polymarketus" {
			a = pAcct
		}
		pos, err := a.Positions(vctx)
		if err != nil {
			return 0, err
		}
		return pos[venueID], nil
	}
	trader.SetVerifier(verify)
	// Seed anything Reconcile was told to adopt: the drift monitor compares the
	// venues against the trader's ledger, so an adopted position it has never
	// heard of would read as drift and kill on the first tick.
	if adopt {
		adopted := sup.PreExisting()
		trader.Adopt(adopted)

		// Also tell the RISK ENGINE, which keys by canonical market while the
		// venues report by their own ids. Without this the position cap treats
		// adopted exposure as flat: harmless for a hedged pair (it nets to zero
		// either way) but wrong for a naked or partial leg, which is exactly the
		// kind of position worth adopting carefully.
		byCanonical := map[string]core.Qty{}
		canonicalOf := config.CanonicalByVenueID(markets)
		for venueID, contracts := range adopted {
			canonical, ok := canonicalOf[venueID]
			if !ok {
				slog.Warn("adopted a position in a market that is not in markets.yaml — the risk engine cannot cap it",
					"market", venueID, "contracts", contracts)
				continue
			}
			byCanonical[canonical] += core.Qty(math.Round(contracts * 100))
		}
		for canonical, net := range byCanonical {
			engine.AdoptPosition(canonical, net)
			slog.Warn("adopted position seeded into the risk engine",
				"canonical", canonical, "net_yes_contracts", net.String())
		}
	}
	go live.NewDriftMonitor([]live.AccountClient{kAcct, pAcct}, engine, trader.Positions).Run(rootCtx)

	// Live positions surface (Grafana): poll each venue's authoritative positions
	// on a slow ticker and publish them as predbot_position_contracts{venue,market}.
	// Polling happens in this goroutine, never in the metric callback, so venue
	// REST stays off the scrape path. On a read error we keep the last-known
	// values rather than blanking what is a safety view of real exposure.
	{
		canonicalOf := config.CanonicalByVenueID(markets)
		accts := map[string]live.AccountClient{"kalshi": kAcct, "polymarketus": pAcct}
		var posMu sync.Mutex
		posByVenue := map[string]map[string]float64{} // venue -> venueID -> contracts
		if err := obs.RegisterPositionsGauge(func() []obs.Position {
			posMu.Lock()
			defer posMu.Unlock()
			var out []obs.Position
			for v, held := range posByVenue {
				for venueID, qty := range held {
					if qty == 0 {
						continue
					}
					market := venueID
					if c, ok := canonicalOf[venueID]; ok {
						market = c
					}
					out = append(out, obs.Position{Venue: v, Market: market, Contracts: qty})
				}
			}
			return out
		}); err != nil {
			slog.Warn("positions gauge disabled", "err", err)
		}
		go func() {
			poll := func() {
				for venueName, a := range accts {
					cctx, cancel := context.WithTimeout(rootCtx, 15*time.Second)
					pos, err := a.Positions(cctx)
					cancel()
					if err != nil {
						slog.Warn("positions poll failed", "venue", venueName, "err", err)
						continue // keep last-known rather than blanking the safety view
					}
					posMu.Lock()
					posByVenue[venueName] = pos
					posMu.Unlock()
				}
			}
			poll()
			t := time.NewTicker(15 * time.Second)
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

	// --- private fill streams (fills are never assumed) ---------------------
	go live.NewKalshiFills(kDial).Run(rootCtx, trader.OnFill)
	// Poly needs an EXPLICIT slug list (an empty list subscribes to nothing), and
	// it must follow the dynamically discovered sports markets.
	go live.NewPolyUSFills(pPrivDial, func() []string {
		var out []string
		for _, c := range bm.Canonicals() {
			if id, ok := bm.VenueID(c, "polymarketus"); ok {
				out = append(out, id)
			}
		}
		return out
	}).Run(rootCtx, trader.OnFill)

	// --- market data --------------------------------------------------------
	snaps := make(chan core.BookSnapshot, 1024)
	go func() {
		_ = kalshi.New(kDial, venue.NopRecorder(), met).Stream(rootCtx, config.KalshiTickers(markets), snaps)
	}()
	go func() {
		_ = polymarketus.New(pDial, venue.NopRecorder(), met).Stream(rootCtx, config.PolyUSSlugs(markets), snaps)
	}()
	go func() {
		for {
			select {
			case <-rootCtx.Done():
				return
			case s := <-snaps:
				bm.Update(s)
			}
		}
	}()

	// --- dynamic sports subscription (fast, liquid daily games) -------------
	if leagues := splitCSV(*sportsLeagues); len(leagues) > 0 {
		go (&sports.Supervisor{Leagues: leagues, KDial: kDial, PDial: pDial,
			Rec: venue.NopRecorder(), Met: met, BM: bm, Out: snaps}).Run(rootCtx)
		slog.Info("sports trading enabled", "leagues", leagues)
	}

	// --- kill-trigger supervisor -------------------------------------------
	go sup.Run(rootCtx, trader.RealizedUSD, trader.HasUnhedged)

	// --- signals ------------------------------------------------------------
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGUSR1)
	go func() {
		for s := range sigCh {
			if s == syscall.SIGUSR1 { // manual kill switch (design §8)
				engine.Kill("manual SIGUSR1")
				slog.Error("KILL SWITCH ENGAGED via SIGUSR1")
				continue
			}
			slog.Info("shutdown", "signal", s.String())
			cancel()
			return
		}
	}()

	// --- trading loop -------------------------------------------------------
	fees := backtest.Fees{KalshiMult: cfg.Backtest.KalshiFeeMult}
	scfg := strategy.Config{
		MinEdge:  core.Price(cfg.Backtest.MinEdge),
		MaxClip:  core.Qty(*maxOrderContracts * 100),
		MinDepth: core.Qty(*minDepthContracts * 100),
	}
	maxAge := cfg.Backtest.MaxBookAgeMS * int64(time.Millisecond)
	maxSkew := cfg.Backtest.MaxSkewMS * int64(time.Millisecond)
	// `entered` enforces hold-to-resolution (never re-enter a market we are
	// already in). Sports games END, so their entries must age out or the set
	// grows without bound and a genuinely new game could be skipped.
	entered := map[string]time.Time{}
	const sportsEntryTTL = 12 * time.Hour

	slog.Info("live loop starting", "markets", len(markets), "dry_run", *dryRun)
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-rootCtx.Done():
			slog.Info("live loop stopped", "stats", fmt.Sprintf("%+v", trader.Stats()))
			return
		case <-tick.C:
			if engine.Killed() {
				continue
			}
			now := time.Now().UnixNano()
			// Age out finished sports entries.
			for c, at := range entered {
				if sports.Live(c) && time.Since(at) > sportsEntryTTL {
					delete(entered, c)
				}
			}
			for _, canonical := range bm.Canonicals() {
				if _, in := entered[canonical]; in {
					continue // hold to resolution; never re-enter
				}
				books, ok := bm.Pair(canonical)
				if !ok || staleAny(books, now, maxAge) || skewExceeds(books, maxSkew) {
					continue
				}
				inFlight := make(map[string]bool, len(entered))
				for c := range entered {
					inFlight[c] = true
				}
				legs := strategy.CrossVenue(canonical, books, strategy.Exposure{InFlight: inFlight}, fees, scfg)
				if len(legs) == 0 {
					continue
				}
				if *dryRun {
					slog.Info("DRY RUN would place pair", "canonical", canonical,
						"buy", fmt.Sprintf("%s %s@%s", legs[0].Venue, legs[0].Qty.String(), legs[0].Px.String()),
						"sell", fmt.Sprintf("%s %s@%s", legs[1].Venue, legs[1].Qty.String(), legs[1].Px.String()))
					entered[canonical] = time.Now()
					continue
				}
				if err := trader.ExecutePair(rootCtx, canonical, legs); err != nil {
					slog.Warn("pair failed", "canonical", canonical, "err", err)
					sup.NoteError()
					continue
				}
				entered[canonical] = time.Now()
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
	var lo, hi int64
	first := true
	for _, b := range books {
		if first || b.TsRecv < lo {
			lo = b.TsRecv
		}
		if first || b.TsRecv > hi {
			hi = b.TsRecv
		}
		first = false
	}
	return hi-lo > maxSkew
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
