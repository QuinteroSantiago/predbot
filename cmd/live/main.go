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
	"errors"
	"flag"
	"fmt"
	"log/slog"
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
	"tradekit/killswitch"
)

// build identifies the source state this binary was compiled from. Set at link
// time by scripts/trading.sh deploy-live via
//
//	-ldflags "-X main.build=<short-sha>+tradekit.<short-sha>"
//
// It defaults to "dev" for a hand-built binary. This exists so a journal line
// can name its own source: `start` runs whatever is already on the host, so
// without a stamp there is nothing tying a trade to the code that placed it.
var build = "dev"

// Startup and dashboard timings. Trading tuning is config.yaml's live: block.
const (
	startupBalanceTimeout   = 30 * time.Second // read both venues' buying power
	startupReconcileTimeout = 45 * time.Second // positions + open orders on both venues
	orderGroupTimeout       = 15 * time.Second // create the Kalshi exchange-side backstop
	positionsGaugeEvery     = 15 * time.Second // refresh the dashboard's per-venue positions
	positionsGaugeTimeout   = 15 * time.Second // one venue's positions read for that gauge
)

// kalshiShards are the Kalshi exchange shards the sports universe trades on:
// 0 (NFL, NHL, college football, ...) and 3 (baseball since 2026-08-24,
// basketball since 2026-09-10). Each gets its own order group, because a group
// only covers orders on its own shard. A ticker discovered on any other shard
// is refused at placement (exec.KalshiExecutor.groupFor) rather than traded
// without the backstop.
var kalshiShards = []int{0, 3}

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
	maker := flag.Bool("maker", false, "MAKER mode: rest one leg (earn the Poly rebate) and hedge the other on fill, instead of taker/taker (design §9.2)")
	restWait := flag.Duration("rest-wait", 30*time.Second, "maker: how long a resting leg sits on the book before it is cancelled")
	edgeStep := flag.Duration("edge-step", 0, "scale the min edge with time to game start: min_edge per started step (24h: 1c inside 24h, 2c inside 48h, ...); 0 = flat")
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
	kAcctBP := live.NewKalshiAccount(cfg.Kalshi.APIURL, kSign)
	pAcctBP := live.NewPolyUSAccount(cfg.PolymarketUS.APIURL, pSign)
	bpCtx, bpCancel := context.WithTimeout(rootCtx, startupBalanceTimeout)
	kBal, kErr := kAcctBP.BalanceUSD(bpCtx)
	pBal, pErr := pAcctBP.BalanceUSD(bpCtx)
	bpCancel()
	if kErr != nil || pErr != nil {
		slog.Error("could not read venue buying power — refusing to trade blind",
			"kalshi_err", kErr, "polymarketus_err", pErr)
		os.Exit(1)
	}
	slog.Info("venue buying power", "kalshi_usd", kBal, "polymarketus_usd", pBal)
	// The engine budgets Kalshi as one pot, but Kalshi funds are per shard: an
	// empty shard means its orders are rejected. Surface it at arm time.
	shardCtx, shardCancel := context.WithTimeout(rootCtx, startupBalanceTimeout)
	if byShard, err := kAcctBP.BalanceByShard(shardCtx); err != nil {
		slog.Warn("could not read Kalshi per-shard balance", "err", err)
	} else {
		for _, sh := range kalshiShards {
			if byShard[sh] <= 0 {
				slog.Warn("Kalshi shard has no funds — its markets will be rejected", "exchange_index", sh)
			}
		}
		slog.Info("kalshi balance by shard", "usd", byShard)
	}
	shardCancel()
	if kBal <= 0 || pBal <= 0 {
		slog.Error("a venue has no buying power — a two-leg arb needs funds on BOTH",
			"kalshi_usd", kBal, "polymarketus_usd", pBal)
		os.Exit(1)
	}

	engine := risk.New(risk.Config{
		MaxOrderQty:   core.QtyOf(*maxOrderContracts),
		DefaultMaxPos: core.QtyOf(*maxOrderContracts * 4),
		// Keep a small reserve so we never spend a venue down to exactly zero.
		MinBP:       map[string]int64{core.VenueKalshi: 10000, core.VenuePoly: 10000}, // $1
		MaxUnhedged: core.QtyOf(*maxUnhedgedContracts),
	}, map[string]int64{
		core.VenueKalshi: core.FromUSD(kBal),
		core.VenuePoly:   core.FromUSD(pBal),
	}, killFile)
	if engine.Killed() {
		slog.Error("KILLED marker present — a human must remove it", "file", killFile)
		os.Exit(1)
	}

	// --- startup reconciliation (design §8) ---------------------------------
	kAcct := live.NewKalshiAccount(cfg.Kalshi.APIURL, kSign)
	pAcct := live.NewPolyUSAccount(cfg.PolymarketUS.APIURL, pSign)
	sup := live.NewSupervisor([]live.AccountClient{kAcct, pAcct}, engine, nil)
	sup.Tune(cfg.Live)
	sup.PnLFloorUSD = *pnlFloor

	// One-shot adoption, mirroring the KILLED marker in reverse: a human puts the
	// file there deliberately, and it is CONSUMED here so it cannot survive into
	// the next start. Adoption is a decision about one specific set of positions
	// that a human has just looked at — it must never become a standing setting,
	// which is exactly what carrying -adopt-positions in the unit would make it.
	adopt := *adoptPositions
	if ok, err := killswitch.ConsumeMarker(*adoptFile); err != nil {
		// If it cannot be consumed it would silently adopt on every restart.
		slog.Error("adoption marker could not be removed — refusing to adopt on a permission it cannot give back",
			"file", *adoptFile, "err", err)
		os.Exit(1)
	} else if ok {
		adopt = true
		slog.Warn("one-shot adoption marker consumed — this start may adopt; the next may not",
			"file", *adoptFile)
	}
	sup.AdoptExisting = adopt

	recCtx, recCancel := context.WithTimeout(rootCtx, startupReconcileTimeout)
	if err := sup.Reconcile(recCtx); err != nil {
		recCancel()
		slog.Error("RECONCILIATION FAILED — refusing to trade", "err", err)
		os.Exit(1)
	}
	recCancel()

	// --- executors ----------------------------------------------------------
	var kEx, pEx exec.Executor = exec.NopExecutor{}, exec.NopExecutor{}
	var kalshiEx *exec.KalshiExecutor // nil in dry-run; fed ticker shards by discovery
	if !*dryRun {
		kx := exec.NewKalshiExecutor(cfg.Kalshi.APIURL, kSign, "")
		// Exchange-side backstop (design §8): a Kalshi order group whose rolling
		// 15-second contract limit cuts us off even if THIS process misbehaves.
		// Sized to the unhedged cap, one per shard. Refuse to arm without them.
		groups := make(map[int]string, len(kalshiShards))
		for _, sh := range kalshiShards {
			ogCtx, ogCancel := context.WithTimeout(rootCtx, orderGroupTimeout)
			gid, err := kx.CreateOrderGroup(ogCtx, int64(*maxUnhedgedContracts), sh)
			ogCancel()
			if err != nil {
				slog.Error("could not create Kalshi order group — refusing to arm without the exchange-side backstop",
					"exchange_index", sh, "err", err)
				os.Exit(1)
			}
			groups[sh] = gid
			slog.Info("exchange-side backstop armed", "exchange_index", sh, "order_group_id", gid,
				"contracts_limit_per_15s", int64(*maxUnhedgedContracts))
		}
		kx.SetOrderGroups(groups)
		kalshiEx = kx
		kEx = kx
		pEx = exec.NewPolyUSExecutor(cfg.PolymarketUS.APIURL, pSign)
		slog.Warn("LIVE MODE ARMED — real orders will be placed",
			"build", build,
			"max_order_contracts", *maxOrderContracts,
			"max_unhedged_contracts", *maxUnhedgedContracts,
			"pnl_floor_usd", *pnlFloor)
	} else {
		slog.Info("DRY RUN — no orders will be placed (NopExecutor)")
	}

	bm := book.NewManager(markets)
	trader := live.NewTrader(live.Executors{core.VenueKalshi: kEx, core.VenuePoly: pEx}, engine, bm)
	trader.Tune(cfg.Live)
	trader.SetRestWait(*restWait)
	// Execution-latency + order-outcome metrics from the REAL order path (populate
	// once armed): predbot_decision_to_ack_seconds{venue} and
	// predbot_orders_total{venue,outcome}. Same instruments the recorder feeds from
	// its paper sim, so the demo panels read live now and are exact when trading.
	trader.SetMetricHooks(
		func(v string, sec float64) { met.DecisionToAck.Record(rootCtx, sec, obs.Venue(v)) },
		func(v, outcome string) { met.Orders.Add(rootCtx, 1, obs.Outcome(v, outcome)) },
	)
	if err := obs.RegisterUptimeGauge(float64(time.Now().Unix())); err != nil {
		slog.Warn("uptime gauge disabled", "err", err)
	}

	// Maintenance breakers (Polymarket: weekly Thu 2–6 AM ET + unannounced
	// emergency windows; both venues answer 5xx while down). After 3 consecutive
	// 5xx a venue is DOWN: the periodic pollers below go through a GuardedAccount
	// and drop to one probe per 15 min instead of warning every tick, and the
	// trading loop opens no new pair while either venue is down.
	kOut, pOut := live.NewOutage(core.VenueKalshi), live.NewOutage(core.VenuePoly)
	kGuard, pGuard := live.Guard(kAcct, kOut), live.Guard(pAcct, pOut)

	// A fill stream can fail silently, so the venue's own position is the final
	// authority: the trader checks it when no fill arrives, and the drift monitor
	// polls it continuously and kills on anything unexplained. The verifier is
	// NEVER skipped by the outage breaker (an unverifiable no-fill must still
	// kill); it only reports into it, so a successful read ends an outage early.
	verify := func(vctx context.Context, venueName, venueID string) (float64, error) {
		var a live.AccountClient = kAcct
		out := kOut
		if venueName == core.VenuePoly {
			a, out = pAcct, pOut
		}
		pos, err := a.Positions(vctx)
		out.Observe(err)
		if err != nil {
			return 0, err
		}
		return pos[venueID], nil
	}
	trader.SetVerifier(verify)
	trader.SetOrderChecker(func(octx context.Context, venueName, orderID string) (bool, error) {
		var a live.AccountClient = kAcct
		if venueName == core.VenuePoly {
			a = pAcct
		}
		ids, err := a.OpenOrders(octx)
		if err != nil {
			return true, err
		}
		for _, id := range ids {
			if id == orderID {
				return true, nil
			}
		}
		return false, nil
	})
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
			byCanonical[canonical] += core.QtyOf(contracts)
		}
		for canonical, net := range byCanonical {
			engine.AdoptPosition(canonical, net)
			slog.Warn("adopted position seeded into the risk engine",
				"canonical", canonical, "net_yes_contracts", net.String())
		}
	}
	drift := live.NewDriftMonitor([]live.AccountClient{kGuard, pGuard}, engine, trader.Positions)
	drift.Skip = trader.Resting
	go drift.Run(rootCtx)

	// Publish live unhedged (naked-leg) exposure per market to the dashboard.
	if err := obs.RegisterUnhedgedGauge(engine.UnhedgedSnapshot); err != nil {
		slog.Warn("unhedged gauge disabled", "err", err)
	}

	// Live positions surface (Grafana): poll each venue's authoritative positions
	// on a slow ticker and publish them as predbot_position_contracts{venue,market}.
	// Polling happens in this goroutine, never in the metric callback, so venue
	// REST stays off the scrape path. On a read error we keep the last-known
	// values rather than blanking what is a safety view of real exposure.
	{
		canonicalOf := config.CanonicalByVenueID(markets)
		accts := map[string]live.AccountClient{core.VenueKalshi: kGuard, core.VenuePoly: pGuard}
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
					cctx, cancel := context.WithTimeout(rootCtx, positionsGaugeTimeout)
					pos, err := a.Positions(cctx)
					cancel()
					if errors.Is(err, live.ErrVenueUnavailable) {
						continue // down for maintenance; keep last-known, the breaker logged it
					}
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
			t := time.NewTicker(positionsGaugeEvery)
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
	polyFills := live.NewPolyUSFills(pPrivDial, func() []string {
		var out []string
		for _, c := range bm.Canonicals() {
			if id, ok := bm.VenueID(c, core.VenuePoly); ok {
				out = append(out, id)
			}
		}
		return out
	})
	go polyFills.Run(rootCtx, trader.OnFill)

	// --- market data --------------------------------------------------------
	// Per-venue drains: each venue's snapshots feed an independent goroutine, so
	// a burst from one venue (Kalshi stamps many markets at the same ms) can't
	// stall book updates for the other. bm is RWMutex-safe, so both drains may
	// call Update concurrently. Mirrors the recorder's de-serialized drain.
	kalshiSnaps := make(chan core.BookSnapshot, 1024)
	polySnaps := make(chan core.BookSnapshot, 1024)
	go func() {
		_ = kalshi.New(kDial, venue.NopRecorder(), met).Stream(rootCtx, config.KalshiTickers(markets), kalshiSnaps)
	}()
	go func() {
		_ = polymarketus.New(pDial, venue.NopRecorder(), met).Stream(rootCtx, config.PolyUSSlugs(markets), polySnaps)
	}()
	drain := func(in <-chan core.BookSnapshot) {
		for {
			select {
			case <-rootCtx.Done():
				return
			case s := <-in:
				bm.Update(s)
			}
		}
	}
	go drain(kalshiSnaps)
	go drain(polySnaps)

	// --- dynamic sports subscription (fast, liquid daily games) -------------
	var sportsSup *sports.Supervisor // nil without -sports
	if leagues := splitCSV(*sportsLeagues); len(leagues) > 0 {
		// The supervisor emits both venues into one channel; demux by venue into
		// the same per-venue drains so sports markets get the same isolation.
		sportsSnaps := make(chan core.BookSnapshot, 1024)
		go func() {
			for {
				select {
				case <-rootCtx.Done():
					return
				case s := <-sportsSnaps:
					dst := kalshiSnaps
					if s.Venue == core.VenuePoly {
						dst = polySnaps
					}
					select {
					case dst <- s:
					case <-rootCtx.Done():
						return
					}
				}
			}
		}()
		sportsSup = &sports.Supervisor{Leagues: leagues, KDial: kDial, PDial: pDial,
			Rec: venue.NopRecorder(), Met: met, BM: bm, Out: sportsSnaps}
		if kalshiEx != nil {
			sportsSup.OnKalshiShards = kalshiEx.SetShards
		}
		go sportsSup.Run(rootCtx)
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
	fees := backtest.Fees{KalshiMult: cfg.Strategy.KalshiFeeMult}
	scfg := strategy.Config{
		MinEdge:  core.Price(cfg.Strategy.MinEdge),
		MaxClip:  core.QtyOf(*maxOrderContracts),
		MinDepth: core.QtyOf(*minDepthContracts),
	}
	maxAge := cfg.Strategy.MaxBookAgeMS * int64(time.Millisecond)
	maxSkew := cfg.Strategy.MaxSkewMS * int64(time.Millisecond)
	// `entered` enforces hold-to-resolution (never re-enter a market we are
	// already in). Sports games END, so their entries must age out or the set
	// grows without bound and a genuinely new game could be skipped.
	entered := map[string]time.Time{}

	slog.Info("live loop starting", "markets", len(markets), "dry_run", *dryRun,
		"mode", modeName(*maker), "rest_wait", restWait.String(), "edge_step", edgeStep.String())
	l := cfg.Live
	slog.Info("live tuning", "fill_wait", l.FillWait.String(), "hedge_grace", l.HedgeGrace.String(),
		"hedge_retry", l.HedgeRetry.String(), "hedge_slip", core.Price(l.HedgeSlip).String(),
		"max_completion_loss", core.Price(l.MaxCompletionLoss).String(), "max_unhedged_for", l.MaxUnhedgedFor.String(),
		"max_errors", l.MaxErrors, "error_window", l.ErrorWindow.String(), "slow_ack", l.SlowAck.String(),
		"slow_ack_pause", l.SlowAckPause.String(), "sports_entry_ttl", l.SportsEntryTTL.String(),
		"loop_tick", l.LoopTick.String(), "min_edge", core.Price(cfg.Strategy.MinEdge).String())
	tick := time.NewTicker(cfg.Live.LoopTick)
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
				if sports.Live(c) && time.Since(at) > cfg.Live.SportsEntryTTL {
					delete(entered, c)
				}
			}
			// A venue in maintenance can't place, fill-report or reconcile an
			// order, so open nothing until its outage breaker sees it back up.
			if kOut.Down() || pOut.Down() {
				continue
			}
			// A pair's second leg lands only as fast as its venue's ack; while
			// either venue is acking slowly the gap closes before the hedge does.
			if trader.VenueSlow(core.VenueKalshi) || trader.VenueSlow(core.VenuePoly) {
				continue
			}
			for _, canonical := range bm.Canonicals() {
				if _, in := entered[canonical]; in {
					continue // hold to resolution; never re-enter
				}
				// Sports pairs open only before the game starts; see PreGame.
				if sports.Live(canonical) && (sportsSup == nil || !sportsSup.PreGame(canonical, time.Now())) {
					continue
				}
				books, ok := bm.Pair(canonical)
				if !ok || staleAny(books, now, maxAge) || skewExceeds(books, maxSkew) {
					continue
				}
				inFlight := make(map[string]bool, len(entered))
				for c := range entered {
					inFlight[c] = true
				}
				gcfg := scfg
				if st, ok := startTime(sportsSup, canonical); ok {
					gcfg.MinEdge = strategy.HorizonEdge(scfg.MinEdge, time.Until(st), *edgeStep)
				}
				var legs []core.Intent
				if *maker {
					legs = strategy.MakerCrossVenue(canonical, books, strategy.Exposure{InFlight: inFlight}, fees, gcfg)
				} else {
					legs = strategy.CrossVenue(canonical, books, strategy.Exposure{InFlight: inFlight}, fees, gcfg)
				}
				if len(legs) == 0 {
					continue
				}
				// Maker validation: only ever REST the Polymarket leg. The edge is
				// the Poly maker rebate (adverse drift 0.287c < rebate 0.31c); a
				// Kalshi rest earns no rebate AND fails to attach to the Kalshi
				// exchange-side order group (order_group_not_found). Skip cleanly
				// (no entry mark, no error) when the strategy's cheaper rest is Kalshi.
				if *maker {
					ri := strategy.RestingLeg(legs)
					if ri < 0 || legs[ri].Venue != core.VenuePoly {
						continue
					}
					// A rest priced through its own book fills at once as a taker
					// (2026-09-24, CHW). The strategy no longer does this and Poly
					// rejects it (post-only); this is the third lock.
					if strategy.RestCrosses(legs[ri], books[legs[ri].Venue]) {
						slog.Error("skip: resting leg would cross its own book", "canonical", canonical,
							"side", legs[ri].Side.String(), "px", legs[ri].Px.String())
						continue
					}
				}
				// Silent-fill guard (2026-08-25/-26 naked-leg incidents): never
				// act on a Polymarket slug the private fill stream is not yet
				// subscribed to — the order would fill with no execution event and
				// leave a naked leg. A dynamically discovered sports market reaches
				// the book (and this loop) before the fill stream's periodic
				// re-subscribe covers it; skip it and poke an immediate
				// re-subscribe so a later tick can trade it safely.
				if slug, ok := bm.VenueID(canonical, core.VenuePoly); !ok || !polyFills.Covers(slug) {
					polyFills.Resubscribe()
					slog.Warn("skip: Poly fill stream not yet subscribed to slug — deferring to avoid a silent fill",
						"canonical", canonical, "slug", slug)
					continue
				}
				if *dryRun {
					legFmt := func(l core.Intent) string {
						tif := "IOC"
						if l.TIF == core.GTC || l.TIF == core.GTD {
							tif = "REST"
						}
						return fmt.Sprintf("%s %s %s@%s [%s]", l.Venue, l.Side.String(), l.Qty.String(), l.Px.String(), tif)
					}
					slog.Info("DRY RUN would place pair", "canonical", canonical, "mode", modeName(*maker),
						"leg1", legFmt(legs[0]), "leg2", legFmt(legs[1]))
					entered[canonical] = holdFrom(sportsSup, canonical)
					continue
				}
				exec := trader.ExecutePair
				if *maker {
					exec = trader.ExecuteMakerPair
				}
				if err := exec(rootCtx, canonical, legs); err != nil {
					slog.Warn("pair failed", "canonical", canonical, "err", err)
					// Our own risk engine refusing for buying power is not a venue
					// error: the loop retries every tick, so one unaffordable game
					// tripped the error-storm kill in ~5s (2026-09-26).
					if !errors.Is(err, risk.ErrBuyingPower) {
						sup.NoteError()
					}
					continue
				}
				entered[canonical] = holdFrom(sportsSup, canonical)
			}
		}
	}
}

func modeName(maker bool) string {
	if maker {
		return "maker"
	}
	return "taker"
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

// holdFrom is when an entry's hold-to-resolution TTL starts counting: now, or
// the game's scheduled start if later. Games are traded days ahead, so aging
// from entry alone let a game entered 2 days out be re-entered 12h later.
func holdFrom(sup *sports.Supervisor, canonical string) time.Time {
	now := time.Now()
	if st, ok := startTime(sup, canonical); ok && st.After(now) {
		return st
	}
	return now
}

// startTime is a sports game's scheduled start, if the supervisor knows it.
func startTime(sup *sports.Supervisor, canonical string) (time.Time, bool) {
	if sup == nil {
		return time.Time{}, false
	}
	return sup.StartTime(canonical)
}
