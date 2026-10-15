package backtest

import (
	"time"

	"predbot/internal/book"
	"predbot/internal/core"
	"predbot/internal/strategy"
)

// Sim is the SimExecutor (design §10): it takes strategy Intents and matches
// them against the recorded book at t+Δ, so fade is *measured* not modeled — if
// the level is gone at t+Δ, the fill didn't happen (§10.2). Deterministic: it is
// driven entirely by the replay clock, never wall-clock.
type Sim struct {
	fees    strategy.FeeModel
	latency int64 // fill delay in ns (§10.2, default 150ms)
	rep     *Report
	pending []pendingPair
}

type pendingPair struct {
	canonical string
	legs      []core.Intent
	at        int64 // fill time = submit time + latency
}

// NewSim builds a SimExecutor. latencyMS is the design-§10.2 latency Δ.
func NewSim(fees strategy.FeeModel, latencyMS int, rep *Report) *Sim {
	return &Sim{fees: fees, latency: int64(latencyMS) * int64(time.Millisecond), rep: rep}
}

// Submit queues a pair (or single intent) for fill at now+Δ.
func (s *Sim) Submit(canonical string, legs []core.Intent, now int64) {
	if len(legs) == 0 {
		return
	}
	s.rep.PairsAttempted++
	s.rep.byCanonical[canonical]++
	s.pending = append(s.pending, pendingPair{canonical, legs, now + s.latency})
}

// Advance settles every pending pair whose fill time has arrived, matching
// against the current books in bm. Call once per replay tick, then Flush at end.
func (s *Sim) Advance(now int64, bm *book.Manager) {
	kept := s.pending[:0]
	for _, p := range s.pending {
		if p.at > now {
			kept = append(kept, p)
			continue
		}
		s.settle(p, bm)
	}
	s.pending = kept
}

// Flush settles anything still pending at end of replay.
func (s *Sim) Flush(bm *book.Manager) {
	for _, p := range s.pending {
		s.settle(p, bm)
	}
	s.pending = nil
}

// settle evaluates a two-leg pair against the book at fill time. A single-leg
// intent (intra-venue marker) is only counted.
func (s *Sim) settle(p pendingPair, bm *book.Manager) {
	if len(p.legs) == 1 {
		s.rep.IntraCrossings++
		return
	}
	books, ok := bm.Pair(p.canonical)
	if !ok {
		s.rep.PairsFaded++
		return
	}
	buy, sell := p.legs[0], p.legs[1]
	buyPx, buyQty := fillBuy(books[buy.Venue], buy.Px, buy.Qty)
	sellPx, sellQty := fillSell(books[sell.Venue], sell.Px, sell.Qty)

	if buyQty == 0 || sellQty == 0 {
		s.rep.PairsFaded++ // opportunity vanished within Δ
		return
	}

	hedged := buyQty
	if sellQty < hedged {
		hedged = sellQty
	}
	c := hedged.Contracts()

	// Gross uses realized fill prices, so it already reflects slippage.
	gross := int64(float64(int64(sellPx)-int64(buyPx)) * c)
	feeBuy := s.fees.Fee(buy.Venue, buyPx, hedged, false)
	feeSell := s.fees.Fee(sell.Venue, sellPx, hedged, false)
	s.rep.FeesByVenue[buy.Venue] += feeBuy
	s.rep.FeesByVenue[sell.Venue] += feeSell

	// Slippage (informational): realized fill worse than the intended limit.
	s.rep.Slippage += int64(float64(int64(buyPx)-int64(buy.Px))*c) +
		int64(float64(int64(sell.Px)-int64(sellPx))*c)

	// Legging: the unhedged excess is exited on its own venue at the opposite
	// top-of-book (design §8 "sell it"). A first-cut cost model.
	legCost := s.legging(buy, sell, buyQty, sellQty, books)
	s.rep.LeggingCost += legCost

	// Capital locked (§10.4): BuyYes at p costs p·qty, SellYes at p costs (1−p)·qty.
	capital := int64(float64(buyPx)*c) + int64(float64(int64(core.OneDollar)-int64(sellPx))*c)
	net := gross - feeBuy - feeSell - legCost

	s.rep.CapitalLocked += capital
	s.rep.GrossCaptured += gross
	s.rep.NetPnL += net
	s.rep.addPair(p.canonical, net, capital, p.at,
		legCapital{buy.Venue, int64(float64(buyPx) * c)},
		legCapital{sell.Venue, int64(float64(int64(core.OneDollar)-int64(sellPx)) * c)})
	if buyQty == sellQty {
		s.rep.PairsCompleted++
	} else {
		s.rep.PairsLegged++
	}
}

// legging estimates the round-trip cost of exiting the unhedged excess at the
// opposite top-of-book on the over-filled leg's venue.
func (s *Sim) legging(buy, sell core.Intent, buyQty, sellQty core.Qty, books map[string]core.BookSnapshot) int64 {
	switch diff := buyQty - sellQty; {
	case diff > 0: // over-bought YES on the buy venue
		return s.exitCost(buy, diff, books[buy.Venue])
	case diff < 0: // over-sold YES on the sell venue
		return s.exitCost(sell, -diff, books[sell.Venue])
	}
	return 0
}

// exitCost is what it costs to get out of qty of an unhedged leg entered at
// leg.Px: dumped on its own venue at the opposite top of book (sell a long at
// the bid, buy back a short at the ask), as the live trader does when a hedge
// fails, plus that exit's taker fee. The round trip is charged as a loss
// whichever way the price moved. An empty exit side charges nothing — the
// position is then simply held, which this first-cut model does not price.
func (s *Sim) exitCost(leg core.Intent, qty core.Qty, b core.BookSnapshot) int64 {
	if qty <= 0 {
		return 0
	}
	exit, ok := b.BestYesBid()
	if leg.Side == core.SellYes {
		exit, ok = b.BestYesAsk()
	}
	if !ok {
		return 0
	}
	loss := int64(exit.Px) - int64(leg.Px)
	if loss < 0 {
		loss = -loss
	}
	// The exit fee stays inside the legging line (not FeesByVenue), so the
	// report's waterfall gross − fees − legging = net counts it once.
	fee := s.fees.Fee(leg.Venue, exit.Px, qty, false)
	return core.Notional(core.Price(loss), qty) + fee
}

// fillBuy walks YES asks (ascending) taking levels priced <= limit, up to qty.
// Returns the volume-weighted average fill price and the filled quantity.
func fillBuy(b core.BookSnapshot, limit core.Price, qty core.Qty) (core.Price, core.Qty) {
	return walk(b.YesAsks, qty, func(px core.Price) bool { return px <= limit })
}

// fillSell walks YES bids (descending) taking levels priced >= limit.
func fillSell(b core.BookSnapshot, limit core.Price, qty core.Qty) (core.Price, core.Qty) {
	return walk(b.YesBids, qty, func(px core.Price) bool { return px >= limit })
}

func walk(levels []core.Level, want core.Qty, ok func(core.Price) bool) (core.Price, core.Qty) {
	var filled core.Qty
	var notional int64 // px * qty accumulator
	for _, lv := range levels {
		if filled >= want || !ok(lv.Px) {
			break
		}
		take := lv.Qty
		if filled+take > want {
			take = want - filled
		}
		notional += int64(lv.Px) * int64(take)
		filled += take
	}
	if filled == 0 {
		return 0, 0
	}
	return core.Price(notional / int64(filled)), filled
}

// SettleMakerPair books a maker/taker pair once the RESTING leg has actually
// filled (as determined by RestingFill observing a genuine cross).
//
// The economics differ from a taker/taker pair in two ways that matter:
//   - the rested leg fills AT ITS RESTING PRICE (no slippage — the market came
//     to us) and pays the MAKER fee, which on Polymarket is a REBATE;
//   - the taking leg still crosses the book and pays the taker fee.
//
// restedQty is what actually filled on the rest; the taking leg is sized to it.
// now is the fill time, which dates the pair for the hold-to-resolution stats.
// If the taking leg cannot fill, the pair is legged and the rested position is
// left unhedged — counted, not hidden.
func (s *Sim) SettleMakerPair(canonical string, rested, taking core.Intent, restedQty core.Qty, now int64, bm *book.Manager) {
	books, ok := bm.Pair(canonical)
	if !ok || restedQty == 0 {
		s.rep.PairsFaded++
		return
	}
	s.rep.PairsAttempted++
	s.rep.byCanonical[canonical]++

	// Take the other side, sized to the rest's fill.
	var takePx core.Price
	var takeQty core.Qty
	if taking.Side == core.BuyYes {
		takePx, takeQty = fillBuy(books[taking.Venue], taking.Px, restedQty)
	} else {
		takePx, takeQty = fillSell(books[taking.Venue], taking.Px, restedQty)
	}
	if takeQty == 0 {
		// Rest filled but the hedge is gone: an unhedged maker position, dumped
		// as the live trader does. Until 2026-09-24 this booked $0 and dropped
		// the position, so 405 legged rests cost nothing in the report.
		legCost := s.exitCost(rested, restedQty, books[rested.Venue])
		s.rep.LeggingCost += legCost
		s.rep.NetPnL -= legCost
		s.rep.PairsLegged++
		s.rep.addPair(canonical, -legCost, 0, now)
		return
	}
	hedged := restedQty
	if takeQty < hedged {
		hedged = takeQty
	}
	c := hedged.Contracts()

	// Determine buy/sell prices for the gross calculation.
	buyPx, sellPx := rested.Px, takePx
	buyVenue, sellVenue := rested.Venue, taking.Venue
	buyMaker := true
	if rested.Side == core.SellYes {
		buyPx, sellPx = takePx, rested.Px
		buyVenue, sellVenue = taking.Venue, rested.Venue
		buyMaker = false
	}

	gross := int64(float64(int64(sellPx)-int64(buyPx)) * c)
	feeBuy := s.fees.Fee(buyVenue, buyPx, hedged, buyMaker)
	feeSell := s.fees.Fee(sellVenue, sellPx, hedged, !buyMaker)
	s.rep.FeesByVenue[buyVenue] += feeBuy
	s.rep.FeesByVenue[sellVenue] += feeSell

	capital := int64(float64(buyPx)*c) + int64(float64(int64(core.OneDollar)-int64(sellPx))*c)
	// A partial hedge leaves the rest's remainder naked: dump it too.
	legCost := s.exitCost(rested, restedQty-hedged, books[rested.Venue])
	s.rep.LeggingCost += legCost
	net := gross - feeBuy - feeSell - legCost

	s.rep.CapitalLocked += capital
	s.rep.GrossCaptured += gross
	s.rep.NetPnL += net
	s.rep.addPair(canonical, net, capital, now,
		legCapital{buyVenue, int64(float64(buyPx) * c)},
		legCapital{sellVenue, int64(float64(int64(core.OneDollar)-int64(sellPx)) * c)})
	if takeQty >= restedQty {
		s.rep.PairsCompleted++
	} else {
		s.rep.PairsLegged++
	}
}
