package strategy

import "predbot/internal/core"

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
// The resting leg is placed at the current best price on its side (joining the
// touch), and the other leg is taken IOC once the rest fills.
func MakerCrossVenue(canonical string, books map[string]core.BookSnapshot, exp Exposure, fees FeeModel, cfg Config) []core.Intent {
	if exp.InFlight[canonical] {
		return nil
	}
	type vb struct {
		venue string
		book  core.BookSnapshot
	}
	var vs []vb
	for v, b := range books {
		vs = append(vs, vb{v, b})
	}

	var best []core.Intent
	var bestNet int64
	for i := range vs {
		for j := range vs {
			if i == j {
				continue
			}
			buy, sell := vs[i], vs[j]
			ask, okA := buy.book.BestYesAsk()
			bid, okB := sell.book.BestYesBid()
			if !okA || !okB {
				continue
			}
			if cfg.MinDepth > 0 && (ask.Qty < cfg.MinDepth || bid.Qty < cfg.MinDepth) {
				continue
			}
			gross := int64(bid.Px) - int64(ask.Px)
			if gross <= 0 {
				continue
			}
			qty := minQty(ask.Qty, bid.Qty, cfg.MaxClip)
			if qty <= 0 {
				continue
			}
			c := contracts(qty)
			grossTotal := int64(float64(gross) * c)

			// Try resting EACH leg and take the better of the two. The rested
			// leg pays the maker fee (a rebate on Polymarket); the other pays taker.
			for _, restBuy := range []bool{true, false} {
				feeBuy := fees.Fee(buy.venue, ask.Px, qty, restBuy)
				feeSell := fees.Fee(sell.venue, bid.Px, qty, !restBuy)
				net := grossTotal - feeBuy - feeSell
				if net <= 0 || int64(float64(net)/c) < int64(cfg.MinEdge) {
					continue
				}
				if net <= bestNet {
					continue
				}
				bestNet = net
				pairID := canonical + ":maker:" + buy.venue + "->" + sell.venue
				legs := []core.Intent{
					{PairID: pairID, Leg: 0, Venue: buy.venue, VenueID: buy.book.VenueID,
						Side: core.BuyYes, Px: ask.Px, Qty: qty, Reason: "maker buy"},
					{PairID: pairID, Leg: 1, Venue: sell.venue, VenueID: sell.book.VenueID,
						Side: core.SellYes, Px: bid.Px, Qty: qty, Reason: "maker sell"},
				}
				// The resting leg is GTC (it waits); the taking leg is IOC.
				if restBuy {
					legs[0].TIF, legs[1].TIF = core.GTC, core.IOC
					legs[0].Reason, legs[1].Reason = "REST buy", "take sell"
				} else {
					legs[0].TIF, legs[1].TIF = core.IOC, core.GTC
					legs[0].Reason, legs[1].Reason = "take buy", "REST sell"
				}
				best = legs
			}
		}
	}
	return best
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
