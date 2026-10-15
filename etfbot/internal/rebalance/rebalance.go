// Package rebalance turns target weights and current holdings into orders.
// The backtest and the live bot both call Plan, so they trade identically.
package rebalance

import (
	"math"
	"sort"

	"etfbot/internal/market"
)

// Params tune how closely the portfolio tracks its targets.
type Params struct {
	// MinTradeUSD skips adjustments smaller than this, so drift of a few
	// dollars doesn't generate an order every month. Full exits always trade.
	MinTradeUSD float64
	// CashBuffer is the fraction of equity left in cash, so buys still fit
	// when sells fill a little below the price they were planned at.
	CashBuffer float64
}

// Plan returns the orders that move holdings to targets at prices: sells
// first, then buys, each group sorted by symbol. Every held or targeted symbol
// must have a price.
func Plan(holdings map[string]float64, cash float64, prices, targets map[string]float64, p Params) []market.Order {
	equity := cash
	for sym, q := range holdings {
		equity += q * prices[sym]
	}
	invest := equity * (1 - p.CashBuffer)

	syms := map[string]bool{}
	for s, q := range holdings {
		if q != 0 {
			syms[s] = true
		}
	}
	for s, w := range targets {
		if w > 0 {
			syms[s] = true
		}
	}
	var sells, buys []market.Order
	for sym := range syms {
		q, w, px := holdings[sym], targets[sym], prices[sym]
		if w <= 0 {
			sells = append(sells, market.Order{Symbol: sym, Side: market.Sell, Qty: q})
			continue
		}
		diff := w*invest - q*px
		if math.Abs(diff) < p.MinTradeUSD {
			continue
		}
		if diff > 0 {
			buys = append(buys, market.Order{Symbol: sym, Side: market.Buy, Notional: roundCents(diff)})
		} else {
			sells = append(sells, market.Order{Symbol: sym, Side: market.Sell, Qty: math.Min(q, -diff/px)})
		}
	}
	bySym := func(o []market.Order) {
		sort.Slice(o, func(i, j int) bool { return o[i].Symbol < o[j].Symbol })
	}
	bySym(sells)
	bySym(buys)
	return append(sells, buys...)
}

// FitBuys scales buy orders down, proportionally, so their total fits in cash.
// Call it after the sells have filled, with the cash they actually raised.
func FitBuys(buys []market.Order, cash float64) []market.Order {
	var total float64
	for _, o := range buys {
		total += o.Notional
	}
	if total <= cash || total == 0 {
		return buys
	}
	k := math.Max(cash, 0) / total
	out := make([]market.Order, 0, len(buys))
	for _, o := range buys {
		o.Notional = math.Floor(o.Notional*k*100) / 100
		if o.Notional > 0 {
			out = append(out, o)
		}
	}
	return out
}

func roundCents(x float64) float64 { return math.Round(x*100) / 100 }
