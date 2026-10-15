package book

import (
	"testing"

	"predbot/internal/config"
	"predbot/internal/core"
)

func testMarkets() []config.Market {
	var m config.Market
	m.Canonical = "MKT"
	m.Venues.Kalshi.Ticker = "KT"
	m.Venues.PolymarketUS.Slug = "ps"
	return []config.Market{m}
}

func TestUpdateAndPair(t *testing.T) {
	m := NewManager(testMarkets())

	// Unknown venue id is refused (design §12).
	if _, ok := m.Update(core.BookSnapshot{Venue: "kalshi", VenueID: "UNKNOWN"}); ok {
		t.Error("unknown venue id should be refused")
	}

	// One venue present -> pair not ready.
	if c, ok := m.Update(core.BookSnapshot{Venue: "kalshi", VenueID: "KT"}); !ok || c != "MKT" {
		t.Fatalf("kalshi update: c=%q ok=%v", c, ok)
	}
	if _, ready := m.Pair("MKT"); ready {
		t.Error("pair should not be ready with only one venue")
	}

	// Both venues present -> ready.
	m.Update(core.BookSnapshot{Venue: "polymarketus", VenueID: "ps"})
	books, ready := m.Pair("MKT")
	if !ready || len(books) != 2 {
		t.Fatalf("pair should be ready with 2 venues, got %d ready=%v", len(books), ready)
	}
}
