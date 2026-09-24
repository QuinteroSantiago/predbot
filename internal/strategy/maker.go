package strategy

import (
	"sort"

	"predbot/internal/core"
)

// MakerCrossVenue looks for pairs that are unprofitable as taker/taker but
// profitable when ONE leg rests as a maker (design §9.2 "Maker variants").
//
// Why this matters: the binding constraint on this bot has never been speed, it
// is fees. A taker/taker round trip costs ~3.49c per contract at p=0.5, and
// almost nothing clears that. Resting the Polymarket leg EARNS a rebate
// (Θ=−0.0125) instead of paying Θ=0.0695, which drops the hurdle to ~1.44c —
// measured on recorded data, that turns 1 opportunity into 120.
//
// THE CATCH, and it is a big one: a resting order only earns if it FILLS, and it
// fills precisely when someone chooses to cross to it — which is disproportionately
// when the price is about to move against you (adverse selection). This is
// market making, not arbitrage: the risk profile is different, and the
// backtester must model fill probability honestly rather than assuming a rest
// always fills. See backtest.RestingFill.
//
// The resting leg JOINS the touch on its own side of its own book — a rested
// buy at that venue's best bid, a rested sell at its best ask — so it waits on
// the book and earns the maker rebate. The other leg is taken IOC at the other
// venue's touch once the rest fills. So the two maker shapes are:
//
//	REST buy on R @ R.bid, TAKE sell on T @ T.bid:  gross = T.bid − R.bid
//	REST sell on R @ R.ask, TAKE buy on T @ T.ask:  gross = R.ask − T.ask
//
// Until 2026-09-24 the rest was priced at the OTHER side of its book (a rested
// sell at the best bid), which crosses: every "rest" filled instantly as a
// taker while the edge was computed with the maker rebate, and the backtest,
// which uses this function, credited the same phantom rebate.
func MakerCrossVenue(canonical string, books map[string]core.BookSnapshot, exp Exposure, fees FeeModel, cfg Config) []core.Intent {
	if exp.InFlight[canonical] {
		return nil
	}
	var venues []string
	for v := range books {
		venues = append(venues, v)
	}
	sort.Strings(venues) // ties on net must pick the same pair every run

	var best []core.Intent
	var bestNet int64
	consider := func(rest, take core.Intent, takeDepth core.Qty) {
		if cfg.MinDepth > 0 && takeDepth < cfg.MinDepth {
			return // the hedge must be able to clear when the rest fills
		}
		qty := takeDepth
		if cfg.MaxClip > 0 && qty > cfg.MaxClip {
			qty = cfg.MaxClip
		}
		if qty <= 0 {
			return
		}
		buy, sell := rest, take
		if rest.Side == core.SellYes {
			buy, sell = take, rest
		}
		gross := int64(sell.Px) - int64(buy.Px)
		if gross <= 0 {
			return
		}
		c := contracts(qty)
		net := int64(float64(gross)*c) -
			fees.Fee(rest.Venue, rest.Px, qty, true) - fees.Fee(take.Venue, take.Px, qty, false)
		if net <= 0 || int64(float64(net)/c) < int64(cfg.MinEdge) || net <= bestNet {
			return
		}
		bestNet = net
		pairID := canonical + ":maker:" + buy.Venue + "->" + sell.Venue
		buy.PairID, buy.Leg, buy.Qty = pairID, 0, qty
		sell.PairID, sell.Leg, sell.Qty = pairID, 1, qty
		best = []core.Intent{buy, sell}
	}

	for _, r := range venues {
		for _, t := range venues {
			if r == t {
				continue
			}
			rb, tb := books[r], books[t]
			// REST buy on r at its bid; TAKE sell on t at its bid.
			if rBid, ok := rb.BestYesBid(); ok {
				if tBid, ok := tb.BestYesBid(); ok {
					consider(
						core.Intent{Venue: r, VenueID: rb.VenueID, Side: core.BuyYes, Px: rBid.Px, TIF: core.GTC, Reason: "REST buy"},
						core.Intent{Venue: t, VenueID: tb.VenueID, Side: core.SellYes, Px: tBid.Px, TIF: core.IOC, Reason: "take sell"},
						tBid.Qty)
				}
			}
			// REST sell on r at its ask; TAKE buy on t at its ask.
			if rAsk, ok := rb.BestYesAsk(); ok {
				if tAsk, ok := tb.BestYesAsk(); ok {
					consider(
						core.Intent{Venue: r, VenueID: rb.VenueID, Side: core.SellYes, Px: rAsk.Px, TIF: core.GTC, Reason: "REST sell"},
						core.Intent{Venue: t, VenueID: tb.VenueID, Side: core.BuyYes, Px: tAsk.Px, TIF: core.IOC, Reason: "take buy"},
						tAsk.Qty)
				}
			}
		}
	}
	return best
}

// RestCrosses reports whether a resting leg's price would cross its own book
// (a buy at or above the best ask, a sell at or below the best bid) — i.e. it
// would fill immediately as a taker instead of resting.
func RestCrosses(rest core.Intent, b core.BookSnapshot) bool {
	if rest.Side == core.BuyYes {
		ask, ok := b.BestYesAsk()
		return ok && rest.Px >= ask.Px
	}
	bid, ok := b.BestYesBid()
	return ok && rest.Px <= bid.Px
}

// RestingLeg returns the index of the leg that rests (TIF GTC), or -1 if this is
// a plain taker/taker pair.
func RestingLeg(legs []core.Intent) int {
	for i, l := range legs {
		if l.TIF == core.GTC {
			return i
		}
	}
	return -1
}
