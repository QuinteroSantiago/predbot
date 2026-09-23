package backtest

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"predbot/internal/core"
	"predbot/internal/venue"
)

// AdverseSelection measures the market maker's fundamental problem: a resting
// order fills precisely when someone is eager to take the other side, which is
// disproportionately when the price is about to move AGAINST you.
//
// Why this decides the maker-rebate thesis: resting a leg cuts the fee hurdle
// from ~3.49c to ~1.44c, turning 1 opportunity into 120. But if each fill is
// followed by an adverse move larger than the rebate plus captured spread, the
// rebate is bait — you are being paid to buy things about to be worth less.
//
// IT MUST BE DRIVEN BY TRADE PRINTS, NOT BOOKS. An earlier version tried to
// infer fills from order-book changes and found nothing, because a book cannot
// distinguish a TRADE from a CANCELLATION — the size at the touch drops either
// way. Adverse selection is defined on fills, so books are structurally unable
// to answer it. Every trade print, by contrast, IS a maker fill for whoever was
// resting: the aggressor is the taker, so the maker took the OPPOSITE side.
//
// Method: for each trade, record the mid at that moment and the side the MAKER
// was filled on (opposite the taker). Re-observe the mid at each horizon and
// report the drift in the direction that hurts the maker.
type AdverseSelection struct {
	Horizons []int64 // seconds

	open   map[string][]*probe // per market, in time order
	closed map[int64][]float64 // horizon -> adverse drift samples, cents
	mids   map[string]float64  // latest mid per market, cents
	trades int
	minH   int64
}

type probe struct {
	side      core.Side // the side the MAKER was filled on
	midAtFill float64   // cents
	ts        int64
	remaining map[int64]bool
}

func NewAdverseSelection(horizons ...int64) *AdverseSelection {
	if len(horizons) == 0 {
		horizons = []int64{30, 120, 600}
	}
	minH := horizons[0]
	for _, h := range horizons {
		if h < minH {
			minH = h
		}
	}
	return &AdverseSelection{
		Horizons: horizons,
		open:     map[string][]*probe{},
		closed:   map[int64][]float64{},
		mids:     map[string]float64{},
		minH:     minH,
	}
}

func mid(b core.BookSnapshot) (float64, bool) {
	bid, okB := b.BestYesBid()
	ask, okA := b.BestYesAsk()
	if !okB || !okA {
		return 0, false
	}
	return (float64(bid.Px) + float64(ask.Px)) / 2 / 100, true // cents
}

// ObserveBook keeps the running mid per market and advances open probes to their
// horizons. Books supply the price path; they never create probes.
func (a *AdverseSelection) ObserveBook(canonical string, b core.BookSnapshot) {
	m, ok := mid(b)
	if !ok {
		return
	}
	a.mids[canonical] = m

	// Probes are appended in time order, so once one is too young for the
	// shortest horizon, so is everything after it.
	ps := a.open[canonical]
	cut := 0
	for i, p := range ps {
		elapsed := (b.TsRecv - p.ts) / 1e9
		if elapsed < a.minH {
			break
		}
		done := true
		for _, h := range a.Horizons {
			if !p.remaining[h] {
				continue
			}
			if elapsed >= h {
				// Positive = moved AGAINST the maker. A maker filled LONG is hurt
				// when the mid falls; filled SHORT when it rises.
				d := p.midAtFill - m
				if p.side == core.SellYes {
					d = m - p.midAtFill
				}
				a.closed[h] = append(a.closed[h], d)
				p.remaining[h] = false
			}
			if p.remaining[h] {
				done = false
			}
		}
		if done {
			cut = i + 1
		}
	}
	if cut > 0 {
		a.open[canonical] = ps[cut:]
	}
}

// ObserveTrade records a real fill. The trade's taker is the aggressor, so the
// MAKER was filled on the opposite side — that is whose P&L we are tracking.
func (a *AdverseSelection) ObserveTrade(canonical string, t venue.Trade) {
	m, ok := a.mids[canonical]
	if !ok {
		return // no book context yet for this market
	}
	a.trades++
	makerSide := core.SellYes // taker bought => maker sold
	if t.TakerSide == core.SellYes {
		makerSide = core.BuyYes
	}
	if len(a.open[canonical]) > 500 { // bound per-market memory
		return
	}
	rem := make(map[int64]bool, len(a.Horizons))
	for _, h := range a.Horizons {
		rem[h] = true
	}
	ts := t.TsRecv
	if ts == 0 {
		ts = t.TsServer
	}
	a.open[canonical] = append(a.open[canonical],
		&probe{side: makerSide, midAtFill: m, ts: ts, remaining: rem})
}

// AdverseRebateCents is the Polymarket maker rebate per contract at p=0.5, the
// yardstick for the adverse-selection verdict.
const AdverseRebateCents = 0.31

// AdverseResult is the adverse-selection measurement, for the text report and
// the backtest runs page. Drift is in cents; positive = against the maker.
type AdverseResult struct {
	Trades      int              `json:"trades"` // trade prints that seeded probes
	RebateCents float64          `json:"rebate_cents"`
	Horizons    []AdverseHorizon `json:"horizons"` // shortest first
}

// AdverseHorizon is the drift measured a fixed time after each fill.
type AdverseHorizon struct {
	Seconds   int64   `json:"seconds"`
	Fills     int     `json:"fills"`
	MeanCents float64 `json:"mean_cents"`
	// StdErrCents is the standard error of MeanCents: ±1.96·StdErrCents is a
	// rough 95% range for the true mean drift.
	StdErrCents  float64 `json:"stderr_cents"`
	AgainstShare float64 `json:"against_share"` // fraction of fills the price moved against
}

// Result computes the adverse-selection summary.
func (a *AdverseSelection) Result() AdverseResult {
	res := AdverseResult{Trades: a.trades, RebateCents: AdverseRebateCents}
	hs := append([]int64{}, a.Horizons...)
	sort.Slice(hs, func(i, j int) bool { return hs[i] < hs[j] })
	for _, h := range hs {
		xs := a.closed[h]
		hr := AdverseHorizon{Seconds: h, Fills: len(xs)}
		if len(xs) > 0 {
			var sum, worse float64
			for _, x := range xs {
				sum += x
				if x > 0 {
					worse++
				}
			}
			n := float64(len(xs))
			hr.MeanCents, hr.AgainstShare = sum/n, worse/n
			if len(xs) > 1 {
				var ss float64
				for _, x := range xs {
					ss += (x - hr.MeanCents) * (x - hr.MeanCents)
				}
				hr.StdErrCents = math.Sqrt(ss/(n-1)) / math.Sqrt(n)
			}
		}
		res.Horizons = append(res.Horizons, hr)
	}
	return res
}

// Report summarises the measured adverse drift.
func (a *AdverseSelection) Report() string {
	res := a.Result()
	var b strings.Builder
	fmt.Fprintln(&b, "=== Adverse selection: where does the price go AFTER a maker fill? ===")
	fmt.Fprintf(&b, "  driven by %d real trade prints (a book cannot tell a trade from a cancel)\n", res.Trades)
	fmt.Fprintln(&b, "  positive drift = the price moved AGAINST the maker, i.e. the rebate was bait")
	for _, h := range res.Horizons {
		if h.Fills == 0 {
			fmt.Fprintf(&b, "  %4ds: no samples yet\n", h.Seconds)
			continue
		}
		fmt.Fprintf(&b, "  %4ds: mean adverse drift %+.3fc over %d fills (%.0f%% moved against the maker)\n",
			h.Seconds, h.MeanCents, h.Fills, 100*h.AgainstShare)
	}
	fmt.Fprintf(&b, "  VERDICT RULE: the Poly maker rebate is ~%.2fc/contract at p=0.5.\n", AdverseRebateCents)
	fmt.Fprintln(&b, "  If mean adverse drift exceeds rebate + captured spread, maker resting LOSES.")
	return b.String()
}
