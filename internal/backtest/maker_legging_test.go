package backtest

import (
	"testing"

	"predbot/internal/book"
	"predbot/internal/config"
	"predbot/internal/core"
)

// A rest that fills with no hedge available is dumped at the opposite touch,
// as the live trader does, and that exit is a loss in the report. Until
// 2026-09-24 it was booked at $0, so the fixed maker run's 405 legged rests
// cost nothing and its net looked flat instead of negative.
func TestMakerLeggedRestIsChargedItsExit(t *testing.T) {
	var m config.Market
	m.Canonical = "MKT"
	m.Venues.Kalshi.Ticker = "K"
	m.Venues.PolymarketUS.Slug = "P"
	bm := book.NewManager([]config.Market{m})
	// Poly now quotes 0.60 / 0.62; Kalshi's ask has moved to 0.70, above the
	// 0.57 the hedge needed, so the hedge can't fill.
	bm.Update(core.BookSnapshot{Venue: "polymarketus", VenueID: "P",
		YesBids: []core.Level{{Px: 6000, Qty: 10000}}, YesAsks: []core.Level{{Px: 6200, Qty: 10000}}})
	bm.Update(core.BookSnapshot{Venue: "kalshi", VenueID: "K",
		YesBids: []core.Level{{Px: 6800, Qty: 10000}}, YesAsks: []core.Level{{Px: 7000, Qty: 10000}}})

	rep := NewReport()
	s := NewSim(Fees{KalshiMult: 0.07}, 0, rep)
	rested := core.Intent{Venue: "polymarketus", VenueID: "P", Side: core.SellYes, Px: 6600, Qty: 1000, TIF: core.GTC}
	taking := core.Intent{Venue: "kalshi", VenueID: "K", Side: core.BuyYes, Px: 5700, Qty: 1000, TIF: core.IOC}
	s.SettleMakerPair("MKT", rested, taking, 1000, 0, bm)

	if rep.PairsLegged != 1 {
		t.Fatalf("legged = %d; want 1", rep.PairsLegged)
	}
	// Short 10 at 0.66, bought back at the 0.62 ask: |0.62−0.66|·10 = $0.40,
	// plus Poly's taker fee on the exit.
	if rep.LeggingCost < 4000 {
		t.Errorf("legging cost = %d; want at least $0.40 (4000) for the dump", rep.LeggingCost)
	}
	if rep.NetPnL != -rep.LeggingCost {
		t.Errorf("net = %d; want -legging %d", rep.NetPnL, -rep.LeggingCost)
	}
}
