package backtest

import (
	"math"

	"predbot/internal/core"
)

// Fees implements strategy.FeeModel for both venues (design §6.1, §6.2, §10.3).
//
// Both venues price fees as Θ·C·p·(1−p): a fraction of the "risk" of the
// contract, maximized at p=0.5 and zero at the extremes.
//
//	Kalshi:      ceil-to-$0.0001( mult · C · p · (1−p) ), taker; maker ≈ 25% of taker [VERIFY]
//	Polymarket US: bankers-round-to-$0.01( Θ · C · p · (1−p) ), Θ=0.06 taker, −0.0125 maker
//
// C = contracts, p = price in dollars. Returned in signed $0.0001 units
// (negative == rebate). NOTE: Kalshi's per-order rounding accumulator (§6.1) is
// NOT yet modeled — this is per-fill ceil only. TODO before Phase 4.
type Fees struct {
	KalshiMult float64 // base taker multiplier, default 0.07
}

const (
	polyTakerTheta = 0.06
	polyMakerTheta = -0.0125
	kalshiMakerFrac = 0.25 // maker ≈ 25% of taker [VERIFY against official PDF]
)

// Fee returns the signed fee in $0.0001 units.
func (f Fees) Fee(venue string, px core.Price, qty core.Qty, maker bool) int64 {
	c := float64(qty) / 100          // contracts
	p := float64(px) / float64(core.OneDollar)
	risk := c * p * (1 - p)
	switch venue {
	case "polymarketus":
		theta := polyTakerTheta
		if maker {
			theta = polyMakerTheta
		}
		dollars := bankersRound2(theta * risk)
		return int64(math.Round(dollars * float64(core.OneDollar)))
	case "kalshi":
		mult := f.KalshiMult
		if mult == 0 {
			mult = 0.07
		}
		if maker {
			mult *= kalshiMakerFrac
		}
		// Ceil to $0.0001; fees are always charged (never a rebate) on Kalshi.
		return int64(math.Ceil(mult * risk * float64(core.OneDollar)))
	default:
		return 0
	}
}

// bankersRound2 rounds to the nearest $0.01, ties to even (design §6.2).
func bankersRound2(x float64) float64 {
	return math.RoundToEven(x*100) / 100
}
