// Package risk is the gate every live order passes, and the drawdown stop.
// Checks run on the whole plan before anything is sent: a plan with one bad
// order sends nothing.
package risk

import (
	"errors"
	"fmt"
	"math"

	"tradekit/killswitch"

	"etfbot/internal/config"
	"etfbot/internal/market"
)

var (
	ErrKilled     = errors.New("risk: kill switch engaged")
	ErrUniverse   = errors.New("risk: symbol outside the configured universe")
	ErrWeight     = errors.New("risk: target weight above the cap")
	ErrOrderSize  = errors.New("risk: order above the size cap")
	ErrOverInvest = errors.New("risk: targets sum above 100%")
)

// Gate holds the limits and the kill switch.
type Gate struct {
	Limits  config.Risk
	Allowed map[string]bool
	Cash    string // exempt from MaxWeight
	Kill    *killswitch.Switch
}

// New builds the gate for cfg.
func New(cfg config.Config, kill *killswitch.Switch) *Gate {
	allowed := map[string]bool{cfg.Strategy.Cash: true}
	for _, s := range cfg.Strategy.Universe {
		allowed[s] = true
	}
	return &Gate{Limits: cfg.Risk, Allowed: allowed, Cash: cfg.Strategy.Cash, Kill: kill}
}

// Check validates targets and the orders planned from them. Sells of symbols
// outside the universe are allowed, so an unexpected holding can be exited.
func (g *Gate) Check(targets map[string]float64, orders []market.Order) error {
	if g.Kill.Killed() {
		return ErrKilled
	}
	var sum float64
	for sym, w := range targets {
		switch {
		case !g.Allowed[sym]:
			return fmt.Errorf("%w: target %s", ErrUniverse, sym)
		case w < 0 || (sym != g.Cash && w > g.Limits.MaxWeight+1e-9):
			return fmt.Errorf("%w: %s at %.1f%%", ErrWeight, sym, w*100)
		}
		sum += w
	}
	if sum > 1+1e-9 {
		return fmt.Errorf("%w (%.2f%%)", ErrOverInvest, sum*100)
	}
	for _, o := range orders {
		if o.Side == market.Buy && !g.Allowed[o.Symbol] {
			return fmt.Errorf("%w: buy %s", ErrUniverse, o.Symbol)
		}
		if o.Notional > g.Limits.MaxOrderUSD || math.IsNaN(o.Notional) || math.IsNaN(o.Qty) || o.Qty < 0 || o.Notional < 0 {
			return fmt.Errorf("%w: %s %s $%.2f", ErrOrderSize, o.Side, o.Symbol, o.Notional)
		}
	}
	return nil
}

// CheckSellSize caps a sell's dollar value at price, which the plan alone
// doesn't know.
func (g *Gate) CheckSellSize(o market.Order, price float64) error {
	if o.Side == market.Sell && o.Qty*price > g.Limits.MaxOrderUSD {
		return fmt.Errorf("%w: sell %s $%.2f", ErrOrderSize, o.Symbol, o.Qty*price)
	}
	return nil
}

// Drawdown engages the kill switch if equity has fallen more than MaxDrawdown
// below peak, and reports whether it did.
func (g *Gate) Drawdown(equity, peak float64) bool {
	if peak > 0 && equity < peak*(1-g.Limits.MaxDrawdown) {
		g.Kill.Kill(fmt.Sprintf("drawdown: equity $%.2f is %.1f%% below the $%.2f peak",
			equity, (1-equity/peak)*100, peak))
		return true
	}
	return false
}
