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
	// cancelErr, when set, is returned by every Cancel (the venue refused it).
	cancelErr error
	cancels   int
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
func (f *fakeExec) Cancel(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels++
	return f.cancelErr
}
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

// The HEDGE fails -> the naked quantity is on the LEAD (kalshi) and must be
// exited on the lead's own venue. Under the lead/hedge model the lead fills
// first and the hedge is what can fall short, so the naked leg is always kalshi.
func TestLeggedExitOnHedgeFailure(t *testing.T) {
	tr, k, p, _ := setup(t)
	// Poly (the hedge) fails; kalshi (the lead) filled and is the naked leg to exit.
	p.fail = true
	_ = tr.ExecutePair(context.Background(), "MKT", pair())
	tr.pending.Wait() // the naked leg is now resolved in the background
	s := tr.Stats()
	if s.PairsLegged != 1 {
		t.Errorf("legged = %d; want 1", s.PairsLegged)
	}
	if s.ExitsPlaced != 1 {
		t.Errorf("exits = %d; want 1 (must exit the unhedged leg)", s.ExitsPlaced)
	}
	// The exit must be on the LEAD's venue (kalshi) and OPPOSITE the original side.
	ko := k.orders()
	if len(ko) != 2 {
		t.Fatalf("kalshi orders = %d; want 2 (entry + exit)", len(ko))
	}
	if ko[0].Side == ko[1].Side {
		t.Error("exit must be the opposite side of the entry")
	}
	// The exit must CROSS the touch (sell a long at bestBid 4900 - 2c slip), not
	// rest exactly on it — that resting price is what left legs naked before.
	if ko[1].Side != core.SellYes || ko[1].Px != 4700 {
		t.Errorf("exit should cross: got %s @ %d; want SellYes @ 4700 (4900 bid - 200 slip)", ko[1].Side, ko[1].Px)
	}
	// The hedge failed outright, so poly placed nothing.
	if n := len(p.orders()); n != 0 {
		t.Errorf("poly orders = %d; want 0 (the hedge failed to place)", n)
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
	tr, _, p, _ := setup(t)
	if tr.HasUnhedged() {
		t.Fatal("should start hedged")
	}
	p.fail = true // the hedge fails -> naked lead, then the exit clears it
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

// The hedge must be placed ONLY AFTER the lead fills — the lead/hedge sequencing
// that makes a Polymarket-only naked leg impossible. (This deliberately reverses
// the earlier simultaneous model, which fired both legs together and let either
// fill alone.) The lead (kalshi) must be the first order out; the hedge (poly)
// must follow, never precede it.
func TestHedgeFollowsLeadSequentially(t *testing.T) {
	tr, k, p, _ := setup(t)
	var mu sync.Mutex
	var order []string
	k.onPlace = func() { mu.Lock(); order = append(order, "kalshi"); mu.Unlock() }
	p.onPlace = func() { mu.Lock(); order = append(order, "polymarketus"); mu.Unlock() }

	if err := tr.ExecutePair(context.Background(), "MKT", pair()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	tr.pending.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "kalshi" || order[1] != "polymarketus" {
		t.Fatalf("want the lead (kalshi) placed first then the hedge (poly), got %v", order)
	}
}

// The whole point of leading with Kalshi: if the LEAD takes nothing, the hedge
// (Polymarket) must NEVER be placed. This is what makes the recurring
// "Polymarket only" naked leg structurally impossible.
func TestLeadMissAbortsWithoutHedging(t *testing.T) {
	tr, k, p, _ := setup(t)
	k.fail = true // the lead takes nothing

	if err := tr.ExecutePair(context.Background(), "MKT", pair()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	tr.pending.Wait()

	if n := len(p.orders()); n != 0 {
		t.Errorf("poly orders = %d; want 0 — the hedge must not fire when the lead misses", n)
	}
	s := tr.Stats()
	if s.PairsLegged != 0 || s.ExitsPlaced != 0 {
		t.Errorf("a clean abort must leave nothing naked: legged=%d exits=%d, want 0/0", s.PairsLegged, s.ExitsPlaced)
	}
	if tr.HasUnhedged() {
		t.Error("a lead miss must leave the book flat, not unhedged")
	}
}

// A lead that fills against a failing hedge must NOT be dumped immediately — the
// hedge gets the grace window to complete first, because a completed pair pays
// out while a dump just realizes the loss.
func TestLeggedPairCompletesWithinGraceWindow(t *testing.T) {
	tr, k, p, _ := setup(t)
	p.fail = true // the hedge (poly) rejects at first, so the lead is naked

	if err := tr.ExecutePair(context.Background(), "MKT", pair()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	// The hedge venue recovers before the grace window expires.
	time.Sleep(40 * time.Millisecond)
	p.setFail(false)
	tr.pending.Wait()

	if s := tr.Stats(); s.PairsCompleted != 1 {
		t.Errorf("completed = %d; want 1 (the retry should have hedged it)", s.PairsCompleted)
	}
	if tr.HasUnhedged() {
		t.Error("must be flat after completing the hedge")
	}
	// Completing means hedging on the missing venue (poly), not dumping the lead.
	if len(p.orders()) != 1 {
		t.Errorf("poly orders = %d; want 1 (the completing hedge)", len(p.orders()))
	}
	if len(k.orders()) != 1 {
		t.Errorf("kalshi orders = %d; want 1 (lead entry only — the lead must NOT be dumped)", len(k.orders()))
	}
}

// When the hedge never comes back, the grace window must expire and the naked
// LEAD must be dumped on its own venue rather than held indefinitely.
func TestGraceExpiryDumpsNakedLeg(t *testing.T) {
	tr, k, p, _ := setup(t)
	p.fail = true // the hedge (poly) never recovers, so the lead stays naked

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
	// The dump is on the LEAD's venue (kalshi): entry plus an OPPOSITE-side exit.
	ko := k.orders()
	if len(ko) != 2 || ko[0].Side == ko[1].Side {
		t.Errorf("expected an entry plus an OPPOSITE-side exit on kalshi, got %d orders", len(ko))
	}
	// The hedge never placed (it failed every attempt).
	if n := len(p.orders()); n != 0 {
		t.Errorf("poly orders = %d; want 0 (the hedge failed every attempt)", n)
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
// lost got sold a second time. Since 2026-09-24 the hedge IOC's own venue check
// books the silent fill, so the pair completes: no re-sell, and no kill, since
// the ledger now matches the venue.
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

	if err != nil || eng.Killed() {
		t.Fatalf("err=%v killed=%v; a silent hedge fill booked from the venue completes the pair", err, eng.Killed())
	}
	if n := len(p.orders()); n != 1 {
		t.Errorf("poly orders = %d; want 1 — the bot re-sold a leg that had already filled", n)
	}
	if got := tr.Positions()["P"]; got != -5 {
		t.Errorf("ledger P = %v; want -5 from the venue", got)
	}
	if s := tr.Stats(); s.PairsCompleted != 1 {
		t.Errorf("completed = %d; want 1", s.PairsCompleted)
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

// 2026-09-24: an exit IOC filled with no event, so the bot still believed it
// held the leg it had dumped. The venue check books the fill instead.
func TestIOCSilentFillBookedFromVenue(t *testing.T) {
	tr, k, _, _ := setup(t)
	k.placeFn = func(core.Order) (string, error) { return "koid", nil } // no fill event
	tr.SetVerifier(func(_ context.Context, venue, _ string) (float64, error) {
		if venue == "kalshi" {
			return 5, nil // the BUY filled on the venue
		}
		return 0, nil
	})
	leg := core.Intent{Venue: "kalshi", VenueID: "K", Side: core.BuyYes, Px: 5000, Qty: 500, TIF: core.IOC}
	if err := tr.engine.CheckAndReserve("MKT", leg); err != nil {
		t.Fatal(err)
	}
	filled, err := tr.place(context.Background(), "MKT", leg)
	if err != nil || filled != 500 {
		t.Fatalf("place = %v, %v; want 500 booked from the venue", filled, err)
	}
	if got := tr.Positions()["K"]; got != 5 {
		t.Errorf("ledger K = %v; want 5", got)
	}
}

// A venue move AGAINST the order's direction is not its fill.
func TestIOCSilentFillIgnoresWrongDirection(t *testing.T) {
	tr, k, _, _ := setup(t)
	k.placeFn = func(core.Order) (string, error) { return "koid", nil }
	tr.SetVerifier(func(context.Context, string, string) (float64, error) { return -5, nil })
	leg := core.Intent{Venue: "kalshi", VenueID: "K", Side: core.BuyYes, Px: 5000, Qty: 500, TIF: core.IOC}
	if err := tr.engine.CheckAndReserve("MKT", leg); err != nil {
		t.Fatal(err)
	}
	if filled, _ := tr.place(context.Background(), "MKT", leg); filled != 0 {
		t.Errorf("filled = %v; a short is not a BUY's fill", filled)
	}
}

// 2026-09-25 (NHL CGY): the Poly hedge ack took ~3-4s and a 7c gap closed
// before it landed. An ack over the limit pauses new pairs on that venue.
func TestSlowAckPausesVenue(t *testing.T) {
	tr, _, p, _ := setup(t)
	tr.SetSlowAck(50*time.Millisecond, time.Minute)
	p.placeFn = func(o core.Order) (string, error) {
		time.Sleep(80 * time.Millisecond) // a slow venue ack
		go tr.OnFill(Fill{Venue: "polymarketus", VenueID: o.VenueID, ClientOrderID: "poid", Side: o.Side, Px: o.Px, Qty: o.Qty})
		return "poid", nil
	}
	if tr.VenueSlow("polymarketus") {
		t.Fatal("venue slow before any order")
	}
	if err := tr.ExecutePair(context.Background(), "MKT", pair()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	tr.pending.Wait()
	if !tr.VenueSlow("polymarketus") {
		t.Error("an 80ms ack over a 50ms limit must pause the venue")
	}
	if tr.VenueSlow("kalshi") {
		t.Error("kalshi acked fast; it must not be paused")
	}
}
