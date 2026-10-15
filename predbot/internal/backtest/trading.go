package backtest

import (
	"os"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"predbot/internal/book"
	"predbot/internal/config"
	"predbot/internal/core"
	"predbot/internal/strategy"
)

// TradingOpts configures the trading replay (design §10.1): the cross-venue
// strategy, or the maker variant, driven through the SimExecutor.
type TradingOpts struct {
	Fees       Fees
	LatencyMS  int
	Strategy   strategy.Config
	MaxAge     int64                // ns; skip a pair if either book is older
	MaxSkew    int64                // ns; skip a pair if the books are further apart
	Resolution map[string]time.Time // canonical -> resolution date (§10.4)
	Maker      bool                 // rest one leg instead of taking both
	// RestWait cancels a maker rest that hasn't filled after this long, as the
	// live trader does (-rest-wait); a partial fill by then is hedged, and an
	// unfilled rest frees the market to be tried again. 0 = rests never expire.
	RestWait time.Duration
	Spread   bool // sample cross-venue spread every 5 min per market

	// Adverse, when set, is fed every paired book plus the trade prints up to
	// each snapshot's time, from Trades (in TsRecv order). The trade cursor is
	// global across markets, so adverse runs need the whole stream in one place.
	Adverse *AdverseSelection
	Trades  *TradeStream

	// Progress, when set, is incremented once per replayed snapshot (mapped or
	// not), so it counts toward the same total as the id-scan pass.
	Progress *atomic.Int64
}

// TradingResult is everything the trading replay produces. Report is not yet
// finalized; the caller runs Finalize with the resolution dates and hurdle.
type TradingResult struct {
	Report         *Report
	Opps           []Opportunity
	Spread         []SpreadPoint
	RestsPlaced    int
	RestsFilled    int // filled in full before any timeout
	RestsPartial   int // timed out part-filled; the filled part was hedged
	RestsCancelled int // timed out with nothing filled
}

// shard holds the replay state for a set of markets. Every field that changes
// per tick is keyed by canonical, so a shard never needs another market's state.
type shard struct {
	opts TradingOpts
	bm   *book.Manager
	sim  *Sim
	rep  *Report

	// These are hold-to-resolution arbs: entering a standing gap locks capital on
	// both venues until the market resolves (design §10.4), so you enter each
	// market's gap ONCE and cannot recycle that capital. Model that — enter the
	// first time a market shows edge, never re-enter (design §9: don't re-emit an
	// opportunity you're already working). Counting per-tick instead would treat
	// one persistent mispricing as thousands of independent trades.
	entered map[string]bool
	// active tracks whether a gap is currently open per market, so we log one
	// Opportunity per episode (gap opens) rather than per tick — "every trade we
	// could have made", distinct from the hold-to-resolution subset `entered`.
	active map[string]bool
	// Maker mode: track resting orders and whether the market ever crossed them.
	resting    map[string]*RestingFill
	restTaking map[string]core.Intent // the leg we'll take once the rest fills
	restAt     map[string]int64       // when each rest was placed (ns)
	// hedges waiting out the latency between a rest filling and the taking leg
	// landing; settled against the books as they are when it's due.
	hedges     map[string]*pendingHedge
	lastBucket map[string]int64 // 5-min spread sampling per market

	opps                                                   []Opportunity
	spread                                                 []SpreadPoint
	restsPlaced, restsFilled, restsPartial, restsCancelled int
}

type pendingHedge struct {
	rest, take core.Intent
	qty        core.Qty
	filledAt   int64 // ns: dates the pair
	due        int64 // ns: when the hedge lands
}

func newShard(markets []config.Market, opts TradingOpts) *shard {
	rep := NewReport()
	return &shard{
		opts:       opts,
		bm:         book.NewManager(markets),
		sim:        NewSim(opts.Fees, opts.LatencyMS, rep),
		rep:        rep,
		entered:    map[string]bool{},
		active:     map[string]bool{},
		resting:    map[string]*RestingFill{},
		restTaking: map[string]core.Intent{},
		restAt:     map[string]int64{},
		hedges:     map[string]*pendingHedge{},
		lastBucket: map[string]int64{},
	}
}

// tick advances the replay by one snapshot.
func (sh *shard) tick(s core.BookSnapshot) {
	o := &sh.opts
	now := s.TsRecv
	sh.sim.Advance(now, sh.bm) // settle fills whose t+Δ has arrived
	// Like Advance, these run before this snapshot's book update: a market's
	// book only changes on its own snapshots, so whichever tick first passes a
	// deadline sees the same book — and a shard's result doesn't depend on
	// which other markets share it.
	sh.settleHedges(now)
	sh.expireRests(now)
	canonical, ok := sh.bm.Update(s)
	if !ok {
		return // not in markets.yaml (§12)
	}
	sh.rep.seen(canonical, now)
	books, ready := sh.bm.Pair(canonical)
	if !ready || staleAny(books, now, o.MaxAge) || skewExceeds(books, o.MaxSkew) {
		return
	}
	if o.Spread {
		if bucket := (now / 1_000_000) / 300_000; sh.lastBucket[canonical] != bucket {
			if g, ok := CrossGrossCents(books); ok {
				sh.spread = append(sh.spread, SpreadPoint{
					TsMs: now / 1_000_000, Canonical: canonical,
					Category: Category(canonical), GrossEdgeCents: g})
			}
			sh.lastBucket[canonical] = bucket
		}
	}
	if o.Adverse != nil {
		o.Adverse.ObserveBook(canonical, s)
		// Feed any trades that happened up to this snapshot's time.
		for o.Trades != nil {
			t, more := o.Trades.Peek()
			if !more || t.TsRecv > s.TsRecv {
				break
			}
			if c, ok2 := sh.bm.Canonical(t.Venue, t.VenueID); ok2 {
				o.Adverse.ObserveTrade(c, t)
			}
			o.Trades.Next()
		}
	}

	// Advance any resting order for this market against the new book.
	if r, ok := sh.resting[canonical]; ok {
		if r.Observe(books[r.Intent.Venue]) > 0 && r.Complete() {
			sh.restsFilled++
			// The rest actually filled — hedge it: maker fee (a rebate on Poly)
			// on the rest, taker on the hedge, once the latency has passed.
			sh.hedge(canonical, r.Intent, sh.restTaking[canonical], r.Filled, now)
			sh.clearRest(canonical)
		}
	}

	exp := strategy.Exposure{InFlight: sh.entered}
	var legs []core.Intent
	if o.Maker {
		legs = strategy.MakerCrossVenue(canonical, books, exp, o.Fees, o.Strategy)
	} else {
		legs = strategy.CrossVenue(canonical, books, exp, o.Fees, o.Strategy)
	}
	switch {
	case len(legs) > 0:
		if !sh.active[canonical] { // new episode -> log an opportunity
			op := BuildOpportunity(legs[0], legs[1], o.Fees, now, o.Resolution[canonical])
			op.Canonical, op.Category = canonical, Category(canonical)
			sh.opps = append(sh.opps, op)
			sh.active[canonical] = true
		}
		if !sh.entered[canonical] {
			if ri := strategy.RestingLeg(legs); o.Maker && ri >= 0 {
				// The resting leg does NOT fill on submission — it fills only
				// if the market later crosses it. Track it and count honestly.
				if _, dup := sh.resting[canonical]; !dup {
					sh.resting[canonical] = NewRestingFill(legs[ri], books[legs[ri].Venue])
					sh.restTaking[canonical] = legs[1-ri] // the other leg is taken on fill
					sh.restAt[canonical] = now
					sh.restsPlaced++
				}
			} else {
				sh.sim.Submit(canonical, legs, now)
			}
			sh.entered[canonical] = true // hold to resolution; never re-enter
		}
	default:
		sh.active[canonical] = false // gap closed; a reopen is a new episode
	}
	for _, l := range strategy.IntraVenue(canonical, books, o.Fees, o.Strategy) {
		sh.sim.Submit(canonical, []core.Intent{l}, now)
	}
}

// hedge books the taking leg of a rest filled at time at: at once with no
// latency (the original model), else when the latency has passed.
func (sh *shard) hedge(canonical string, rest, take core.Intent, qty core.Qty, at int64) {
	lat := int64(sh.opts.LatencyMS) * int64(time.Millisecond)
	if lat == 0 {
		sh.sim.SettleMakerPair(canonical, rest, take, qty, at, sh.bm)
		return
	}
	sh.hedges[canonical] = &pendingHedge{rest: rest, take: take, qty: qty, filledAt: at, due: at + lat}
}

func (sh *shard) settleHedges(now int64) {
	for c, h := range sh.hedges {
		if now >= h.due {
			sh.sim.SettleMakerPair(c, h.rest, h.take, h.qty, h.filledAt, sh.bm)
			delete(sh.hedges, c)
		}
	}
}

// expireRests cancels rests older than RestWait, as the live trader does.
func (sh *shard) expireRests(now int64) {
	wait := int64(sh.opts.RestWait)
	if wait <= 0 {
		return
	}
	for c, r := range sh.resting {
		if now-sh.restAt[c] < wait {
			continue
		}
		if r.Filled > 0 {
			// Dated at the deadline, not at whichever tick noticed it: that tick
			// depends on which other markets share the shard.
			sh.restsPartial++
			sh.hedge(c, r.Intent, sh.restTaking[c], r.Filled, sh.restAt[c]+wait)
		} else {
			sh.restsCancelled++
			// Abandoned cleanly with no position: the market may be tried again.
			delete(sh.entered, c)
		}
		sh.clearRest(c)
	}
}

func (sh *shard) clearRest(c string) {
	delete(sh.resting, c)
	delete(sh.restTaking, c)
	delete(sh.restAt, c)
}

// finish settles anything still pending at end of replay. end is the last
// timestamp of the WHOLE replay: a shard's own data can stop earlier, and a
// rest whose deadline the serial run reached must expire here too, or the
// result would depend on the shard count.
func (sh *shard) finish(end int64) {
	sh.expireRests(end)
	for c, h := range sh.hedges {
		sh.sim.SettleMakerPair(c, h.rest, h.take, h.qty, h.filledAt, sh.bm)
		delete(sh.hedges, c)
	}
	sh.sim.Flush(sh.bm)
}

// RunTrading makes the trading pass over the recorded archive: every snapshot
// goes through the Book Manager, the strategy and the SimExecutor in TsRecv
// order. markets must include any sports markets reconstructed from the data.
//
// The sim, not decode, is what bounds a trading run, so it is sharded by
// market: each canonical market is pinned to one shard, which sees that
// market's snapshots in order and owns all of its state. Markets never touch
// each other's state (no shared capital gate; a pair settles against its own
// market's books), so the merged result matches a serial run.
func RunTrading(dir string, w Window, markets []config.Market, opts TradingOpts) (*TradingResult, error) {
	return runTrading(dir, w, markets, opts, simShards())
}

func runTrading(dir string, w Window, markets []config.Market, opts TradingOpts, n int) (*TradingResult, error) {
	if n < 1 {
		n = 1
	}
	if opts.Adverse != nil {
		n = 1 // the trade cursor is global across markets
	}
	var end int64 // last timestamp replayed, across every market
	if n == 1 {
		sh := newShard(markets, opts)
		err := StreamReplay(dir, w, func(s core.BookSnapshot) error {
			end = max(end, s.TsRecv)
			sh.tick(s)
			if opts.Progress != nil {
				opts.Progress.Add(1)
			}
			return nil
		})
		sh.finish(end)
		return mergeShards([]*shard{sh}), err
	}

	shards := make([]*shard, n)
	queues := make([]chan []core.BookSnapshot, n)
	var wg sync.WaitGroup
	for i := range shards {
		shards[i] = newShard(markets, opts)
		queues[i] = make(chan []core.BookSnapshot, shardQueue)
		wg.Add(1)
		go func(sh *shard, q <-chan []core.BookSnapshot) {
			defer wg.Done()
			for batch := range q {
				for _, s := range batch {
					sh.tick(s)
				}
			}
			sh.finish(end) // written by the router before it closed q
		}(shards[i], queues[i])
	}

	// Route read-only: this manager only maps venue ids to canonicals. Markets
	// are dealt to shards round-robin as they first appear; a day's games show
	// up together, so they land on different shards and run side by side.
	route := book.NewManager(markets)
	owner := map[string]int{}
	pending := make([][]core.BookSnapshot, n)
	err := StreamReplay(dir, w, func(s core.BookSnapshot) error {
		// Counted as routed, not as simulated: a shard can lag the router by
		// up to shardQueue batches, which is noise against a whole archive.
		if opts.Progress != nil {
			opts.Progress.Add(1)
		}
		end = max(end, s.TsRecv) // serial ticks advance on every snapshot, routed or not
		c, ok := route.Canonical(s.Venue, s.VenueID)
		if !ok {
			return nil // not in markets.yaml (§12); a shard would drop it too
		}
		i, seen := owner[c]
		if !seen {
			i = len(owner) % n
			owner[c] = i
		}
		pending[i] = append(pending[i], s)
		if len(pending[i]) == routeBatch {
			queues[i] <- pending[i]
			pending[i] = make([]core.BookSnapshot, 0, routeBatch)
		}
		return nil
	})
	for i, q := range queues {
		if len(pending[i]) > 0 {
			q <- pending[i]
		}
		close(q)
	}
	wg.Wait()
	return mergeShards(shards), err
}

const (
	// routeBatch is how many snapshots cross to a shard at once; per-snapshot
	// channel sends would cost more than the routing itself.
	routeBatch = 1024
	// shardQueue is how many batches a shard may lag the router before the
	// router blocks. Deep enough to ride out a burst on one market without
	// stalling the others; bounded so memory stays O(shards), not O(archive).
	shardQueue = 64
)

// TradingShards is how many sim shards RunTrading will use for opts.
func TradingShards(opts TradingOpts) int {
	if opts.Adverse != nil {
		return 1
	}
	return simShards()
}

// simShards is the trading replay's shard count: one per usable CPU, overridden
// by PREDBOT_SIM_SHARDS (1 = serial).
func simShards() int {
	n := runtime.GOMAXPROCS(0)
	if v, err := strconv.Atoi(os.Getenv("PREDBOT_SIM_SHARDS")); err == nil && v > 0 {
		n = v
	}
	return n
}

// mergeShards combines the shards' results. Opportunities and spread samples
// are ordered by time then market so exports don't depend on the shard count.
func mergeShards(shards []*shard) *TradingResult {
	res := &TradingResult{Report: NewReport()}
	for _, sh := range shards {
		res.Report.Merge(sh.rep)
		res.Opps = append(res.Opps, sh.opps...)
		res.Spread = append(res.Spread, sh.spread...)
		res.RestsPlaced += sh.restsPlaced
		res.RestsFilled += sh.restsFilled
		res.RestsPartial += sh.restsPartial
		res.RestsCancelled += sh.restsCancelled
	}
	sort.SliceStable(res.Opps, func(i, j int) bool {
		a, b := res.Opps[i], res.Opps[j]
		if a.TsMs != b.TsMs {
			return a.TsMs < b.TsMs
		}
		return a.Canonical < b.Canonical
	})
	sort.SliceStable(res.Spread, func(i, j int) bool {
		a, b := res.Spread[i], res.Spread[j]
		if a.TsMs != b.TsMs {
			return a.TsMs < b.TsMs
		}
		return a.Canonical < b.Canonical
	})
	return res
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
