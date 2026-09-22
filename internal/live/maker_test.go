package live

import (
	"context"
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
