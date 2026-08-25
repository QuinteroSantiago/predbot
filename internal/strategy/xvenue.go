package strategy

import "predbot/internal/core"

// CrossVenue detects a cross-venue arbitrage (design §9.1): buy YES cheap on one
// venue, sell YES dear on the other, when the gap clears both taker fees.
//
//	edge_AB = BestYesBid(B) − BestYesAsk(A) − fee(A,taker) − fee(B,taker)
//
// It returns a two-leg pair (BuyYes on the cheap venue, SellYes on the dear one)
// sized to the min of the two top-level depths and MaxClip. Detect-only in
// Phase 2: the caller feeds these to the SimExecutor, not a real venue.
func CrossVenue(canonical string, books map[string]core.BookSnapshot, exp Exposure, fees FeeModel, cfg Config) []core.Intent {
	if exp.InFlight[canonical] {
		return nil
	}
	// Consider every ordered pair of venues (both directions).
	type venueBook struct {
		venue string
		book  core.BookSnapshot
	}
	vs := make([]venueBook, 0, len(books))
	for v, b := range books {
		vs = append(vs, venueBook{v, b})
	}
	var best []core.Intent
	var bestNet int64
	for i := range vs {
		for j := range vs {
			if i == j {
				continue
			}
			buy, sell := vs[i], vs[j] // buy YES on i, sell YES on j
			ask, okA := buy.book.BestYesAsk()
			bid, okB := sell.book.BestYesBid()
			if !okA || !okB {
				continue
			}
			// BOTH legs must have real depth. A quote with a token size on one
			// venue is not a hedgeable opportunity — filling leg 1 against it
			// leaves a naked position when leg 2 can't be filled.
			if cfg.MinDepth > 0 && (ask.Qty < cfg.MinDepth || bid.Qty < cfg.MinDepth) {
				continue
			}
			gross := int64(bid.Px) - int64(ask.Px) // per-contract, $0.0001
			if gross <= 0 {
				continue
			}
			qty := minQty(ask.Qty, bid.Qty, cfg.MaxClip)
			if qty <= 0 {
				continue
			}
			c := contracts(qty)
			grossTotal := int64(float64(gross) * c)
			feeBuy := fees.Fee(buy.venue, ask.Px, qty, false)
			feeSell := fees.Fee(sell.venue, bid.Px, qty, false)
			net := grossTotal - feeBuy - feeSell
			// Require the per-contract net edge to clear MinEdge.
			if net <= 0 || int64(float64(net)/c) < int64(cfg.MinEdge) {
				continue
			}
			if net > bestNet {
				bestNet = net
				pairID := canonical + ":" + buy.venue + "->" + sell.venue
				best = []core.Intent{
					{PairID: pairID, Leg: 0, Venue: buy.venue, VenueID: buy.book.VenueID,
						Side: core.BuyYes, Px: ask.Px, Qty: qty, TIF: core.IOC,
						Reason: "xvenue buy"},
					{PairID: pairID, Leg: 1, Venue: sell.venue, VenueID: sell.book.VenueID,
						Side: core.SellYes, Px: bid.Px, Qty: qty, TIF: core.IOC,
						Reason: "xvenue sell"},
				}
			}
		}
	}
	return best
}

func minQty(a, b, c core.Qty) core.Qty {
	m := a
	if b < m {
		m = b
	}
	if c > 0 && c < m {
		m = c
	}
	return m
}
