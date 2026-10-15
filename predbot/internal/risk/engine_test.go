package risk

import (
	"errors"
	"path/filepath"
	"testing"

	"predbot/internal/core"
)

func cfg() Config {
	return Config{
		MaxOrderQty:   5000,  // 50 contracts
		DefaultMaxPos: 10000, // 100 contracts
		MinBP:         map[string]int64{"kalshi": 0, "polymarketus": 0},
		MaxUnhedged:   20000, // 200 contracts
	}
}

func buy(qty core.Qty) core.Intent {
	return core.Intent{Venue: "kalshi", Side: core.BuyYes, Px: 5000, Qty: qty}
}

func TestCheckPassesAndReserves(t *testing.T) {
	e := New(cfg(), map[string]int64{"kalshi": 1_000_000}, "")
	if err := e.CheckAndReserve("MKT", buy(1000)); err != nil {
		t.Fatalf("valid order rejected: %v", err)
	}
	// buying power reduced by p·qty = 0.50 * 10 contracts = $5.00 = 50000 units.
	if got := e.bp["kalshi"]; got != 1_000_000-50000 {
		t.Errorf("buying power = %d; want %d", got, 1_000_000-50000)
	}
}

func TestRejections(t *testing.T) {
	cases := []struct {
		name string
		bp   int64
		in   core.Intent
		want error
	}{
		{"too big", 1e9, buy(6000), ErrOrderTooBig},
		{"zero qty", 1e9, buy(0), ErrOrderTooBig},
		{"price >= $1", 1e9, core.Intent{Venue: "kalshi", Side: core.BuyYes, Px: 10000, Qty: 100}, ErrPriceSanity},
		{"price <= 0", 1e9, core.Intent{Venue: "kalshi", Side: core.BuyYes, Px: 0, Qty: 100}, ErrPriceSanity},
		{"no buying power", 100, buy(1000), ErrBuyingPower},
	}
	for _, c := range cases {
		e := New(cfg(), map[string]int64{"kalshi": c.bp}, "")
		if err := e.CheckAndReserve("MKT", c.in); err != c.want {
			t.Errorf("%s: got %v, want %v", c.name, err, c.want)
		}
	}
}

func TestPositionCapCountsInFlight(t *testing.T) {
	// Cap is 100 contracts. 40-contract orders (4000 units, under the 50 max):
	// two reserve 80 in-flight; the third would reach 120 > 100 -> rejected.
	e := New(cfg(), map[string]int64{"kalshi": 1e9}, "")
	if err := e.CheckAndReserve("MKT", buy(4000)); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := e.CheckAndReserve("MKT", buy(4000)); err != nil {
		t.Fatalf("second: %v", err)
	}
	if err := e.CheckAndReserve("MKT", buy(4000)); err != ErrPositionCap {
		t.Errorf("third order should hit position cap (would be 120 > 100), got %v", err)
	}
}

func TestKillSwitchBlocksEverything(t *testing.T) {
	e := New(cfg(), map[string]int64{"kalshi": 1e9}, "")
	e.Kill("test")
	if !e.Killed() {
		t.Error("should be killed")
	}
	if err := e.CheckAndReserve("MKT", buy(100)); err != ErrKilled {
		t.Errorf("killed engine must reject, got %v", err)
	}
}

func TestKillMarkerPersists(t *testing.T) {
	f := filepath.Join(t.TempDir(), "KILLED")
	e := New(cfg(), nil, f)
	e.Kill("boom")
	// A fresh engine with the same marker file starts killed (survives restart).
	e2 := New(cfg(), nil, f)
	if !e2.Killed() {
		t.Error("kill marker must keep a restarted engine killed")
	}
}

func TestFillMovesInFlightToPosition(t *testing.T) {
	e := New(cfg(), map[string]int64{"kalshi": 1e9}, "")
	i := buy(4000)
	e.CheckAndReserve("MKT", i)
	if e.inflight["MKT"] != 4000 {
		t.Fatalf("in-flight = %d; want 4000", e.inflight["MKT"])
	}
	e.OnFill("MKT", i, 4000)
	if e.inflight["MKT"] != 0 || e.Position("MKT") != 4000 {
		t.Errorf("after fill: inflight=%d pos=%d; want 0/4000", e.inflight["MKT"], e.Position("MKT"))
	}
}

func TestRejectReleasesReservation(t *testing.T) {
	e := New(cfg(), map[string]int64{"kalshi": 1_000_000}, "")
	i := buy(4000)
	e.CheckAndReserve("MKT", i)
	e.OnReject("MKT", i)
	if e.inflight["MKT"] != 0 || e.bp["kalshi"] != 1_000_000 {
		t.Errorf("reject must restore inflight+bp: inflight=%d bp=%d", e.inflight["MKT"], e.bp["kalshi"])
	}
}

// A hedged pair nets to zero in YES terms, so it consumes NO position cap —
// whether this process opened it or adopted it at startup. Pinned because the
// distinction drives what adoption has to seed.
func TestHedgedPairConsumesNoPositionCap(t *testing.T) {
	e := New(Config{MaxOrderQty: 1000, DefaultMaxPos: 500,
		MinBP: map[string]int64{"kalshi": 0, "polymarketus": 0}, MaxUnhedged: 100000},
		map[string]int64{"kalshi": 1e9, "polymarketus": 1e9}, "")

	buy := core.Intent{Venue: "kalshi", VenueID: "K", Side: core.BuyYes, Px: 5000, Qty: 500, TIF: core.IOC}
	sell := core.Intent{Venue: "polymarketus", VenueID: "P", Side: core.SellYes, Px: 6000, Qty: 500, TIF: core.IOC}
	for _, i := range []core.Intent{buy, sell} {
		if err := e.CheckAndReserve("MKT", i); err != nil {
			t.Fatalf("reserve %v: %v", i.Side, err)
		}
		e.OnFill("MKT", i, i.Qty)
	}
	if got := e.Position("MKT"); got != 0 {
		t.Fatalf("hedged pair left position %v; want 0", got)
	}
}

// An adopted UNBALANCED position must consume the cap. Without seeding, the
// engine reads live directional risk as flat and allows a full position on top.
func TestAdoptedNakedPositionConsumesTheCap(t *testing.T) {
	e := New(Config{MaxOrderQty: 1000, DefaultMaxPos: 500,
		MinBP: map[string]int64{"kalshi": 0}, MaxUnhedged: 100000},
		map[string]int64{"kalshi": 1e9}, "")

	e.AdoptPosition("MKT", 500) // long 5 YES, adopted at startup, unhedged

	if got := e.Position("MKT"); got != 500 {
		t.Fatalf("adopted position = %v; want 500", got)
	}
	// The cap is 500 and it is already fully used: buying more must be refused.
	err := e.CheckAndReserve("MKT", core.Intent{Venue: "kalshi", VenueID: "K",
		Side: core.BuyYes, Px: 5000, Qty: 500, TIF: core.IOC})
	if !errors.Is(err, ErrPositionCap) {
		t.Fatalf("want ErrPositionCap on top of an adopted position, got %v", err)
	}
	// Reducing it must still be allowed.
	if err := e.CheckAndReserve("MKT", core.Intent{Venue: "kalshi", VenueID: "K",
		Side: core.SellYes, Px: 5000, Qty: 500, TIF: core.IOC}); err != nil {
		t.Errorf("selling down an adopted position must be allowed, got %v", err)
	}
}
