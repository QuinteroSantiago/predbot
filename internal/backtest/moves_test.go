package backtest

import (
	"math"
	"testing"

	"predbot/internal/core"
	"predbot/internal/venue"
)

func mvBook(venueName string, sec int64, bidCents, askCents int) core.BookSnapshot {
	return core.BookSnapshot{
		Venue: venueName, VenueID: "M",
		YesBids: []core.Level{{Px: core.Price(bidCents * 100), Qty: 1000}},
		YesAsks: []core.Level{{Px: core.Price(askCents * 100), Qty: 1000}},
		TsRecv:  sec * 1e9, State: core.StateOpen,
	}
}

// A 10¢ jump that half reverts, worked by hand. With a fee-free venue the P&L
// is pure spread: follow buys the ask and sells the later bid; fade sells the
// bid and buys back the later ask.
func TestMoveStudyWorkedExample(t *testing.T) {
	near := func(what string, got, want float64) {
		t.Helper()
		if math.Abs(got-want) > 1e-9 {
			t.Errorf("%s = %v, want %v", what, got, want)
		}
	}
	for _, v := range []string{"testvenue", "kalshi"} {
		ms := NewMoveStudy(Fees{KalshiMult: 0.07}, map[string]int{v + "|M": 0})
		ms.ObserveBook(mvBook(v, 0, 49, 51))   // mid 50
		ms.ObserveBook(mvBook(v, 30, 49, 51))  // mid 50
		ms.ObserveBook(mvBook(v, 60, 59, 61))  // mid 60: +10¢ in 60s -> event, enter at 59/61
		ms.ObserveBook(mvBook(v, 90, 54, 56))  // 30s later, mid 55 (a +5¢ move too, but in cooldown)
		ms.ObserveBook(mvBook(v, 180, 51, 53)) // 2m later, mid 52 (−3¢ vs 60s ago: no new event)
		r := ms.Result()
		if r.MoveEvents != 1 {
			t.Fatalf("%s: %d move events, want 1", v, r.MoveEvents)
		}
		got := map[int64]MoveRow{}
		for _, row := range r.Rows {
			if row.Bucket != "10–20¢" {
				t.Fatalf("%s: bucket %q, want 10–20¢", v, row.Bucket)
			}
			got[row.HorizonS] = row
		}
		if len(got) != 3 {
			t.Fatalf("%s: horizons %v, want 30s, 2m and settle (10m/30m ran past the data)", v, got)
		}
		fee := func(c int) float64 { return ms.feeCents(v, core.Price(c*100)) }
		near(v+" 30s cont", got[30].Cont, -5)
		near(v+" 30s follow", got[30].Follow, 54-61-fee(61)-fee(54))
		near(v+" 30s fade", got[30].Fade, 59-56-fee(59)-fee(56))
		near(v+" 2m cont", got[120].Cont, -8)
		near(v+" 2m follow", got[120].Follow, 51-61-fee(61)-fee(51))
		near(v+" 2m fade", got[120].Fade, 59-53-fee(59)-fee(53))
		// Resolved NO: the long pays 0, the short keeps the bid.
		near(v+" settle cont", got[0].Cont, -60)
		near(v+" settle follow", got[0].Follow, 0-61-fee(61))
		near(v+" settle fade", got[0].Fade, 59-0-fee(59))
	}
	// Kalshi's fee is 0.07·p·(1−p) per contract, ceiled to $0.0001: 1.67¢ at
	// 61¢ and 1.74¢ at 54¢.
	ms := NewMoveStudy(Fees{KalshiMult: 0.07}, nil)
	near("kalshi fee at 61¢", ms.feeCents("kalshi", 6100), 1.67)
	near("kalshi fee at 54¢", ms.feeCents("kalshi", 5400), 1.74)
}

func TestMoveStudyFlowTrigger(t *testing.T) {
	ms := NewMoveStudy(Fees{}, nil)
	ms.ObserveBook(mvBook("testvenue", 0, 49, 51))
	buy := func(sec int64, contracts int64) venue.Trade {
		return venue.Trade{Venue: "testvenue", VenueID: "M", Px: 5100, Qty: core.Qty(contracts * 100), TakerSide: core.BuyYes, TsRecv: sec * 1e9}
	}
	ms.ObserveTrade(buy(10, 15))
	if r := ms.Result(); r.FlowEvents != 0 {
		t.Fatalf("15 contracts triggered; the floor is 20")
	}
	ms.ObserveTrade(buy(40, 15)) // 30 within 60s
	ms.ObserveTrade(buy(50, 15)) // still inside the cooldown
	if r := ms.Result(); r.FlowEvents != 1 {
		t.Fatalf("%d flow events, want 1", r.FlowEvents)
	}
}

func TestClusteredMean(t *testing.T) {
	m, se := clusteredMean([]float64{1, 3}, []string{"a", "b"})
	if m != 2 || math.Abs(se-1) > 1e-12 {
		t.Errorf("two clusters: mean %v se %v, want 2 and 1", m, se)
	}
	// Ten copies of one game's result are still one observation.
	xs, gs := make([]float64, 10), make([]string, 10)
	for i := range xs {
		xs[i], gs[i] = float64(i), "only"
	}
	if _, se := clusteredMean(xs, gs); se != 0 {
		t.Errorf("one cluster: se %v, want 0 (no honest error)", se)
	}
}
