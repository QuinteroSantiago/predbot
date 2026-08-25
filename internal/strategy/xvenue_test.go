package strategy

import (
	"testing"

	"predbot/internal/core"
)

// A flat fee model for deterministic edge tests.
type flatFee struct{ perContract int64 }

func (f flatFee) Fee(_ string, _ core.Price, qty core.Qty, _ bool) int64 {
	return f.perContract * int64(qty) / 100
}

func TestCrossVenueDetectsArb(t *testing.T) {
	// Venue A: YES ask 0.50 (qty 100). Venue B: YES bid 0.60 (qty 100).
	// Gross 10¢/contract; with 1¢ total fees, net 9¢ > MinEdge 1¢.
	books := map[string]core.BookSnapshot{
		"kalshi":       {Venue: "kalshi", VenueID: "A", YesAsks: []core.Level{{Px: 5000, Qty: 10000}}, YesBids: []core.Level{{Px: 4900, Qty: 10000}}},
		"polymarketus": {Venue: "polymarketus", VenueID: "B", YesBids: []core.Level{{Px: 6000, Qty: 10000}}, YesAsks: []core.Level{{Px: 6100, Qty: 10000}}},
	}
	fees := flatFee{perContract: 50} // 0.5¢/contract each side
	cfg := Config{MinEdge: 100, MaxClip: 5000}
	intents := CrossVenue("MKT", books, Exposure{}, fees, cfg)
	if len(intents) != 2 {
		t.Fatalf("want 2-leg pair, got %d", len(intents))
	}
	if intents[0].Side != core.BuyYes || intents[0].Venue != "kalshi" {
		t.Errorf("leg0 should BuyYes on kalshi (cheap ask), got %+v", intents[0])
	}
	if intents[1].Side != core.SellYes || intents[1].Venue != "polymarketus" {
		t.Errorf("leg1 should SellYes on polymarketus (high bid), got %+v", intents[1])
	}
	if intents[0].Qty != 5000 { // capped by MaxClip
		t.Errorf("qty should be capped to MaxClip 5000, got %d", intents[0].Qty)
	}
}

func TestCrossVenueNoArbWhenGapTooSmall(t *testing.T) {
	// Gross only 1¢; fees eat it -> no intent.
	books := map[string]core.BookSnapshot{
		"kalshi":       {Venue: "kalshi", VenueID: "A", YesAsks: []core.Level{{Px: 5000, Qty: 10000}}},
		"polymarketus": {Venue: "polymarketus", VenueID: "B", YesBids: []core.Level{{Px: 5100, Qty: 10000}}},
	}
	fees := flatFee{perContract: 100} // 1¢/contract each side -> net negative
	cfg := Config{MinEdge: 100, MaxClip: 5000}
	if intents := CrossVenue("MKT", books, Exposure{}, fees, cfg); intents != nil {
		t.Errorf("expected no arb, got %+v", intents)
	}
}

func TestCrossVenueRespectsInFlight(t *testing.T) {
	books := map[string]core.BookSnapshot{
		"kalshi":       {Venue: "kalshi", VenueID: "A", YesAsks: []core.Level{{Px: 5000, Qty: 10000}}},
		"polymarketus": {Venue: "polymarketus", VenueID: "B", YesBids: []core.Level{{Px: 6000, Qty: 10000}}},
	}
	exp := Exposure{InFlight: map[string]bool{"MKT": true}}
	if intents := CrossVenue("MKT", books, exp, flatFee{}, Config{MinEdge: 100, MaxClip: 5000}); intents != nil {
		t.Errorf("should not re-emit while in-flight, got %+v", intents)
	}
}

// A gap against a venue with token depth is NOT an opportunity: filling leg 1
// would leave a naked position when leg 2 can't be hedged. This is the exact
// failure that produced a real unhedged short on 2026-08-24.
func TestMinDepthRejectsPhantomEdge(t *testing.T) {
	books := map[string]core.BookSnapshot{
		// Kalshi ask is a lone contract — a book about to vanish.
		"kalshi":       {Venue: "kalshi", VenueID: "A", YesAsks: []core.Level{{Px: 5000, Qty: 100}}},
		"polymarketus": {Venue: "polymarketus", VenueID: "B", YesBids: []core.Level{{Px: 6000, Qty: 50000}}},
	}
	cfg := Config{MinEdge: 100, MaxClip: 5000, MinDepth: 1000} // need 10 contracts both sides
	if got := CrossVenue("MKT", books, Exposure{}, flatFee{perContract: 50}, cfg); got != nil {
		t.Errorf("thin hedge book must be rejected, got %+v", got)
	}
	// With depth on both sides the same edge IS tradeable.
	books["kalshi"] = core.BookSnapshot{Venue: "kalshi", VenueID: "A",
		YesAsks: []core.Level{{Px: 5000, Qty: 50000}}}
	if got := CrossVenue("MKT", books, Exposure{}, flatFee{perContract: 50}, cfg); len(got) != 2 {
		t.Errorf("deep books should produce a pair, got %+v", got)
	}
}
