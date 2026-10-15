// Package strategy holds the pure decision functions (design §9). A strategy
// proposes Intents; it never places orders and does no I/O, so the same code
// runs live and in backtest. Phase 2 runs them detect-only.
package strategy

import (
	"time"

	"predbot/internal/core"
)

// FeeModel returns the signed fee (in $0.0001 units) for a fill of qty at px on
// venue as maker or taker (design §10.3). Negative == rebate. Implemented in the
// backtest package; the interface lives here so strategies can price net edge.
type FeeModel interface {
	Fee(venue string, px core.Price, qty core.Qty, maker bool) int64
}

// Exposure is what a strategy needs to avoid re-emitting an opportunity it is
// already working (design §9): filled positions, in-flight orders, and unhedged
// pairs, keyed by canonical market. Phase 2 detect-only keeps it minimal.
type Exposure struct {
	InFlight map[string]bool // canonical -> a pair is already working
}

// Config carries the tunables (design §9.1, §14).
type Config struct {
	MinEdge core.Price // minimum net edge per contract, > 0 after fees (§9.1)
	MaxClip core.Qty   // max size per opportunity

	// MinDepth is the minimum top-of-book quantity required on BOTH legs before
	// an opportunity is real. Without it the strategy will "find" an edge against
	// a venue quoting one lonely contract (or a book about to empty), fill the
	// first leg, and be unable to hedge — a naked directional position, not an
	// arb. This happened live on 2026-08-24 (MLB-20260827-BAL): a 4c gap against
	// an all-but-empty Kalshi book left a 5-contract unhedged short.
	MinDepth core.Qty
}

// HorizonEdge scales the minimum edge with time to the game's start: a pair
// locks capital on both venues until the game resolves, so a trade that ties it
// up longer must pay more. One step of base per started step until start —
// with a 24h step and 1c base: 1c inside 24h, 2c inside 48h, 3c inside 72h.
// A game already under way (or a step of 0) needs just base.
func HorizonEdge(base core.Price, untilStart, step time.Duration) core.Price {
	if step <= 0 || untilStart <= 0 {
		return base
	}
	return base * core.Price((untilStart+step-1)/step)
}
