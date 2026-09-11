// Package exec holds order executors. Phase 3a provides PaperTrader — a
// SimExecutor driven by the LIVE feed and wall-clock (design §17 Phase 3): it
// takes strategy Intents, simulates the fill against the live Book Manager at
// t+Δ (so fade and latency are real, not modeled), and tracks a running paper
// P&L. No venue ever hears about these orders — it is pure simulation.
package exec

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"time"

	"predbot/internal/book"
	"predbot/internal/core"
	"predbot/internal/strategy"
)

// Stats is the paper account's running state, in USD.
type Stats struct {
	GrossUSD      float64
	FeesUSD       float64
	LeggingUSD    float64
	NetUSD        float64
	CapitalUSD    float64 // capital currently locked in open paper positions
	Attempted     int
	Completed     int
	Legged        int
	Faded         int
	OpenPositions int
}

// PaperTrader simulates fills against the live book. Safe for concurrent use:
// a strategy loop calls Submit while Run settles due fills and Snapshot reads.
type PaperTrader struct {
	fees    strategy.FeeModel
	latency time.Duration
	bm      *book.Manager
	log     *os.File // optional JSONL fills log (paper-fills.jsonl), for comparing against the backtest

	mu        sync.Mutex
	pending   []pendingPair
	stat      Stats
	onOutcome func(outcome string) // optional: called once per settled pair (filled/partial/faded)
}

// OnOutcome registers a callback fired for every settled pair with its outcome
// ("filled", "partial", or "faded"), so a caller can bump an execution-outcome
// metric without the exec package depending on the obs package. Not safe to call
// concurrently with Run; set it once at construction.
func (p *PaperTrader) OnOutcome(f func(outcome string)) { p.onOutcome = f }

// outcome invokes the callback if one is registered.
func (p *PaperTrader) outcome(o string) {
	if p.onOutcome != nil {
		p.onOutcome(o)
	}
}

type pendingPair struct {
	canonical string
	legs      []core.Intent
	at        time.Time
}

// NewPaperTrader builds a paper executor. latencyMS is the design-§10.2 Δ; logPath
// (if non-empty) receives a JSONL line per settled pair, for paper-vs-backtest comparison.
func NewPaperTrader(fees strategy.FeeModel, latencyMS int, bm *book.Manager, logPath string) *PaperTrader {
	p := &PaperTrader{fees: fees, latency: time.Duration(latencyMS) * time.Millisecond, bm: bm}
	if logPath != "" {
		if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640); err == nil {
			p.log = f
		}
	}
	return p
}

// Submit queues a pair for fill at now+Δ.
func (p *PaperTrader) Submit(canonical string, legs []core.Intent) {
	if len(legs) != 2 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stat.Attempted++
	p.pending = append(p.pending, pendingPair{canonical, legs, time.Now().Add(p.latency)})
}

// Run settles due fills every 50ms until ctx is done.
func (p *PaperTrader) Run(ctx context.Context) {
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			if p.log != nil {
				_ = p.log.Close()
			}
			return
		case <-t.C:
			p.settleDue()
		}
	}
}

func (p *PaperTrader) settleDue() {
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	kept := p.pending[:0]
	for _, pp := range p.pending {
		if pp.at.After(now) {
			kept = append(kept, pp)
			continue
		}
		p.settle(pp)
	}
	p.pending = kept
}

// settle matches a pair against the live book at fill time. Caller holds p.mu.
func (p *PaperTrader) settle(pp pendingPair) {
	books, ok := p.bm.Pair(pp.canonical)
	if !ok {
		p.stat.Faded++
		p.outcome("faded")
		return
	}
	buy, sell := pp.legs[0], pp.legs[1]
	bb, sb := books[buy.Venue], books[sell.Venue]
	buyPx, buyQty := fillBuy(bb, buy.Px, buy.Qty)
	sellPx, sellQty := fillSell(sb, sell.Px, sell.Qty)
	if buyQty == 0 || sellQty == 0 {
		p.stat.Faded++ // opportunity vanished within Δ (real fade)
		p.outcome("faded")
		return
	}
	hedged := buyQty
	if sellQty < hedged {
		hedged = sellQty
	}
	c := float64(hedged) / 100
	gross := float64(int64(sellPx)-int64(buyPx)) / 1e4 * c
	feeBuy := float64(p.fees.Fee(buy.Venue, buyPx, hedged, false)) / 1e4
	feeSell := float64(p.fees.Fee(sell.Venue, sellPx, hedged, false)) / 1e4
	capital := float64(buyPx)/1e4*c + float64(int64(core.OneDollar)-int64(sellPx))/1e4*c

	p.stat.GrossUSD += gross
	p.stat.FeesUSD += feeBuy + feeSell
	p.stat.CapitalUSD += capital
	p.stat.NetUSD += gross - feeBuy - feeSell
	p.stat.OpenPositions++
	if buyQty == sellQty {
		p.stat.Completed++
		p.outcome("filled")
	} else {
		p.stat.Legged++
		p.outcome("partial")
	}
	p.writeLog(pp.canonical, buy, sell, buyPx, sellPx, hedged, gross, feeBuy+feeSell)
}

func (p *PaperTrader) writeLog(canonical string, buy, sell core.Intent, buyPx, sellPx core.Price, qty core.Qty, gross, fees float64) {
	if p.log == nil {
		return
	}
	line, _ := json.Marshal(map[string]any{
		"ts_ms": time.Now().UnixMilli(), "canonical": canonical,
		"buy_venue": buy.Venue, "buy_cents": float64(buyPx) / 100,
		"sell_venue": sell.Venue, "sell_cents": float64(sellPx) / 100,
		"qty": float64(qty) / 100, "gross_usd": gross, "fees_usd": fees, "net_usd": gross - fees,
	})
	_, _ = p.log.Write(append(line, '\n'))
}

// Snapshot returns a copy of the current stats, safe to call concurrently.
func (p *PaperTrader) Snapshot() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stat
}

// fillBuy walks YES asks (ascending) up to the limit and qty; fillSell walks YES
// bids (descending). Same book-walk as the backtest fill model (design §10.2).
func fillBuy(b core.BookSnapshot, limit core.Price, qty core.Qty) (core.Price, core.Qty) {
	return walk(b.YesAsks, qty, func(px core.Price) bool { return px <= limit })
}
func fillSell(b core.BookSnapshot, limit core.Price, qty core.Qty) (core.Price, core.Qty) {
	return walk(b.YesBids, qty, func(px core.Price) bool { return px >= limit })
}
func walk(levels []core.Level, want core.Qty, ok func(core.Price) bool) (core.Price, core.Qty) {
	var filled core.Qty
	var notional int64
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
