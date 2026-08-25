package exec

import (
	"testing"

	"predbot/internal/book"
	"predbot/internal/config"
	"predbot/internal/core"
)

type flatFee struct{ perContract int64 }

func (f flatFee) Fee(_ string, _ core.Price, qty core.Qty, _ bool) int64 {
	return f.perContract * int64(qty) / 100
}

func testBM() *book.Manager {
	var m config.Market
	m.Canonical = "MKT"
	m.Venues.Kalshi.Ticker = "K"
	m.Venues.PolymarketUS.Slug = "P"
	return book.NewManager([]config.Market{m})
}

func TestPaperFillProfit(t *testing.T) {
	bm := testBM()
	// Kalshi ask 0.50 (100 contracts), Poly bid 0.60 (100) -> 10c gross.
	bm.Update(core.BookSnapshot{Venue: "kalshi", VenueID: "K", YesAsks: []core.Level{{Px: 5000, Qty: 10000}}})
	bm.Update(core.BookSnapshot{Venue: "polymarketus", VenueID: "P", YesBids: []core.Level{{Px: 6000, Qty: 10000}}})

	pt := NewPaperTrader(flatFee{perContract: 50}, 0, bm, "") // 0.5c/contract fee, 0 latency
	pt.Submit("MKT", []core.Intent{
		{Venue: "kalshi", VenueID: "K", Side: core.BuyYes, Px: 5000, Qty: 10000},
		{Venue: "polymarketus", VenueID: "P", Side: core.SellYes, Px: 6000, Qty: 10000},
	})
	pt.settleDue()

	s := pt.Snapshot()
	if s.Completed != 1 {
		t.Fatalf("completed = %d; want 1", s.Completed)
	}
	// gross = 10c * 100 contracts = $10; fees = 2 * 0.5c * 100 = $1; net = $9.
	if s.NetUSD < 8.9 || s.NetUSD > 9.1 {
		t.Errorf("net = %.2f; want ~9.00", s.NetUSD)
	}
}

func TestPaperFadeWhenBookGone(t *testing.T) {
	bm := testBM()
	// Only one venue has a book -> the pair can't fill -> fade.
	bm.Update(core.BookSnapshot{Venue: "kalshi", VenueID: "K", YesAsks: []core.Level{{Px: 5000, Qty: 10000}}})
	bm.Update(core.BookSnapshot{Venue: "polymarketus", VenueID: "P"}) // empty book

	pt := NewPaperTrader(flatFee{}, 0, bm, "")
	pt.Submit("MKT", []core.Intent{
		{Venue: "kalshi", VenueID: "K", Side: core.BuyYes, Px: 5000, Qty: 10000},
		{Venue: "polymarketus", VenueID: "P", Side: core.SellYes, Px: 6000, Qty: 10000},
	})
	pt.settleDue()
	if s := pt.Snapshot(); s.Faded != 1 || s.Completed != 0 {
		t.Errorf("expected 1 fade, got faded=%d completed=%d", s.Faded, s.Completed)
	}
}
