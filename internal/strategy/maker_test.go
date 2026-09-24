package strategy

import (
	"testing"

	"predbot/internal/core"
)

// makerFee rebates the maker and charges the taker, so a pair only clears when
// the rest genuinely rests.
type makerFee struct{ maker, taker int64 } // per contract, $0.0001

func (f makerFee) Fee(_ string, _ core.Price, qty core.Qty, maker bool) int64 {
	per := f.taker
	if maker {
		per = f.maker
	}
	return per * int64(qty) / 100
}

// 2026-09-24 (MLB-20260924-CHW): a "REST sell" was priced at Poly's best BID,
// crossed, and filled instantly as a taker. A rested leg must join its OWN side
// of its own book and never cross.
func TestMakerRestJoinsOwnSideAndNeverCrosses(t *testing.T) {
	books := map[string]core.BookSnapshot{
		// Poly quotes 0.60/0.66; Kalshi 0.55/0.57. Resting a Poly sell at its ask
		// (0.66) and taking Kalshi's ask (0.57) is a 9c gross maker pair.
		"polymarketus": {Venue: "polymarketus", VenueID: "P",
			YesBids: []core.Level{{Px: 6000, Qty: 10000}}, YesAsks: []core.Level{{Px: 6600, Qty: 10000}}},
		"kalshi": {Venue: "kalshi", VenueID: "K",
			YesBids: []core.Level{{Px: 5500, Qty: 10000}}, YesAsks: []core.Level{{Px: 5700, Qty: 10000}}},
	}
	legs := MakerCrossVenue("MKT", books, Exposure{}, makerFee{maker: -50, taker: 300}, Config{MinEdge: 100, MaxClip: 2000})
	if len(legs) != 2 {
		t.Fatalf("want a maker pair, got %+v", legs)
	}
	ri := RestingLeg(legs)
	if ri < 0 {
		t.Fatal("no resting leg")
	}
	rest, take := legs[ri], legs[1-ri]
	for _, l := range legs {
		if l.TIF == core.GTC && RestCrosses(l, books[l.Venue]) {
			t.Fatalf("rest %+v crosses its own book", l)
		}
	}
	if rest.Venue != "polymarketus" || rest.Side != core.SellYes || rest.Px != 6600 {
		t.Errorf("rest = %+v; want SELL on poly at its ask 0.66", rest)
	}
	if take.Venue != "kalshi" || take.Side != core.BuyYes || take.Px != 5700 || take.TIF != core.IOC {
		t.Errorf("take = %+v; want IOC BUY on kalshi at its ask 0.57", take)
	}
	if rest.Qty != 2000 || take.Qty != 2000 {
		t.Errorf("qty = %d/%d; want MaxClip 2000", rest.Qty, take.Qty)
	}
}

// The old pricing's "edge" came from crossing: Poly bid 0.60 over Kalshi ask
// 0.59. Resting at the correct prices, every maker shape here nets <= 0 after
// the hedge's 3c taker fee, so there is no pair.
func TestMakerNoPairWhenOnlyTheCrossPaid(t *testing.T) {
	books := map[string]core.BookSnapshot{
		"polymarketus": {Venue: "polymarketus", VenueID: "P",
			YesBids: []core.Level{{Px: 6000, Qty: 10000}}, YesAsks: []core.Level{{Px: 6050, Qty: 10000}}},
		"kalshi": {Venue: "kalshi", VenueID: "K",
			YesBids: []core.Level{{Px: 5850, Qty: 10000}}, YesAsks: []core.Level{{Px: 5900, Qty: 10000}}},
	}
	if legs := MakerCrossVenue("MKT", books, Exposure{}, makerFee{maker: -50, taker: 300}, Config{MinEdge: 100}); legs != nil {
		t.Fatalf("want no pair, got %+v", legs)
	}
}

func TestMakerHedgeNeedsDepth(t *testing.T) {
	books := map[string]core.BookSnapshot{
		"polymarketus": {Venue: "polymarketus", VenueID: "P",
			YesBids: []core.Level{{Px: 6000, Qty: 10000}}, YesAsks: []core.Level{{Px: 6600, Qty: 10000}}},
		"kalshi": {Venue: "kalshi", VenueID: "K",
			YesBids: []core.Level{{Px: 5500, Qty: 100}}, YesAsks: []core.Level{{Px: 5700, Qty: 100}}},
	}
	// Kalshi shows 1 contract a side, below MinDepth 10: it may still be the
	// RESTING venue, but never the hedge a filled rest depends on.
	legs := MakerCrossVenue("MKT", books, Exposure{}, makerFee{maker: -50, taker: 300}, Config{MinEdge: 100, MinDepth: 1000})
	if ri := RestingLeg(legs); ri >= 0 && legs[1-ri].Venue == "kalshi" {
		t.Fatalf("hedge placed on a 1-contract book: %+v", legs)
	}
}

func TestRestCrosses(t *testing.T) {
	b := core.BookSnapshot{YesBids: []core.Level{{Px: 6000, Qty: 1}}, YesAsks: []core.Level{{Px: 6100, Qty: 1}}}
	cases := []struct {
		side core.Side
		px   core.Price
		want bool
	}{
		{core.SellYes, 6000, true}, {core.SellYes, 6100, false},
		{core.BuyYes, 6100, true}, {core.BuyYes, 6000, false},
	}
	for _, c := range cases {
		if got := RestCrosses(core.Intent{Side: c.side, Px: c.px}, b); got != c.want {
			t.Errorf("%v @%d crosses = %v; want %v", c.side, c.px, got, c.want)
		}
	}
}
