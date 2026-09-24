package live

import (
	"context"
	"errors"
	"testing"
	"time"

	"predbot/internal/core"
	"predbot/internal/risk"
)

// makerPair rests the Polymarket leg (GTC, earns the rebate) and takes the
// Kalshi leg (IOC), the shape MakerCrossVenue emits.
func makerPair() []core.Intent {
	return []core.Intent{
		{Venue: "kalshi", VenueID: "K", Side: core.BuyYes, Px: 5000, Qty: 500, TIF: core.IOC},
		{Venue: "polymarketus", VenueID: "P", Side: core.SellYes, Px: 6000, Qty: 500, TIF: core.GTC},
	}
}

// A resting leg that fills is hedged through completeOrExit -> pair completes,
// with one order on each venue (the rest, then the taken hedge).
func TestMakerRestFillsThenHedges(t *testing.T) {
	tr, k, p, _ := setup(t)
	tr.restWait = 300 * time.Millisecond
	if err := tr.ExecuteMakerPair(context.Background(), "MKT", makerPair()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	tr.pending.Wait() // let the background rest-wait + hedge finish
	if len(p.orders()) != 1 {
		t.Fatalf("want 1 resting order on poly, got %d", len(p.orders()))
	}
	if len(k.orders()) != 1 {
		t.Fatalf("want 1 hedge order on kalshi, got %d", len(k.orders()))
	}
	if s := tr.Stats(); s.PairsCompleted != 1 {
		t.Errorf("completed = %d; want 1", s.PairsCompleted)
	}
	if tr.HasUnhedged() {
		t.Errorf("still unhedged after a completed maker pair")
	}
}

// A resting leg that NEVER fills is cancelled after restWait, leaves no
// position, and places no hedge — the clean, common maker outcome.
func TestMakerRestTimesOutCancelsClean(t *testing.T) {
	tr, k, p, eng := setup(t)
	tr.restWait = 150 * time.Millisecond
	tr.SetVerifier(func(context.Context, string, string) (float64, error) { return 0, nil })
	// Poly accepts the rest but reports NO fill (it just sits on the book).
	p.placeFn = func(core.Order) (string, error) { return "poid", nil }

	if err := tr.ExecuteMakerPair(context.Background(), "MKT", makerPair()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	tr.pending.Wait()
	if len(p.orders()) != 1 {
		t.Fatalf("want the rest recorded on poly, got %d", len(p.orders()))
	}
	if len(k.orders()) != 0 {
		t.Fatalf("no hedge should be placed when the rest never fills, got %d kalshi orders", len(k.orders()))
	}
	if s := tr.Stats(); s.PairsCompleted != 0 || s.PairsLegged != 0 {
		t.Errorf("completed=%d legged=%d; want 0/0", s.PairsCompleted, s.PairsLegged)
	}
	if tr.HasUnhedged() {
		t.Errorf("unhedged tally should be clear after an unfilled cancelled rest")
	}
	if eng.Killed() {
		t.Errorf("engine must not kill on a clean unfilled cancel")
	}
}

// A rest that fills while the fill stream is DOWN (no event) is discovered by
// the venue check and RECOVERED — the Kalshi hedge is placed and the pair
// completes — instead of killing. This is the hedge-or-flatten path.
func TestMakerSilentFillRecoversByHedging(t *testing.T) {
	tr, k, p, eng := setup(t)
	tr.restWait = 100 * time.Millisecond
	// Poly accepts the rest but the stream reports NO fill (it was disconnected).
	p.placeFn = func(core.Order) (string, error) { return "poid", nil }
	// Asked directly, the venue reveals the rest actually filled: short 5 on Poly
	// (the rested leg is a SELL), which matches the resting order's direction.
	tr.SetVerifier(func(_ context.Context, venue, _ string) (float64, error) {
		if venue == "polymarketus" {
			return -5, nil
		}
		return 0, nil
	})

	if err := tr.ExecuteMakerPair(context.Background(), "MKT", makerPair()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	tr.pending.Wait()

	if eng.Killed() {
		t.Fatal("a silent fill with known provenance must recover, not kill")
	}
	if len(k.orders()) != 1 {
		t.Fatalf("want the Kalshi hedge placed to recover, got %d kalshi orders", len(k.orders()))
	}
	if s := tr.Stats(); s.PairsCompleted != 1 {
		t.Errorf("completed=%d; want 1 (recovered pair hedged whole)", s.PairsCompleted)
	}
	if tr.HasUnhedged() {
		t.Error("unhedged tally should clear once the recovered fill is hedged")
	}
}

// A venue position that points the WRONG way relative to the resting order is
// not "our missed fill" — it is unexplained drift, and must still hard-stop.
func TestMakerSilentFillWrongDirectionKills(t *testing.T) {
	tr, k, p, eng := setup(t)
	tr.restWait = 100 * time.Millisecond
	p.placeFn = func(core.Order) (string, error) { return "poid", nil }
	// We rested a SELL, but the venue shows a LONG — this is not our fill.
	tr.SetVerifier(func(_ context.Context, venue, _ string) (float64, error) {
		if venue == "polymarketus" {
			return +5, nil
		}
		return 0, nil
	})

	if err := tr.ExecuteMakerPair(context.Background(), "MKT", makerPair()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	tr.pending.Wait()

	if !eng.Killed() {
		t.Fatal("unexplained-direction drift must kill")
	}
	if len(k.orders()) != 0 {
		t.Fatalf("no hedge should be placed on unexplained drift, got %d kalshi orders", len(k.orders()))
	}
}

// A killed engine places nothing.
func TestMakerKilledPlacesNothing(t *testing.T) {
	tr, k, p, eng := setup(t)
	eng.Kill("test")
	err := tr.ExecuteMakerPair(context.Background(), "MKT", makerPair())
	if err != risk.ErrKilled {
		t.Fatalf("err = %v; want ErrKilled", err)
	}
	if len(p.orders()) != 0 || len(k.orders()) != 0 {
		t.Fatalf("killed engine placed orders: poly=%d kalshi=%d", len(p.orders()), len(k.orders()))
	}
}

// A pair whose resting leg is on Kalshi is refused cleanly — no order, no
// attempt (only the Poly leg may rest: rebate + order-group reasons).
func TestMakerRefusesKalshiRest(t *testing.T) {
	tr, k, p, _ := setup(t)
	legs := []core.Intent{
		{Venue: "kalshi", VenueID: "K", Side: core.BuyYes, Px: 5000, Qty: 500, TIF: core.GTC},
		{Venue: "polymarketus", VenueID: "P", Side: core.SellYes, Px: 6000, Qty: 500, TIF: core.IOC},
	}
	if err := tr.ExecuteMakerPair(context.Background(), "MKT", legs); err != nil {
		t.Fatalf("execute: %v", err)
	}
	tr.pending.Wait()
	if len(k.orders()) != 0 || len(p.orders()) != 0 {
		t.Fatalf("a Kalshi-rest pair must place nothing, got kalshi=%d poly=%d", len(k.orders()), len(p.orders()))
	}
	if s := tr.Stats(); s.PairsAttempted != 0 {
		t.Errorf("attempted=%d; want 0 (skipped before any attempt)", s.PairsAttempted)
	}
}

// A pair with no resting leg (both IOC) falls back to the taker path.
func TestMakerNoRestingLegFallsBackToTaker(t *testing.T) {
	tr, k, p, _ := setup(t)
	if err := tr.ExecuteMakerPair(context.Background(), "MKT", pair()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	tr.pending.Wait()
	if len(k.orders()) != 1 || len(p.orders()) != 1 {
		t.Fatalf("taker fallback want 1 per venue, got kalshi=%d poly=%d", len(k.orders()), len(p.orders()))
	}
	if s := tr.Stats(); s.PairsCompleted != 1 {
		t.Errorf("completed = %d; want 1", s.PairsCompleted)
	}
}

// 2026-09-24: a rest filled with no event and the drift monitor killed 23s in,
// before the rest's own post-cancel check at 30s. Polling the venue during the
// rest finds the fill in seconds and recovers it, and the market is marked
// resting (so drift stands aside) only while the lifecycle owns it.
func TestMakerPollCatchesSilentFillEarly(t *testing.T) {
	tr, k, p, eng := setup(t)
	tr.restWait = 5 * time.Second
	tr.restPoll = 20 * time.Millisecond
	p.placeFn = func(core.Order) (string, error) { return "poid", nil } // no fill event, ever
	filledAt := time.Now().Add(60 * time.Millisecond)
	tr.SetVerifier(func(_ context.Context, venue, _ string) (float64, error) {
		if venue == "polymarketus" && time.Now().After(filledAt) {
			return -5, nil // the rested SELL filled
		}
		return 0, nil
	})

	start := time.Now()
	if err := tr.ExecuteMakerPair(context.Background(), "MKT", makerPair()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !tr.Resting("P") {
		t.Error("market should be marked resting while the rest works")
	}
	tr.pending.Wait()

	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("recovery took %v; the poll should beat the %v rest wait", took, tr.restWait)
	}
	if tr.Resting("P") {
		t.Error("resting mark must clear when the lifecycle ends")
	}
	if eng.Killed() {
		t.Fatal("a silent fill caught by the poll must recover, not kill")
	}
	if len(k.orders()) != 1 {
		t.Fatalf("want the Kalshi hedge, got %d kalshi orders", len(k.orders()))
	}
	if s := tr.Stats(); s.PairsCompleted != 1 {
		t.Errorf("completed=%d; want 1", s.PairsCompleted)
	}
}

// The poll must not mistake a position the bot already knows about (a fill it
// DID see, or an earlier pair) for a silent fill.
func TestMakerPollIgnoresKnownPosition(t *testing.T) {
	tr, k, p, eng := setup(t)
	tr.restWait = 200 * time.Millisecond
	tr.restPoll = 20 * time.Millisecond
	tr.posByVenueID["P"] = -3 // held from an earlier pair
	p.placeFn = func(core.Order) (string, error) { return "poid", nil }
	tr.SetVerifier(func(_ context.Context, venue, _ string) (float64, error) {
		if venue == "polymarketus" {
			return -3, nil
		}
		return 0, nil
	})
	if err := tr.ExecuteMakerPair(context.Background(), "MKT", makerPair()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	tr.pending.Wait()
	if eng.Killed() || len(k.orders()) != 0 {
		t.Errorf("known position read as a fill: killed=%v kalshi orders=%d", eng.Killed(), len(k.orders()))
	}
}

// 2026-09-24: every Poly cancel failed, the bot logged "abandoned cleanly", and
// the still-working rest filled 6 minutes later with no hedge. A failed cancel
// on an order the venue still lists as open must kill, not be forgotten.
func TestMakerFailedCancelOnOpenOrderKills(t *testing.T) {
	tr, k, p, eng := setup(t)
	tr.restWait = 50 * time.Millisecond
	p.placeFn = func(core.Order) (string, error) { return "poid", nil }
	p.cancelErr = errors.New("cancel 400")
	tr.SetVerifier(func(context.Context, string, string) (float64, error) { return 0, nil })
	tr.SetOrderChecker(func(_ context.Context, _, id string) (bool, error) { return id == "poid", nil })

	if err := tr.ExecuteMakerPair(context.Background(), "MKT", makerPair()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	tr.pending.Wait()
	if !eng.Killed() {
		t.Fatal("a rest that could not be cancelled and is still open must kill")
	}
	if p.cancels != 3 {
		t.Errorf("cancel attempts = %d; want 3 before giving up", p.cancels)
	}
	if len(k.orders()) != 0 {
		t.Errorf("no hedge for an unfilled rest, got %d kalshi orders", len(k.orders()))
	}
}

// If the open-order check itself fails, the order may still be working: kill.
func TestMakerFailedCancelUnverifiableKills(t *testing.T) {
	tr, _, p, eng := setup(t)
	tr.restWait = 50 * time.Millisecond
	p.placeFn = func(core.Order) (string, error) { return "poid", nil }
	p.cancelErr = errors.New("cancel 400")
	tr.SetVerifier(func(context.Context, string, string) (float64, error) { return 0, nil })
	tr.SetOrderChecker(func(context.Context, string, string) (bool, error) { return true, errors.New("timeout") })
	if err := tr.ExecuteMakerPair(context.Background(), "MKT", makerPair()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	tr.pending.Wait()
	if !eng.Killed() {
		t.Fatal("an unverifiable failed cancel must kill")
	}
}

// A cancel also fails on an order that already filled. If the venue no longer
// lists it, carry on: the position check then finds and recovers the fill.
func TestMakerFailedCancelOnFilledOrderRecovers(t *testing.T) {
	tr, k, p, eng := setup(t)
	tr.restWait = 50 * time.Millisecond
	p.placeFn = func(core.Order) (string, error) { return "poid", nil }
	p.cancelErr = errors.New("cancel 400: order not open")
	tr.SetVerifier(func(_ context.Context, venue, _ string) (float64, error) {
		if venue == "polymarketus" {
			return -5, nil // the rested SELL filled, event lost
		}
		return 0, nil
	})
	tr.SetOrderChecker(func(context.Context, string, string) (bool, error) { return false, nil })
	if err := tr.ExecuteMakerPair(context.Background(), "MKT", makerPair()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	tr.pending.Wait()
	if eng.Killed() {
		t.Fatal("a filled (no longer open) rest must recover, not kill")
	}
	if len(k.orders()) != 1 {
		t.Fatalf("want the Kalshi hedge, got %d", len(k.orders()))
	}
}
