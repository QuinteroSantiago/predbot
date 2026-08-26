// Package risk is the Position & Risk Engine (design §8): the single gate that
// NO order may bypass. It owns positions, in-flight (approved-but-unfilled)
// orders, per-venue buying power, and the unhedged tally, and it does the risk
// check and the reservation in ONE atomic step so there is no check-then-act
// race between "approve intent" and "fill arrives".
package risk

import (
	"errors"
	"os"
	"sync"

	"predbot/internal/core"
)

// Typed rejections (design §8). Every path to an order returns one of these or nil.
var (
	ErrKilled      = errors.New("risk: kill switch engaged")
	ErrOrderTooBig = errors.New("risk: order exceeds max size")
	ErrPriceSanity = errors.New("risk: price outside (0, $1)")
	ErrPositionCap = errors.New("risk: position cap exceeded")
	ErrBuyingPower = errors.New("risk: insufficient buying power")
	ErrUnhedgedCap = errors.New("risk: unhedged cap exceeded")
)

// Config holds the hard limits. Start these small (tuition-sized, design §17).
type Config struct {
	MaxOrderQty   core.Qty            // per-order cap (0.01-contract units)
	DefaultMaxPos core.Qty            // per-market net position cap
	MaxPos        map[string]core.Qty // per-market overrides
	MinBP         map[string]int64    // per-venue minimum buying power to keep, $0.0001
	MaxUnhedged   core.Qty            // total unhedged contracts allowed
}

// Engine is safe for concurrent use.
type Engine struct {
	mu         sync.Mutex
	cfg        Config
	pos        map[string]core.Qty // canonical -> filled net YES contracts (signed)
	inflight   map[string]core.Qty // canonical -> approved-but-unfilled, signed
	bp         map[string]int64    // venue -> buying power, $0.0001
	unhedged   map[string]core.Qty // pairID -> unhedged qty (abs)
	killed     bool
	killReason string
	killFile   string // marker persisted so a restart stays killed (§8)
}

// New builds an engine with starting per-venue buying power (in $0.0001 units).
// killFile, if non-empty, is created on Kill and its presence at startup keeps
// the engine killed until a human removes it (design §8, §14).
func New(cfg Config, startingBP map[string]int64, killFile string) *Engine {
	e := &Engine{
		cfg: cfg, killFile: killFile,
		pos: map[string]core.Qty{}, inflight: map[string]core.Qty{},
		bp: map[string]int64{}, unhedged: map[string]core.Qty{},
	}
	for v, b := range startingBP {
		e.bp[v] = b
	}
	if killFile != "" {
		if _, err := os.Stat(killFile); err == nil {
			e.killed = true
			e.killReason = "KILLED marker present at startup — remove " + killFile + " to re-enable"
		}
	}
	return e
}

func signed(i core.Intent) core.Qty {
	if i.Side == core.SellYes {
		return -i.Qty
	}
	return i.Qty
}

func abs(q core.Qty) core.Qty {
	if q < 0 {
		return -q
	}
	return q
}

// bpCost is the buying power an intent consumes: BuyYes at p costs p·qty;
// SellYes at p costs (1−p)·qty (Poly US posts $1 margin, receives p; Kalshi
// economics assumed equivalent — [VERIFY] field semantics before live, §8).
func bpCost(i core.Intent) int64 {
	c := int64(i.Qty) // 0.01-contract units
	px := int64(i.Px) // $0.0001 units
	if i.Side == core.SellYes {
		px = int64(core.OneDollar) - px
	}
	return px * c / 100 // ($0.0001 * 0.01-contract) -> $0.0001
}

// CheckAndReserve is the gate: it validates the intent against every limit and,
// only if all pass, reserves the in-flight quantity and buying power atomically.
// canonical is the market key. On any violation it reserves nothing and returns
// the typed error.
func (e *Engine) CheckAndReserve(canonical string, i core.Intent) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	switch {
	case e.killed:
		return ErrKilled
	case i.Qty <= 0 || i.Qty > e.cfg.MaxOrderQty:
		return ErrOrderTooBig
	case i.Px <= 0 || i.Px >= core.OneDollar:
		return ErrPriceSanity
	}
	maxPos := e.cfg.DefaultMaxPos
	if m, ok := e.cfg.MaxPos[canonical]; ok {
		maxPos = m
	}
	if abs(e.pos[canonical]+e.inflight[canonical]+signed(i)) > maxPos {
		return ErrPositionCap
	}
	if e.bp[i.Venue]-bpCost(i) < e.cfg.MinBP[i.Venue] {
		return ErrBuyingPower
	}
	if e.totalUnhedgedLocked()+i.Qty > e.cfg.MaxUnhedged {
		return ErrUnhedgedCap
	}

	// All checks passed — reserve.
	e.inflight[canonical] += signed(i)
	e.bp[i.Venue] -= bpCost(i)
	return nil
}

// OnFill applies a fill: move the quantity from in-flight to a real position.
func (e *Engine) OnFill(canonical string, i core.Intent, filled core.Qty) {
	e.mu.Lock()
	defer e.mu.Unlock()
	sgn := core.Qty(1)
	if i.Side == core.SellYes {
		sgn = -1
	}
	e.inflight[canonical] -= sgn * filled
	e.pos[canonical] += sgn * filled
}

// OnReject releases a reservation when an order is rejected or cancelled unfilled.
func (e *Engine) OnReject(canonical string, i core.Intent) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.inflight[canonical] -= signed(i)
	e.bp[i.Venue] += bpCost(i)
}

// SetUnhedged records the current unhedged quantity for a pair (design §8 legging).
func (e *Engine) SetUnhedged(pairID string, qty core.Qty) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if qty == 0 {
		delete(e.unhedged, pairID)
	} else {
		e.unhedged[pairID] = abs(qty)
	}
}

// UnhedgedSnapshot returns the current unhedged quantity per pair, in contracts.
// For observability: a naked / half-filled leg shows up here as a nonzero entry
// (published as predbot_unhedged_contracts{canonical}).
func (e *Engine) UnhedgedSnapshot() map[string]float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[string]float64, len(e.unhedged))
	for k, q := range e.unhedged {
		out[k] = float64(q) / 100
	}
	return out
}

func (e *Engine) totalUnhedgedLocked() core.Qty {
	var t core.Qty
	for _, q := range e.unhedged {
		t += q
	}
	return t
}

// Kill engages the kill switch and persists the marker (design §8). Idempotent.
func (e *Engine) Kill(reason string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.killed {
		return
	}
	e.killed = true
	e.killReason = reason
	if e.killFile != "" {
		_ = os.WriteFile(e.killFile, []byte(reason+"\n"), 0o640)
	}
}

// Killed reports whether the kill switch is engaged.
func (e *Engine) Killed() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.killed
}

// AdoptPosition seeds a filled net position this process did not open, so the
// position cap accounts for exposure that already existed at startup.
//
// It matters only for an UNBALANCED position. A hedged pair nets to zero in YES
// terms and consumes no cap — the same whether this process opened it or adopted
// it. A naked or partial leg does not, and without seeding, the engine would
// treat that live directional risk as flat and allow a full position on top.
func (e *Engine) AdoptPosition(canonical string, net core.Qty) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pos[canonical] = net
}

// Position returns the current filled net position for a market.
func (e *Engine) Position(canonical string) core.Qty {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.pos[canonical]
}
