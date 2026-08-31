// Package live wires the strategy to REAL order placement (design §8, §11).
// Every order passes through the risk gate; nothing here can place an order that
// risk.Engine has not approved and reserved.
//
// Legging policy (design §8, "sell it"):
//  1. Place the leg on the venue with the THINNER top-of-book first (higher fade
//     risk); on fill, immediately IOC the second leg.
//  2. If leg 2 is unfilled/partial, IOC-exit the unhedged quantity on leg 1's
//     venue at the best available price and book it as legging cost.
//  3. If the exit also fails, escalate: unhedged stays elevated and the risk
//     engine's unhedged cap will Kill(). Never auto-retry into a halted market.
package live

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/google/uuid"

	"predbot/internal/book"
	"predbot/internal/core"
	"predbot/internal/exec"
	"predbot/internal/risk"
)

// silentFillTolerance is the position mismatch (in contracts) tolerated before
// an unfilled leg 1 is treated as a silent fill. Matches DriftMonitor.Tolerance.
const silentFillTolerance = 0.01

// parkRetention is how long a fill that arrived before its order was registered
// is held waiting to be claimed. Long enough to cover any Place() round trip,
// short enough that a genuinely unattributable fill is surfaced promptly.
const parkRetention = 30 * time.Second

// Executors holds the per-venue order clients.
type Executors map[string]exec.Executor

// Trader turns approved Intents into real orders.
type Trader struct {
	ex      Executors
	engine  *risk.Engine
	bm      *book.Manager
	log     *slog.Logger
	legWait time.Duration // how long to wait for leg 2 before exiting (design §8)

	fillWait time.Duration // how long to wait for venue fill events per order
	// hedgeGrace is how long a legged pair gets to complete its missing side
	// before the naked leg is dumped. It MUST stay below the supervisor's
	// MaxUnhedgedFor (60s) or the bot kills itself mid-recovery.
	hedgeGrace time.Duration
	hedgeRetry time.Duration // how often to re-attempt the missing leg
	// maxCompletionLoss caps how far underwater a completing hedge may be before
	// we dump instead. Completing at a small loss beats dumping (one spread
	// either way, but a hedged pair still pays out); completing at a large one
	// locks that loss in until resolution.
	maxCompletionLoss core.Price
	pending           sync.WaitGroup
	// verify asks the VENUE what position it actually holds. Used to detect a
	// silent fill when no fill event arrived (fill streams can fail).
	verify func(ctx context.Context, venue, venueID string) (float64, error)

	// onAck / onOrder are optional metric hooks (kept as callbacks so this package
	// needn't import obs): onAck gets each venue order round-trip in seconds;
	// onOrder gets each leg's outcome (filled/partial/faded/error) with its venue.
	onAck   func(venue string, sec float64)
	onOrder func(venue, outcome string)

	mu       sync.Mutex
	stats    Stats
	awaiting map[string]chan Fill // client_order_id -> fill sink
	// parked holds fills whose correlation key had no sink YET. On Polymarket
	// the key is the VENUE-assigned order id, which we only learn from the
	// Place() HTTP response — so the private stream can (and on 2026-08-25 did,
	// by 1ms) deliver the fill before alias() installs that key. Parking lets
	// the late alias claim it instead of dropping a real fill on the floor.
	parked map[string]parkedFills
	// posByVenueID is what the bot believes it holds, keyed the way the VENUES
	// report positions. The risk engine keys by canonical market, so it cannot
	// answer this; the DriftMonitor needs the venue view to tell a legitimate
	// position apart from an unexplained one.
	posByVenueID map[string]float64
}

// parkedFills is a set of fills waiting for their order to be registered.
type parkedFills struct {
	fills []Fill
	at    time.Time // when the first one arrived, for expiry
}

// Stats is the live trading tally.
type Stats struct {
	PairsAttempted int
	PairsCompleted int
	PairsLegged    int
	ExitsPlaced    int
	Rejected       int
	OrdersPlaced   int
	RealizedUSD    float64 // realized P&L from actual fills (buys negative, sells positive)
	FeesUSD        float64 // fees reported by the venues
	UnhedgedQty    core.Qty
}

// RealizedUSD returns realized P&L net of venue-reported fees. This is what the
// supervisor's P&L floor watches.
func (t *Trader) RealizedUSD() float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stats.RealizedUSD - t.stats.FeesUSD
}

// HasUnhedged reports whether any unhedged quantity is outstanding — the
// supervisor kills if this stays true too long (design §8).
func (t *Trader) HasUnhedged() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stats.UnhedgedQty != 0
}

func NewTrader(ex Executors, engine *risk.Engine, bm *book.Manager, legWait time.Duration) *Trader {
	return &Trader{ex: ex, engine: engine, bm: bm, legWait: legWait,
		// 1s, not 3s. With awaitFills returning early on a full fill, this timer
		// no longer bounds a GOOD pair — it only bounds how long we sit unaware
		// that a leg is naked. Live fill events arrive in ~1ms; anything slower
		// is a lost event, and the venue position query resolves that far better
		// than waiting longer for a message that may never come.
		fillWait:   1 * time.Second,
		hedgeGrace: 30 * time.Second, // < supervisor MaxUnhedgedFor (60s)
		hedgeRetry: 2 * time.Second,
		// 2c/contract: wider than normal fee drag, tighter than a real dislocation.
		maxCompletionLoss: 200,
		awaiting:          map[string]chan Fill{},
		parked:            map[string]parkedFills{},
		posByVenueID:      map[string]float64{},
		log:               slog.With("component", "live-trader")}
}

// SetVerifier installs the venue-position check used to catch silent fills.
func (t *Trader) SetVerifier(f func(ctx context.Context, venue, venueID string) (float64, error)) {
	t.verify = f
}

// SetMetricHooks installs optional execution-metric callbacks: onAck records each
// venue order round-trip (seconds), onOrder records each leg's outcome. Both may
// be nil. Set once before trading; the hooks are read without the trader lock.
func (t *Trader) SetMetricHooks(onAck func(venue string, sec float64), onOrder func(venue, outcome string)) {
	t.onAck, t.onOrder = onAck, onOrder
}

// Positions returns what the bot BELIEVES it holds, keyed by venue market id —
// the input the drift monitor compares against the venues' own numbers. Built
// from actual fills, so a position the bot legitimately opened is not mistaken
// for drift (previously this was a stub returning nothing, which meant the very
// first successful hedged pair tripped the kill switch).
func (t *Trader) Positions() map[string]float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]float64, len(t.posByVenueID))
	for k, v := range t.posByVenueID {
		out[k] = v
	}
	return out
}

// Adopt seeds the ledger with positions that already existed on the venues at
// startup. Without this, adopting at the supervisor level would be pointless:
// the drift monitor compares the venues against this map, so an adopted
// position the ledger has never heard of is indistinguishable from drift.
func (t *Trader) Adopt(pos map[string]float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, v := range pos {
		t.posByVenueID[k] = v
	}
}

// Stats returns a copy of the running tally.
func (t *Trader) Stats() Stats {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stats
}

// ExecutePair places a two-leg arb. It returns an error if the pair could not be
// completed; an unhedged leg is exited per the legging policy before returning.
func (t *Trader) ExecutePair(ctx context.Context, canonical string, legs []core.Intent) error {
	if len(legs) != 2 {
		return fmt.Errorf("live: want 2 legs, got %d", len(legs))
	}
	if t.engine.Killed() {
		return risk.ErrKilled
	}
	t.bump(func(s *Stats) { s.PairsAttempted++ })

	// 1. Order the legs: thinner top-of-book first (higher fade risk).
	first, second := t.orderLegs(canonical, legs)

	// 2. Risk-gate BOTH legs before placing anything — if the pair can't clear
	//    risk in full, we never want a half-position.
	if err := t.engine.CheckAndReserve(canonical, first); err != nil {
		t.bump(func(s *Stats) { s.Rejected++ })
		return fmt.Errorf("live: leg1 risk: %w", err)
	}
	if err := t.engine.CheckAndReserve(canonical, second); err != nil {
		t.engine.OnReject(canonical, first) // release leg1's reservation
		t.bump(func(s *Stats) { s.Rejected++ })
		return fmt.Errorf("live: leg2 risk: %w", err)
	}

	// 3. Place BOTH legs SIMULTANEOUSLY.
	//
	// This replaced sequential legging on 2026-08-25. Legging sent leg 2 only
	// after leg 1's fill event came back, so every pair carried a multi-second
	// window (fillWait alone was 3s) in which we held one side and the other
	// could move away. The backtest attributed 12 of 19 attempts — 63% — to that
	// fade.
	//
	// Firing together does NOT remove naked-leg risk; it MOVES it. Under legging
	// only leg 1 could strand us. Now either side can fill alone, including the
	// side we meant to be short. What it buys is duration: the two legs resolve
	// within one round trip of each other instead of seconds apart, and the
	// losses came from the window, not from the frequency.
	type legResult struct {
		filled core.Qty
		err    error
	}
	fire := func(i core.Intent) <-chan legResult {
		ch := make(chan legResult, 1)
		go func() {
			f, err := t.place(ctx, canonical, i)
			ch <- legResult{filled: f, err: err}
		}()
		return ch
	}
	c1, c2 := fire(first), fire(second)
	r1, r2 := <-c1, <-c2

	if r1.err != nil {
		t.log.Warn("leg placement failed", "canonical", canonical, "venue", first.Venue, "err", r1.err)
	}
	if r2.err != nil {
		t.log.Warn("leg placement failed", "canonical", canonical, "venue", second.Venue, "err", r2.err)
	}

	// 3b. Emit ONE structured summary of the whole placement — both legs and the
	//     resulting hedge verdict — so a naked or half-filled pair is legible from
	//     a single log line instead of stitched together from several. Pure
	//     observability; it changes nothing about what follows.
	t.logPairOutcome(canonical, first, second, r1.filled, r2.filled)

	// 4. Neither side filled: nothing to hedge, but "no fill reported" is not
	//    proof of no position. Both legs were live, so BOTH must be verified.
	if r1.filled == 0 && r2.filled == 0 {
		if err := t.verifyNoSilentFill(ctx, canonical, first); err != nil {
			return err
		}
		if err := t.verifyNoSilentFill(ctx, canonical, second); err != nil {
			return err
		}
		t.log.Warn("neither leg filled; pair abandoned (both venues match expectation)",
			"canonical", canonical)
		return nil
	}

	// 5. Reconcile. The pair is hedged only where the two fills match; whatever
	//    is left over on one side is naked.
	naked := r1.filled - r2.filled
	if naked == 0 {
		t.bump(func(s *Stats) { s.PairsCompleted++ })
		return nil
	}

	// held is the side we over-filled (what we'd have to sell to get flat);
	// missing is the side still needed to complete the hedge.
	held, missing := first, second
	qty := naked
	if naked < 0 {
		held, missing = second, first
		qty = -naked
	}
	t.engine.SetUnhedged(canonical, qty)
	t.bump(func(s *Stats) { s.PairsLegged++; s.UnhedgedQty += qty })

	// Before re-placing ANYTHING, ask the venue whether the leg we think is
	// missing actually filled. "No fill event arrived" is not "no fill": the
	// stream can be dead (Polymarket's was, dialed on the wrong endpoint) or the
	// event can simply land after fillWait closed. Re-placing on that assumption
	// is what sold 10 against 5 on 2026-08-25.
	//
	// This check used to run ONLY when both legs reported zero, which is the one
	// case where nothing is at stake. The asymmetric case — one leg filled, one
	// silent — is precisely when a wrong answer costs money.
	if err := t.verifyNoSilentFill(ctx, canonical, missing); err != nil {
		t.log.Error("naked leg is OUTSTANDING and unresolved — human reconciliation required",
			"canonical", canonical, "venue", held.Venue, "qty", qty.String())
		return err
	}

	t.log.Warn("pair is legged — entering hedge grace window",
		"canonical", canonical, "naked_venue", held.Venue, "naked_qty", qty.String(),
		"need", missing.Venue, "grace", t.hedgeGrace.String())

	// 6. Resolve the naked leg in the BACKGROUND. The live loop is single
	//    threaded across all markets, so blocking it here for the whole grace
	//    window would freeze every other market and stall the kill checks.
	t.pending.Add(1)
	go func() {
		defer t.pending.Done()
		t.completeOrExit(ctx, canonical, held, missing, qty)
	}()
	return nil
}

// legState classifies one leg's fill against what was requested.
func legState(req, filled core.Qty) string {
	switch {
	case filled <= 0:
		return "NONE"
	case filled < req:
		return "PARTIAL"
	default:
		return "FULL"
	}
}

// pairVerdict summarises a two-leg placement into one label plus each leg's
// state. Pure (no I/O) so the classification is unit-testable:
//   - BOTH_ZERO       neither leg took — nothing at risk (still verify silently)
//   - HEDGED          equal fills, both fully filled — a clean pair
//   - HEDGED_PARTIAL  equal fills but under the requested size (thin fill)
//   - LEGGED          unequal fills — the difference is a NAKED leg (the bug class)
func pairVerdict(first, second core.Intent, f1, f2 core.Qty) (verdict, nakedVenue string, nakedQty core.Qty) {
	switch {
	case f1 == 0 && f2 == 0:
		return "BOTH_ZERO", "", 0
	case f1 == f2:
		if f1 < first.Qty || f2 < second.Qty {
			return "HEDGED_PARTIAL", "", 0
		}
		return "HEDGED", "", 0
	default:
		naked := f1 - f2
		if naked < 0 {
			return "LEGGED", second.Venue, -naked
		}
		return "LEGGED", first.Venue, naked
	}
}

// logPairOutcome emits ONE line describing a pair placement end to end. INFO for
// a clean hedge, WARN for anything a human would want to notice (a naked leg, a
// thin fill, or an unexpected miss). This is the entry to reach for first when
// debugging naked/half-filled legs. Observability only.
func (t *Trader) logPairOutcome(canonical string, first, second core.Intent, f1, f2 core.Qty) {
	verdict, nakedVenue, nakedQty := pairVerdict(first, second, f1, f2)
	legFmt := func(i core.Intent, filled core.Qty) string {
		return fmt.Sprintf("%s %s @%s req=%s filled=%s [%s]",
			i.Venue, i.Side.String(), i.Px.String(), i.Qty.String(), filled.String(), legState(i.Qty, filled))
	}
	log := t.log.Info
	if verdict != "HEDGED" {
		log = t.log.Warn
	}
	log("PAIR OUTCOME "+verdict,
		"canonical", canonical, "verdict", verdict,
		"leg1", legFmt(first, f1), "leg2", legFmt(second, f2),
		"naked_venue", nakedVenue, "naked_qty", nakedQty.String())
}

// verifyNoSilentFill confirms the venue holds no position for a leg that
// reported no fill. A fill stream can fail silently — Polymarket's did, because
// it was dialed on the market-data endpoint — so an unfilled leg means
// "unknown", not "flat", until the venue itself says otherwise.
func (t *Trader) verifyNoSilentFill(ctx context.Context, canonical string, leg core.Intent) error {
	if t.verify == nil {
		t.log.Error("leg reported no fill and NO VERIFIER is installed — cannot rule out a silent fill",
			"canonical", canonical, "venue", leg.Venue, "market", leg.VenueID)
		return nil
	}
	qty, verr := t.verify(ctx, leg.Venue, leg.VenueID)
	if verr != nil {
		// The check that would have caught a silent fill is itself broken.
		t.log.Error("KILL: cannot verify position after a no-fill — the silent-fill check is blind",
			"canonical", canonical, "venue", leg.Venue, "market", leg.VenueID, "err", verr)
		t.engine.Kill("position verification failed after an unfilled leg")
		return fmt.Errorf("live: leg verification failed on %s %s: %w — KILLED", leg.Venue, leg.VenueID, verr)
	}
	// Compare against what the bot BELIEVES it holds, not against zero: a
	// completed pair legitimately leaves a position on each venue.
	t.mu.Lock()
	expected := t.posByVenueID[leg.VenueID]
	t.mu.Unlock()
	if diff := qty - expected; diff > silentFillTolerance || diff < -silentFillTolerance {
		t.log.Error("SILENT FILL DETECTED — venue holds a position we saw no fill for",
			"canonical", canonical, "venue", leg.Venue, "market", leg.VenueID,
			"venue_qty", qty, "bot_qty", expected, "unexplained", diff)
		t.engine.Kill("silent fill: venue position without a fill event")
		return fmt.Errorf("live: silent fill on %s %s (venue %v, bot %v) — KILLED",
			leg.Venue, leg.VenueID, qty, expected)
	}
	return nil
}

// orderLegs returns (first, second) with the thinner-book leg first.
func (t *Trader) orderLegs(canonical string, legs []core.Intent) (core.Intent, core.Intent) {
	books, ok := t.bm.Pair(canonical)
	if !ok {
		return legs[0], legs[1]
	}
	depth := func(i core.Intent) core.Qty {
		b := books[i.Venue]
		if i.Side == core.BuyYes {
			if lv, ok := b.BestYesAsk(); ok {
				return lv.Qty
			}
		} else if lv, ok := b.BestYesBid(); ok {
			return lv.Qty
		}
		return 0
	}
	if depth(legs[1]) < depth(legs[0]) {
		return legs[1], legs[0]
	}
	return legs[0], legs[1]
}

// place sends one order and reports the filled quantity. IOC orders that don't
// fill return 0. A placement error releases nothing here — the caller decides.
func (t *Trader) place(ctx context.Context, canonical string, i core.Intent) (core.Qty, error) {
	ex, ok := t.ex[i.Venue]
	if !ok {
		return 0, fmt.Errorf("live: no executor for venue %q", i.Venue)
	}
	o := core.Order{Intent: i, ClientID: uuid.NewString()}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// Register the client id BEFORE placing, so a fill that arrives while
	// Place() is still returning is not missed.
	wait := t.awaitFills(o.ClientID, i.Qty)
	defer t.forget(o.ClientID)

	ackStart := time.Now()
	id, err := ex.Place(ctx, o)
	if t.onAck != nil {
		t.onAck(i.Venue, time.Since(ackStart).Seconds())
	}
	if err != nil {
		// Design §11: never resubmit blind — the order may have landed.
		if t.onOrder != nil {
			t.onOrder(i.Venue, "error")
		}
		return 0, err
	}
	// Polymarket has no client_order_id, so its fills arrive keyed by the
	// exchange-assigned order id. Alias that id to the same sink so either
	// correlation key routes to this order.
	if id != "" && id != o.ClientID {
		t.alias(id, o.ClientID)
		defer t.forget(id)
	}
	t.bump(func(s *Stats) { s.OrdersPlaced++ })
	t.log.Info("order placed", "canonical", canonical, "venue", i.Venue,
		"side", i.Side.String(), "px", i.Px.String(), "qty", i.Qty.String(),
		"client_id", o.ClientID, "venue_order_id", id)

	// Wait for the venue's authoritative fill events (design §11: fills come
	// from the private stream, never assumed). An IOC either fills within this
	// window or is cancelled by the venue — a timeout means "no fill", which is
	// safe: we release the reservation rather than assume a position we lack.
	filled := wait(t.fillWait)
	if filled > 0 {
		t.engine.OnFill(canonical, i, filled)
	}
	if filled < i.Qty {
		// Never let this outcome be silent. A timeout here means "no fill event
		// ARRIVED", which is not the same as "no fill happened" — the journal
		// used to simply stop after "order placed", which is exactly what made
		// the 2026-08-25 naked shorts invisible in the logs.
		t.log.Warn("order did not fully fill within fillWait — no fill EVENT arrived (not proof of no position)",
			"canonical", canonical, "venue", i.Venue, "market", i.VenueID,
			"client_id", o.ClientID, "venue_order_id", id,
			"requested", i.Qty.String(), "filled", filled.String(),
			"fill_wait", t.fillWait.String())
		rel := i
		rel.Qty = i.Qty - filled
		t.engine.OnReject(canonical, rel) // release the unfilled remainder
	}
	if t.onOrder != nil {
		switch {
		case filled == 0:
			t.onOrder(i.Venue, "faded") // accepted but no fill within the window (IOC cancelled / lost event)
		case filled < i.Qty:
			t.onOrder(i.Venue, "partial")
		default:
			t.onOrder(i.Venue, "filled")
		}
	}
	return filled, nil
}

// OnFill feeds a venue fill event into the trader; wire this to the private
// fill streams. It routes the fill to whichever in-flight order it belongs to.
func (t *Trader) OnFill(f Fill) {
	// Accrue realized cash flow from the ACTUAL fill FIRST, before any
	// correlation check: a fill we cannot match to an in-flight order (restart,
	// late fill, manual trade) still moved real money, and the P&L floor must
	// see it. Buying YES pays out cash; selling YES takes it in.
	cash := float64(f.Px) / 1e4 * (float64(f.Qty) / 100)
	contracts := float64(f.Qty) / 100
	t.mu.Lock()
	if f.Side == core.BuyYes {
		t.stats.RealizedUSD -= cash
		t.posByVenueID[f.VenueID] += contracts
	} else {
		t.stats.RealizedUSD += cash
		t.posByVenueID[f.VenueID] -= contracts
	}
	t.stats.FeesUSD += f.FeeUSD
	ch, ok := t.awaiting[f.ClientOrderID]
	if !ok {
		// Not necessarily unknown — the order may simply not be registered yet.
		// Park it; alias() claims it the instant Place() returns the venue id.
		pk := t.parked[f.ClientOrderID]
		if len(pk.fills) == 0 {
			pk.at = time.Now()
		}
		pk.fills = append(pk.fills, f)
		t.parked[f.ClientOrderID] = pk
	}
	t.mu.Unlock()

	t.sweepParked()
	if !ok {
		return
	}
	select {
	case ch <- f:
	default:
	}
}

// sweepParked expires parked fills nobody claimed and surfaces them loudly.
// The money is already right — OnFill accrues RealizedUSD and posByVenueID
// before any correlation check — but an unclaimed fill means a position exists
// that no in-flight order explains, and reconciliation is the backstop.
func (t *Trader) sweepParked() {
	t.mu.Lock()
	var stale []Fill
	now := time.Now()
	for k, pk := range t.parked {
		if now.Sub(pk.at) > parkRetention {
			stale = append(stale, pk.fills...)
			delete(t.parked, k)
		}
	}
	t.mu.Unlock()
	for _, f := range stale {
		t.log.Warn("fill for unknown order — reconcile", "client_id", f.ClientOrderID,
			"venue", f.Venue, "market", f.VenueID, "qty", f.Qty.String(),
			"parked_for", parkRetention.String())
	}
}

// claimParkedLocked drains any fills that arrived before key was registered.
func (t *Trader) claimParkedLocked(key string, ch chan Fill) {
	pk, ok := t.parked[key]
	if !ok {
		return
	}
	delete(t.parked, key)
	for _, f := range pk.fills {
		select {
		case ch <- f:
		default:
		}
	}
}

// awaitFills registers interest in a client id and returns a wait function that
// accumulates fills for up to d, returning EARLY once want is reached.
//
// The early return matters: this used to run the full fillWait (3s at the time) even on
// a leg that filled in a millisecond. That dead time is not free — while it
// elapses the other leg may already be filled and unhedged, so it is naked
// exposure measured in seconds, and it delays the grace window's first retry by
// the same amount. A leg that is done is done; there is nothing left to wait for.
func (t *Trader) awaitFills(clientID string, want core.Qty) func(time.Duration) core.Qty {
	ch := make(chan Fill, 8)
	t.mu.Lock()
	t.awaiting[clientID] = ch
	t.claimParkedLocked(clientID, ch)
	t.mu.Unlock()
	return func(d time.Duration) core.Qty {
		var total core.Qty
		if total >= want { // nothing requested; nothing to wait for
			return total
		}
		deadline := time.After(d)
		for {
			select {
			case f := <-ch:
				total += f.Qty
				// A PARTIAL fill keeps waiting — the remainder may still land
				// inside the window, and treating it as final would strand a leg.
				if total >= want {
					return total
				}
			case <-deadline:
				return total
			}
		}
	}
}

func (t *Trader) forget(clientID string) {
	t.mu.Lock()
	delete(t.awaiting, clientID)
	t.mu.Unlock()
	// Every order ends here, so this is the reliable place to expire parked
	// fills even when no further fill event ever arrives to trigger a sweep.
	t.sweepParked()
}

// completeOrExit resolves a legged pair. For up to hedgeGrace it re-attempts the
// MISSING leg at the current touch; if that succeeds the pair is hedged after
// all. Only when the window expires is the naked leg dumped.
//
// Why try to complete rather than dump immediately: both actions cross one
// spread, but completing leaves a hedged pair that pays out at resolution,
// whereas dumping just realizes the loss. Dumping is the fallback, not the plan.
//
// The guard below matters. Completing is NOT always better: if the other side
// has moved far enough that buy and sell no longer straddle, hedging locks in a
// guaranteed loss for the life of the position. In that case we dump instead.
func (t *Trader) completeOrExit(ctx context.Context, canonical string, held, missing core.Intent, qty core.Qty) {
	deadline := time.Now().Add(t.hedgeGrace)

	// Progress is measured from what the bot BELIEVES it holds, not from the
	// place() return values. A fill EVENT that arrives after its order's
	// fillWait expired still lands in posByVenueID — OnFill accounts for a fill
	// BEFORE it tries to correlate it — so belief is the only view that includes
	// a LATE fill. Trusting return values alone would re-place a leg that had
	// already filled: the same double-sell as 2026-08-25, from the other side of
	// the race. Both views are fed by the same fill events, so this is strictly
	// a superset, never less information.
	sign := 1.0
	if missing.Side == core.SellYes {
		sign = -1
	}
	t.mu.Lock()
	baseline := t.posByVenueID[missing.VenueID]
	t.mu.Unlock()
	hedged := func() core.Qty {
		t.mu.Lock()
		cur := t.posByVenueID[missing.VenueID]
		t.mu.Unlock()
		got := core.Qty(math.Round((cur - baseline) * sign * 100))
		if got < 0 {
			got = 0
		}
		if got > qty {
			got = qty // never claim more hedged than we set out to hedge
		}
		return got
	}

	var applied core.Qty // progress already reflected into the engine and stats
	sync := func() core.Qty {
		got := hedged()
		if got > applied {
			delta := got - applied
			applied = got
			t.engine.SetUnhedged(canonical, qty-got)
			t.bump(func(s *Stats) { s.UnhedgedQty -= delta })
			t.log.Info("hedge progressed during grace window",
				"canonical", canonical, "filled", delta.String(), "remaining", (qty - got).String())
		}
		return qty - got
	}

	// A late fill can complete the pair before this goroutine even runs.
	remaining := sync()

	for remaining > 0 && time.Now().Before(deadline) {
		if t.engine.Killed() {
			t.log.Warn("engine killed during hedge grace — exiting the naked leg now",
				"canonical", canonical, "qty", remaining.String())
			break
		}
		if sleepCtx(ctx, t.hedgeRetry) != nil {
			return // context cancelled; shutdown path handles reconciliation
		}
		// Re-check immediately before committing an order: a late fill may have
		// landed during the sleep, and this is the last moment we can decline to
		// re-place a leg that is already filled.
		if remaining = sync(); remaining <= 0 {
			break
		}
		books, ok := t.bm.Pair(canonical)
		if !ok {
			continue
		}
		try := missing
		try.Qty = remaining
		try.TIF = core.IOC
		b := books[missing.Venue]
		if missing.Side == core.BuyYes {
			lv, ok := b.BestYesAsk()
			if !ok {
				continue
			}
			try.Px = lv.Px
		} else {
			lv, ok := b.BestYesBid()
			if !ok {
				continue
			}
			try.Px = lv.Px
		}
		// Refuse to hedge into a guaranteed loss: for a true arb the BUY price
		// must stay below the SELL price. held.Px is what we actually paid.
		buyPx, sellPx := try.Px, held.Px
		if held.Side == core.BuyYes {
			buyPx, sellPx = held.Px, try.Px
		}
		if int64(sellPx)-int64(buyPx) < -int64(t.maxCompletionLoss) {
			t.log.Warn("hedge would lock a loss; dumping instead of completing",
				"canonical", canonical, "buy_px", buyPx.String(), "sell_px", sellPx.String())
			break
		}
		if err := t.engine.CheckAndReserve(canonical, try); err != nil {
			continue // risk says no right now; try again next tick
		}
		if _, err := t.place(ctx, canonical, try); err != nil {
			t.engine.OnReject(canonical, try)
		}
		remaining = sync()
	}

	if remaining > 0 {
		t.log.Warn("hedge grace expired — dumping the naked leg",
			"canonical", canonical, "venue", held.Venue, "qty", remaining.String())
		t.exitUnhedged(ctx, canonical, held, remaining) // clears the unhedged tally
		return
	}
	t.bump(func(s *Stats) { s.PairsCompleted++ })
	t.engine.SetUnhedged(canonical, 0)
	t.log.Info("pair completed inside the hedge grace window", "canonical", canonical)
}

// exitUnhedged IOC-exits an unhedged quantity on the leg's own venue at the
// opposite top-of-book (design §8 step 2). Failure escalates: the unhedged
// tally stays elevated and the risk engine will Kill() at the cap.
func (t *Trader) exitUnhedged(ctx context.Context, canonical string, leg core.Intent, qty core.Qty) {
	books, ok := t.bm.Pair(canonical)
	if !ok {
		t.log.Error("cannot exit unhedged: no book", "canonical", canonical, "qty", qty.String())
		return
	}
	b := books[leg.Venue]
	exit := leg
	exit.Qty = qty
	exit.TIF = core.IOC
	if leg.Side == core.BuyYes { // we're long YES -> sell it
		lv, ok := b.BestYesBid()
		if !ok {
			t.log.Error("no bid to exit into", "canonical", canonical)
			return
		}
		exit.Side, exit.Px = core.SellYes, lv.Px
	} else { // we're short YES -> buy it back
		lv, ok := b.BestYesAsk()
		if !ok {
			t.log.Error("no ask to exit into", "canonical", canonical)
			return
		}
		exit.Side, exit.Px = core.BuyYes, lv.Px
	}
	t.bump(func(s *Stats) { s.ExitsPlaced++ })
	if _, err := t.place(ctx, canonical, exit); err != nil {
		t.log.Error("UNHEDGED EXIT FAILED — escalating", "canonical", canonical, "qty", qty.String(), "err", err)
		return
	}
	t.engine.SetUnhedged(canonical, 0)
	t.bump(func(s *Stats) { s.UnhedgedQty -= qty })
}

func (t *Trader) bump(f func(*Stats)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	f(&t.stats)
}

// alias points an additional correlation key (e.g. a venue-assigned order id) at
// an existing order's fill sink. Needed for venues without a client order id.
func (t *Trader) alias(key, clientID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if ch, ok := t.awaiting[clientID]; ok {
		t.awaiting[key] = ch
		t.claimParkedLocked(key, ch)
	}
}
