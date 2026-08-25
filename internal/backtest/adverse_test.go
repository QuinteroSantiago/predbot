package backtest

import (
	"strings"
	"testing"

	"predbot/internal/core"
	"predbot/internal/venue"
)

func snap(ts int64, bid, ask core.Price) core.BookSnapshot {
	return core.BookSnapshot{TsRecv: ts,
		YesBids: []core.Level{{Px: bid, Qty: 1000}},
		YesAsks: []core.Level{{Px: ask, Qty: 1000}}}
}

const sec = int64(1_000_000_000)

// A taker who BUYS means the maker was filled SHORT. If the price then RISES,
// that hurts the maker — positive adverse drift.
func TestAdverseDriftAgainstMaker(t *testing.T) {
	a := NewAdverseSelection(30)
	a.ObserveBook("MKT", snap(0, 5000, 5100)) // mid 50.5c
	a.ObserveTrade("MKT", venue.Trade{TakerSide: core.BuyYes, TsRecv: 1 * sec})
	a.ObserveBook("MKT", snap(35*sec, 5400, 5500)) // mid rose to 54.5c

	rep := a.Report()
	if !strings.Contains(rep, "30s: mean adverse drift +") {
		t.Errorf("maker short + price up should be ADVERSE, got:\n%s", rep)
	}
}

// Same fill, but the price FALLS: good for a maker who was filled short.
func TestFavourableDriftForMaker(t *testing.T) {
	a := NewAdverseSelection(30)
	a.ObserveBook("MKT", snap(0, 5000, 5100))
	a.ObserveTrade("MKT", venue.Trade{TakerSide: core.BuyYes, TsRecv: 1 * sec})
	a.ObserveBook("MKT", snap(35*sec, 4600, 4700)) // mid fell
	if !strings.Contains(a.Report(), "30s: mean adverse drift -") {
		t.Errorf("maker short + price down should be FAVOURABLE:\n%s", a.Report())
	}
}

// The maker side is the OPPOSITE of the taker side — getting this backwards
// would invert every conclusion.
func TestMakerSideIsOppositeTaker(t *testing.T) {
	a := NewAdverseSelection(30)
	a.ObserveBook("MKT", snap(0, 5000, 5100))
	// Taker SELLS => maker was filled LONG. Price falling hurts a long.
	a.ObserveTrade("MKT", venue.Trade{TakerSide: core.SellYes, TsRecv: 1 * sec})
	a.ObserveBook("MKT", snap(35*sec, 4600, 4700))
	if !strings.Contains(a.Report(), "30s: mean adverse drift +") {
		t.Errorf("maker long + price down should be ADVERSE:\n%s", a.Report())
	}
}

// Books alone must produce nothing: fills come only from trade prints.
func TestNoTradesMeansNoSamples(t *testing.T) {
	a := NewAdverseSelection(30)
	a.ObserveBook("MKT", snap(0, 5000, 5100))
	a.ObserveBook("MKT", snap(35*sec, 4000, 4100))
	if !strings.Contains(a.Report(), "no samples yet") {
		t.Errorf("book-only input must yield no fills:\n%s", a.Report())
	}
}
