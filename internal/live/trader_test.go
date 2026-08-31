package live

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"predbot/internal/book"
	"predbot/internal/config"
	"predbot/internal/core"
	"predbot/internal/exec"
	"predbot/internal/risk"
)

// fakeExec records placed orders and can be made to fail on a chosen venue.
type fakeExec struct {
	name    string
	mu      sync.Mutex
	got     []core.Order
	fail    bool
	onPlace func()  // called on entry, to observe placement timing
	tr      *Trader // emits fills back, like the venue's private fill stream
	// placeFn, when set, replaces the default "return \"oid\" and emit a fill
	// keyed by ClientID" behaviour. Needed to reproduce venue-specific
	// correlation: Polymarket keys fills by the VENUE-assigned order id, which
	// the trader only learns from this call's return value.
	placeFn func(core.Order) (string, error)
}

// setFail flips the reject switch under the lock: the hedge grace window runs on
// its own goroutine, so a test toggling this races the trader without it.
func (f *fakeExec) setFail(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = v
}

func (f *fakeExec) Name() string { return f.name }
func (f *fakeExec) Place(_ context.Context, o core.Order) (string, error) {
	f.mu.Lock()
	if f.onPlace != nil {
		f.onPlace()
	}
	fail, tr, placeFn := f.fail, f.tr, f.placeFn
	if !fail {
		f.got = append(f.got, o)
	}
	f.mu.Unlock()

	if fail {
		return "", errors.New("venue rejected")
	}
	if placeFn != nil {
		return placeFn(o)
	}
	if tr != nil { // simulate the venue reporting the fill
		go tr.OnFill(Fill{Venue: f.name, VenueID: o.VenueID, ClientOrderID: o.ClientID,
			Side: o.Side, Px: o.Px, Qty: o.Qty})
	}
	return "oid", nil
}
func (f *fakeExec) Cancel(context.Context, string) error { return nil }
func (f *fakeExec) orders() []core.Order {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]core.Order(nil), f.got...)
}

func setup(t *testing.T) (*Trader, *fakeExec, *fakeExec, *risk.Engine) {
	t.Helper()
	var m config.Market
	m.Canonical = "MKT"
	m.Venues.Kalshi.Ticker = "K"
	m.Venues.PolymarketUS.Slug = "P"
	bm := book.NewManager([]config.Market{m})
	bm.Update(core.BookSnapshot{Venue: "kalshi", VenueID: "K",
		YesAsks: []core.Level{{Px: 5000, Qty: 10000}}, YesBids: []core.Level{{Px: 4900, Qty: 10000}}})
	bm.Update(core.BookSnapshot{Venue: "polymarketus", VenueID: "P",
		YesBids: []core.Level{{Px: 6000, Qty: 500}}, YesAsks: []core.Level{{Px: 6100, Qty: 500}}})

	eng := risk.New(risk.Config{
		MaxOrderQty: 100000, DefaultMaxPos: 100000,
		MinBP: map[string]int64{"kalshi": 0, "polymarketus": 0}, MaxUnhedged: 100000,
	}, map[string]int64{"kalshi": 1e9, "polymarketus": 1e9}, "")

	k := &fakeExec{name: "kalshi"}
	p := &fakeExec{name: "polymarketus"}
	tr := NewTrader(Executors{"kalshi": k, "polymarketus": p}, eng, bm, time.Second)
	tr.fillWait = 200 * time.Millisecond
	tr.hedgeGrace = 300 * time.Millisecond
	tr.hedgeRetry = 20 * time.Millisecond
	k.tr, p.tr = tr, tr
	return tr, k, p, eng
}

func pair() []core.Intent {
	return []core.Intent{
		{Venue: "kalshi", VenueID: "K", Side: core.BuyYes, Px: 5000, Qty: 500, TIF: core.IOC},
		{Venue: "polymarketus", VenueID: "P", Side: core.SellYes, Px: 6000, Qty: 500, TIF: core.IOC},
	}
}

// Both legs fill -> pair completed, one order per venue.
func TestExecutePairBothLegsFill(t *testing.T) {
	tr, k, p, _ := setup(t)
	if err := tr.ExecutePair(context.Background(), "MKT", pair()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(k.orders()) != 1 || len(p.orders()) != 1 {
		t.Fatalf("want 1 order per venue, got kalshi=%d poly=%d", len(k.orders()), len(p.orders()))
	}
	if s := tr.Stats(); s.PairsCompleted != 1 {
		t.Errorf("completed = %d; want 1", s.PairsCompleted)
	}
}

// The DEEPER book anchors first: Kalshi 10000 vs Poly 500 (design §8, flipped
// 2026-08-31 — the deep, reliably-fillable leg is the anchor).
func TestDeeperLegGoesFirst(t *testing.T) {
	tr, _, _, _ := setup(t)
	first, second := tr.orderLegs("MKT", pair())
	if first.Venue != "kalshi" || second.Venue != "polymarketus" {
		t.Errorf("deep-first ordering wrong: first=%s second=%s", first.Venue, second.Venue)
	}
}

// crossPrice must cross the touch by slip in the aggressive direction and stay
// inside the open (0, OneDollar) range so it is always a placeable price.
func TestCrossPriceCrossesTouch(t *testing.T) {
	if got := crossPrice(core.BuyYes, 6000, 200); got != 6200 {
		t.Errorf("buy cross: got %d want 6200 (pay up to fill)", got)
	}
	if got := crossPrice(core.SellYes, 6000, 200); got != 5800 {
		t.Errorf("sell cross: got %d want 5800 (accept down to fill)", got)
	}
	// Clamp to the valid open range rather than emit 0 or >=OneDollar.
	if got := crossPrice(core.BuyYes, core.OneDollar-50, 200); got != core.OneDollar-1 {
		t.Errorf("buy clamp: got %d want %d", got, core.OneDollar-1)
	}
	if got := crossPrice(core.SellYes, 100, 200); got != 1 {
		t.Errorf("sell clamp: got %d want 1", got)
	}
}

// Leg 2 fails -> the unhedged leg-1 quantity must be exited on leg 1's venue.
func TestLeggedExitOnLeg2Failure(t *testing.T) {
	tr, k, p, _ := setup(t)
	// Kalshi (the deep anchor leg) fails; Poly fills and is the naked leg to exit.
	k.fail = true
	_ = tr.ExecutePair(context.Background(), "MKT", pair())
	tr.pending.Wait() // the naked leg is now resolved in the background
	s := tr.Stats()
	if s.PairsLegged != 1 {
		t.Errorf("legged = %d; want 1", s.PairsLegged)
	}
	if s.ExitsPlaced != 1 {
		t.Errorf("exits = %d; want 1 (must exit the unhedged leg)", s.ExitsPlaced)
	}
	// The exit must be on leg 1's venue (poly) and OPPOSITE the original side.
	po := p.orders()
	if len(po) != 2 {
		t.Fatalf("poly orders = %d; want 2 (entry + exit)", len(po))
	}
	if po[0].Side == po[1].Side {
		t.Error("exit must be the opposite side of the entry")
	}
	// The exit must CROSS the touch (buy back a short at bestAsk 6100 + 2c slip),
	// not rest exactly on it — that resting price is what left legs naked before.
	if po[1].Side != core.BuyYes || po[1].Px != 6300 {
		t.Errorf("exit should cross: got %s @ %d; want BuyYes @ 6300 (6100 ask + 200 slip)", po[1].Side, po[1].Px)
	}
}

// A killed engine must place nothing at all.
func TestKilledEngineBlocksExecution(t *testing.T) {
	tr, k, p, eng := setup(t)
	eng.Kill("test")
	if err := tr.ExecutePair(context.Background(), "MKT", pair()); err != risk.ErrKilled {
		t.Errorf("err = %v; want ErrKilled", err)
	}
	if len(k.orders())+len(p.orders()) != 0 {
		t.Error("killed engine must place NO orders")
	}
}

// If risk rejects leg 2, leg 1 must never be placed (no half-position).
func TestRiskRejectionPlacesNothing(t *testing.T) {
	var m config.Market
	m.Canonical = "MKT"
	m.Venues.Kalshi.Ticker = "K"
	m.Venues.PolymarketUS.Slug = "P"
	bm := book.NewManager([]config.Market{m})
	// Note: a hedged pair nets to ~zero position, so a position cap does NOT
	// reject it (correctly). Reject leg 2 via buying power instead: Poly has none.
	eng := risk.New(risk.Config{MaxOrderQty: 100000, DefaultMaxPos: 100000,
		MinBP: map[string]int64{}, MaxUnhedged: 100000},
		map[string]int64{"kalshi": 1e9, "polymarketus": 1}, "")
	k := &fakeExec{name: "kalshi"}
	p := &fakeExec{name: "polymarketus"}
	tr := NewTrader(Executors{"kalshi": k, "polymarketus": p}, eng, bm, time.Second)
	tr.fillWait = 200 * time.Millisecond
	k.tr, p.tr = tr, tr

	err := tr.ExecutePair(context.Background(), "MKT", pair())
	if err == nil {
		t.Fatal("expected a risk rejection")
	}
	if len(k.orders())+len(p.orders()) != 0 {
		t.Error("no order may be placed when risk rejects a leg")
	}
}

var _ exec.Executor = (*fakeExec)(nil)

// SAFETY: if the venue never reports a fill, the trader must NOT assume a
// position. Previously an accepted IOC was booked as filled, which could leave
// the bot believing it was hedged when it held nothing (or vice versa).
func TestNoFillEventMeansNoPosition(t *testing.T) {
	tr, k, p, eng := setup(t)
	k.tr, p.tr = nil, nil // venue accepts the order but reports NO fill

	if err := tr.ExecutePair(context.Background(), "MKT", pair()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got := eng.Position("MKT"); got != 0 {
		t.Errorf("position = %d; want 0 (no fill event => no position)", got)
	}
	if s := tr.Stats(); s.PairsCompleted != 0 {
		t.Errorf("completed = %d; want 0", s.PairsCompleted)
	}
}

// The P&L floor was previously an inert stub. Prove realized P&L is actually
// accrued from fills, so the supervisor's floor can fire.
func TestRealizedPnLAccruesFromFills(t *testing.T) {
	tr, _, _, _ := setup(t)
	// Buy 10 contracts of YES at $0.50 => -$5.00 cash, fee $0.10.
	tr.OnFill(Fill{Venue: "kalshi", ClientOrderID: "x", Side: core.BuyYes,
		Px: 5000, Qty: 1000, FeeUSD: 0.10})
	// Sell 10 contracts at $0.60 => +$6.00 cash, fee $0.10.
	tr.OnFill(Fill{Venue: "polymarketus", ClientOrderID: "y", Side: core.SellYes,
		Px: 6000, Qty: 1000, FeeUSD: 0.10})

	// realized = -5 + 6 = +1.00; net of $0.20 fees => +0.80
	if got := tr.RealizedUSD(); got < 0.79 || got > 0.81 {
		t.Errorf("realized = %.4f; want ~0.80", got)
	}
}

// HasUnhedged must be true while a leg is unhedged so the supervisor can kill.
func TestHasUnhedgedTracksState(t *testing.T) {
	tr, k, _, _ := setup(t)
	if tr.HasUnhedged() {
		t.Fatal("should start hedged")
	}
	k.fail = true // leg 2 (kalshi) fails -> unhedged, then exit clears it
	_ = tr.ExecutePair(context.Background(), "MKT", pair())
	tr.pending.Wait() // the hedge grace window runs in the background
	if tr.HasUnhedged() {
		t.Error("exit should have cleared the unhedged flag")
	}
	if s := tr.Stats(); s.PairsLegged != 1 {
		t.Errorf("expected a legged pair, got %+v", s)
	}
}

// SAFETY: if no fill event arrives but the VENUE reports a position, that is a
// silent fill — the bot must kill rather than believe it holds nothing.
func TestSilentFillDetectedAndKills(t *testing.T) {
	tr, k, p, eng := setup(t)
	k.tr, p.tr = nil, nil // venue accepts but reports NO fill
	tr.SetVerifier(func(context.Context, string, string) (float64, error) {
		return -5, nil // ...yet the venue says we are short 5
	})
	err := tr.ExecutePair(context.Background(), "MKT", pair())
	if err == nil {
		t.Fatal("expected an error on silent fill")
	}
	if !eng.Killed() {
		t.Error("a silent fill MUST engage the kill switch")
	}
}

// A position the bot legitimately opened must NOT read as drift. Previously
// Positions() was a stub returning nothing, so the first successful hedged pair
// tripped the kill switch within 30s.
func TestPositionsTrackFillsForDrift(t *testing.T) {
	tr, _, _, _ := setup(t)
	tr.OnFill(Fill{Venue: "kalshi", VenueID: "K", ClientOrderID: "a", Side: core.BuyYes, Px: 5000, Qty: 500})
	tr.OnFill(Fill{Venue: "polymarketus", VenueID: "P", ClientOrderID: "b", Side: core.SellYes, Px: 6000, Qty: 500})

	pos := tr.Positions()
	if pos["K"] != 5 {
		t.Errorf("kalshi position = %v; want +5 (bought YES)", pos["K"])
	}
	if pos["P"] != -5 {
		t.Errorf("poly position = %v; want -5 (sold YES)", pos["P"])
	}
}

// SAFETY: if the silent-fill check itself FAILS, the bot is in exactly the state
// it was on 2026-08-25 — unable to tell "no fill" from "naked position". A blind
// safety check must kill, not shrug and return success.
func TestVerifierFailureKills(t *testing.T) {
	tr, k, p, eng := setup(t)
	k.tr, p.tr = nil, nil // venue accepts but reports NO fill
	tr.SetVerifier(func(context.Context, string, string) (float64, error) {
		return 0, errors.New("503 from venue")
	})
	if err := tr.ExecutePair(context.Background(), "MKT", pair()); err == nil {
		t.Fatal("expected an error when the position check is blind")
	}
	if !eng.Killed() {
		t.Error("a blind silent-fill check MUST engage the kill switch")
	}
}

// The silent-fill test compares against what the bot BELIEVES it holds, not
// against zero. A position the bot legitimately opened earlier must not be
// re-read as a silent fill, or the second trade in any held market kills.
func TestExistingPositionIsNotASilentFill(t *testing.T) {
	tr, k, p, eng := setup(t)
	// The bot legitimately sold 5 YES on Poly ("P") in an earlier pair.
	tr.OnFill(Fill{Venue: "polymarketus", VenueID: "P", ClientOrderID: "old",
		Side: core.SellYes, Px: 6000, Qty: 500})
	if got := tr.Positions()["P"]; got != -5 {
		t.Fatalf("setup: bot position = %v, want -5", got)
	}

	k.tr, p.tr = nil, nil // this pair's leg 1 reports NO fill
	// Answer per market: Poly holds the known -5, Kalshi holds nothing. Both
	// legs are verified now, so a fixture that returns -5 for every market
	// would look like an unexplained Kalshi position.
	tr.SetVerifier(func(_ context.Context, venue, venueID string) (float64, error) {
		if venueID == "P" {
			return -5, nil
		}
		return 0, nil
	})
	if err := tr.ExecutePair(context.Background(), "MKT", pair()); err != nil {
		t.Fatalf("a known position must not be treated as a silent fill: %v", err)
	}
	if eng.Killed() {
		t.Error("killed on a position the bot already knew about")
	}
}

// Both legs must go out CONCURRENTLY, not one after the other. Sequential
// legging left a multi-second window (fillWait) in which we held one side while
// the other moved away — the backtest blamed 63% of attempts on that fade.
func TestBothLegsPlacedConcurrently(t *testing.T) {
	tr, k, p, _ := setup(t)
	var mu sync.Mutex
	var starts []time.Time
	stamp := func() { mu.Lock(); starts = append(starts, time.Now()); mu.Unlock() }
	k.onPlace, p.onPlace = stamp, stamp

	if err := tr.ExecutePair(context.Background(), "MKT", pair()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	tr.pending.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(starts) != 2 {
		t.Fatalf("want 2 placements, got %d", len(starts))
	}
	// Sequential legging would separate them by at least fillWait (200ms here).
	if gap := starts[1].Sub(starts[0]); gap > 50*time.Millisecond {
		t.Errorf("legs placed %v apart; they must be concurrent, not sequential", gap)
	}
}

// A leg that fills alone must NOT be dumped immediately — it gets the grace
// window to complete first, because a completed pair pays out while a dump just
// realizes the loss.
func TestLeggedPairCompletesWithinGraceWindow(t *testing.T) {
	tr, k, p, _ := setup(t)
	k.fail = true // kalshi rejects at first, so we end up naked on poly

	if err := tr.ExecutePair(context.Background(), "MKT", pair()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	// Kalshi recovers before the grace window expires.
	time.Sleep(40 * time.Millisecond)
	k.setFail(false)
	tr.pending.Wait()

	if s := tr.Stats(); s.PairsCompleted != 1 {
		t.Errorf("completed = %d; want 1 (the retry should have hedged it)", s.PairsCompleted)
	}
	if tr.HasUnhedged() {
		t.Error("must be flat after completing the hedge")
	}
	// Completing means hedging on the MISSING venue, not dumping on the held one.
	if len(p.orders()) != 1 {
		t.Errorf("poly orders = %d; want 1 (entry only — no dump)", len(p.orders()))
	}
}

// When the missing side never comes back, the grace window must expire and the
// naked leg must be dumped rather than held indefinitely.
func TestGraceExpiryDumpsNakedLeg(t *testing.T) {
	tr, k, p, _ := setup(t)
	k.fail = true // kalshi never recovers

	if err := tr.ExecutePair(context.Background(), "MKT", pair()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	tr.pending.Wait()

	if s := tr.Stats(); s.ExitsPlaced != 1 {
		t.Errorf("exits = %d; want 1 (grace expired => dump)", s.ExitsPlaced)
	}
	if tr.HasUnhedged() {
		t.Error("the unhedged tally must be cleared after the dump")
	}
	po := p.orders()
	if len(po) != 2 || po[0].Side == po[1].Side {
		t.Errorf("expected an entry plus an OPPOSITE-side exit on poly, got %d orders", len(po))
	}
}

// The grace window must stay strictly inside the supervisor's unhedged kill
// timer, or the bot kills itself in the middle of its own recovery.
func TestHedgeGraceIsShorterThanSupervisorKill(t *testing.T) {
	tr := NewTrader(Executors{}, risk.New(risk.Config{}, nil, ""), nil, time.Second)
	sup := &Supervisor{}
	sup.MaxUnhedgedFor = 60 * time.Second // the production default
	if tr.hedgeGrace >= sup.MaxUnhedgedFor {
		t.Fatalf("hedgeGrace %v must be < MaxUnhedgedFor %v", tr.hedgeGrace, sup.MaxUnhedgedFor)
	}
}

// --- the fill/registration race (2026-08-25) ---

// Polymarket correlates fills by the VENUE-assigned order id, which the trader
// can only learn from the Place() response. Live, the private order stream
// delivered the execution 1ms BEFORE that response was parsed: the fill matched
// no sink, was logged as "fill for unknown order" and DROPPED, place() reported
// filled=0.00, and the hedge grace window dutifully re-placed a leg that had
// already filled — leaving 10 sold against 5 bought and 5 contracts naked.
//
// The fill must be parked and claimed, not dropped.
func TestFillArrivingBeforeVenueOrderIDIsClaimed(t *testing.T) {
	tr, _, p, _ := setup(t)
	const venueOID = "C3CY9AZR6FSR"
	p.tr = nil // this leg reports its own fill, keyed the way Polymarket does
	p.placeFn = func(o core.Order) (string, error) {
		tr.OnFill(Fill{Venue: "polymarketus", VenueID: o.VenueID, ClientOrderID: venueOID,
			Side: o.Side, Px: o.Px, Qty: o.Qty})
		return venueOID, nil // ...only NOW does the trader learn the key
	}

	if err := tr.ExecutePair(context.Background(), "MKT", pair()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	tr.pending.Wait()

	if n := len(p.orders()); n != 1 {
		t.Fatalf("poly orders = %d; want 1 — a dropped fill makes the grace window re-place a filled leg", n)
	}
	if s := tr.Stats(); s.PairsCompleted != 1 || s.PairsLegged != 0 {
		t.Errorf("completed=%d legged=%d; want 1/0", s.PairsCompleted, s.PairsLegged)
	}
}

// A fill that never gets claimed still has to move the ledger immediately and
// then be surfaced loudly once it expires — parking must not become a place
// where real fills go quiet.
func TestUnclaimedFillMovesLedgerThenExpires(t *testing.T) {
	tr, _, _, _ := setup(t)
	tr.OnFill(Fill{Venue: "polymarketus", VenueID: "P", ClientOrderID: "GHOST",
		Side: core.SellYes, Px: 6000, Qty: 500})

	if got := tr.Positions()["P"]; got != -5 {
		t.Errorf("posByVenueID[P] = %v; want -5 — an unmatched fill still moved real money", got)
	}

	tr.mu.Lock()
	pk, ok := tr.parked["GHOST"]
	if !ok {
		tr.mu.Unlock()
		t.Fatal("fill was not parked")
	}
	pk.at = time.Now().Add(-2 * parkRetention) // age it past retention
	tr.parked["GHOST"] = pk
	tr.mu.Unlock()

	tr.sweepParked()

	tr.mu.Lock()
	_, still := tr.parked["GHOST"]
	tr.mu.Unlock()
	if still {
		t.Error("expired parked fill was never swept — it would sit unreported forever")
	}
}

// Parking must not let a stale fill be claimed by a LATER, unrelated order that
// happens to reuse the key.
func TestClaimedParkedFillIsRemoved(t *testing.T) {
	tr, _, _, _ := setup(t)
	tr.OnFill(Fill{Venue: "polymarketus", VenueID: "P", ClientOrderID: "K1",
		Side: core.SellYes, Px: 6000, Qty: 500})

	ch := make(chan Fill, 8)
	tr.mu.Lock()
	tr.claimParkedLocked("K1", ch)
	_, still := tr.parked["K1"]
	tr.mu.Unlock()

	if still {
		t.Error("parked entry survived being claimed — a second order could double-count it")
	}
	if len(ch) != 1 {
		t.Fatalf("claimed %d fills; want 1", len(ch))
	}
}

// --- fill-wait early return ---

// A fully-filled leg must not sit out the rest of fillWait. Live that was 3s of
// dead time per leg, during which the OTHER leg could already be filled and
// unhedged — the wait window was itself a source of naked exposure.
func TestFullFillReturnsBeforeFillWaitExpires(t *testing.T) {
	tr, _, _, _ := setup(t)
	tr.fillWait = 3 * time.Second // as configured in production

	start := time.Now()
	if err := tr.ExecutePair(context.Background(), "MKT", pair()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	tr.pending.Wait()
	elapsed := time.Since(start)

	if elapsed > time.Second {
		t.Errorf("ExecutePair took %v with fillWait=%v — a full fill must return immediately",
			elapsed, tr.fillWait)
	}
	if s := tr.Stats(); s.PairsCompleted != 1 {
		t.Errorf("completed = %d; want 1", s.PairsCompleted)
	}
}

// A PARTIAL fill must still wait out the whole window: the remainder can land
// inside it, and returning early would strand a leg that was about to complete.
func TestPartialFillStillWaitsOutTheWindow(t *testing.T) {
	tr, _, p, _ := setup(t)
	tr.fillWait = 300 * time.Millisecond
	p.tr = nil
	p.placeFn = func(o core.Order) (string, error) {
		go tr.OnFill(Fill{Venue: "polymarketus", VenueID: o.VenueID, ClientOrderID: o.ClientID,
			Side: o.Side, Px: o.Px, Qty: o.Qty / 2}) // half fill, never completed
		return "poid", nil
	}

	start := time.Now()
	if err := tr.ExecutePair(context.Background(), "MKT", pair()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	elapsed := time.Since(start)
	tr.pending.Wait()

	if elapsed < tr.fillWait {
		t.Errorf("returned in %v; a partial fill must wait the full %v", elapsed, tr.fillWait)
	}
}

// --- the asymmetric path: one leg filled, one leg silent ---

// The dangerous case is NOT "neither leg filled" — that costs nothing. It is one
// leg filled and one silent, because that is when the bot re-places. Before
// 2026-08-25 the venue was never consulted here, so a leg whose fill event was
// lost got sold a second time.
func TestSilentFillOnTheMissingLegIsCaughtBeforeReplacing(t *testing.T) {
	tr, _, p, eng := setup(t)
	p.tr = nil // poly's fill event never arrives...
	p.placeFn = func(o core.Order) (string, error) { return "poid", nil }
	tr.SetVerifier(func(_ context.Context, venue, venueID string) (float64, error) {
		if venueID == "P" {
			return -5, nil // ...but the venue holds the position anyway
		}
		return 0, nil
	})

	err := tr.ExecutePair(context.Background(), "MKT", pair())
	tr.pending.Wait()

	if err == nil {
		t.Fatal("want an error for a silent fill on the missing leg, got nil")
	}
	if !eng.Killed() {
		t.Error("a silent fill must engage the kill switch")
	}
	if n := len(p.orders()); n != 1 {
		t.Errorf("poly orders = %d; want 1 — the bot re-sold a leg that had already filled", n)
	}
}

// A fill EVENT that lands after fillWait closed must stop the grace window from
// re-placing. The venue query can miss this (its position endpoint may lag), so
// the last line of defence is the bot's own belief, which OnFill updates for
// EVERY fill including uncorrelated ones.
func TestLateFillEventStopsTheGraceWindowReplacing(t *testing.T) {
	tr, _, p, _ := setup(t)
	tr.fillWait = 100 * time.Millisecond
	tr.hedgeRetry = 300 * time.Millisecond
	tr.hedgeGrace = 2 * time.Second
	// A venue whose position endpoint agrees with the bot: the network check
	// cannot help here, so only the late fill event can.
	tr.SetVerifier(func(_ context.Context, venue, venueID string) (float64, error) {
		return tr.Positions()[venueID], nil
	})
	p.tr = nil
	p.placeFn = func(o core.Order) (string, error) {
		go func() {
			time.Sleep(150 * time.Millisecond) // lands AFTER fillWait expires
			tr.OnFill(Fill{Venue: "polymarketus", VenueID: o.VenueID, ClientOrderID: o.ClientID,
				Side: o.Side, Px: o.Px, Qty: o.Qty})
		}()
		return "poid", nil
	}

	if err := tr.ExecutePair(context.Background(), "MKT", pair()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	tr.pending.Wait()

	if n := len(p.orders()); n != 1 {
		t.Errorf("poly orders = %d; want 1 — the late fill should have been noticed before re-placing", n)
	}
	if s := tr.Stats(); s.PairsCompleted != 1 {
		t.Errorf("completed = %d; want 1 — the pair was hedged, just late", s.PairsCompleted)
	}
	if tr.HasUnhedged() {
		t.Error("unhedged tally still set after the pair completed")
	}
}

// fillWait bounds how long a naked leg goes UNNOTICED, so it must stay well
// inside the grace window it feeds.
func TestFillWaitIsShorterThanHedgeGrace(t *testing.T) {
	tr := NewTrader(Executors{}, nil, nil, time.Second)
	if tr.fillWait >= tr.hedgeGrace {
		t.Errorf("fillWait %v >= hedgeGrace %v — a leg would be dumped before it was noticed",
			tr.fillWait, tr.hedgeGrace)
	}
	if tr.fillWait != time.Second {
		t.Errorf("fillWait = %v; want 1s", tr.fillWait)
	}
}

func TestPairVerdict(t *testing.T) {
	k := core.Intent{Venue: "kalshi", Qty: 500}
	p := core.Intent{Venue: "polymarketus", Qty: 500}
	cases := []struct {
		name           string
		f1, f2         core.Qty
		wantVerdict    string
		wantNakedVenue string
		wantNakedQty   core.Qty
	}{
		{"clean hedge", 500, 500, "HEDGED", "", 0},
		{"both zero", 0, 0, "BOTH_ZERO", "", 0},
		{"thin equal fill", 200, 200, "HEDGED_PARTIAL", "", 0},
		{"naked leg1 (kalshi over)", 500, 0, "LEGGED", "kalshi", 500},
		{"naked leg2 (poly over)", 0, 500, "LEGGED", "polymarketus", 500},
		{"partial legged", 500, 200, "LEGGED", "kalshi", 300},
	}
	for _, c := range cases {
		v, nv, nq := pairVerdict(k, p, c.f1, c.f2)
		if v != c.wantVerdict || nv != c.wantNakedVenue || nq != c.wantNakedQty {
			t.Errorf("%s: got (%q,%q,%v), want (%q,%q,%v)", c.name, v, nv, nq, c.wantVerdict, c.wantNakedVenue, c.wantNakedQty)
		}
	}
}

func TestLegState(t *testing.T) {
	for _, c := range []struct {
		req, filled core.Qty
		want        string
	}{{500, 500, "FULL"}, {500, 200, "PARTIAL"}, {500, 0, "NONE"}} {
		if got := legState(c.req, c.filled); got != c.want {
			t.Errorf("legState(%v,%v)=%q want %q", c.req, c.filled, got, c.want)
		}
	}
}
