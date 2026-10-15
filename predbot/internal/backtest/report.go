package backtest

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"predbot/internal/core"
)

// Report is the only output that matters (design §10.5): where the money went.
// All monetary fields are in $0.0001 units.
type Report struct {
	GrossCaptured int64
	FeesByVenue   map[string]int64 // taker+maker fees per venue
	Slippage      int64            // realized fill worse than intended (informational)
	LeggingCost   int64            // round-trip exits on half-filled pairs (§8, first-class)
	NetPnL        int64
	CapitalLocked int64 // sum of per-leg buying-power cost (§10.4)

	PairsAttempted int
	PairsCompleted int
	PairsLegged    int // filled unevenly -> some unhedged qty
	PairsFaded     int // vanished within latency Δ (§10.2)

	IntraCrossings int // §9.2 crossed-book detections (feed-consistency)

	// Finalized risk/return stats (§10.4, §10.5), filled by Finalize.
	Annualized       float64 // capital-weighted annualized return
	ExcessAnnualized float64 // Annualized minus the idle-balance hurdle
	IdleYield        float64 // hurdle: idle-balance yield (§10.4, [VERIFY])
	AvgHoldDays      float64 // capital-weighted days to resolution
	SharpePerTrade   float64 // mean/stddev of per-trade returns (unitless)
	WinRate          float64 // fraction of pairs with net > 0
	MaxDrawdown      int64   // worst peak-to-trough of cumulative net P&L

	// PeakCapital is the most capital held at once on each venue ($0.0001),
	// filled by Finalize: a pair holds its legs' capital from entry until its
	// market's last recorded update (the stand-in for resolution, which sports
	// markets don't record). What a bankroll has to cover, unlike CapitalLocked,
	// which sums every pair's capital over the whole run.
	PeakCapital map[string]int64

	byCanonical map[string]int   // opportunities per canonical (receipts)
	pairs       []pairResult     // per-pair outcomes, for the derived stats
	lastSeen    map[string]int64 // canonical -> last update (ns)
}

type pairResult struct {
	canonical              string
	net, capital, entryTs  int64
	legs                   []legCapital
}

// legCapital is the capital one leg of a pair ties up on its venue.
type legCapital struct {
	venue   string
	capital int64
}

func NewReport() *Report {
	return &Report{FeesByVenue: map[string]int64{}, byCanonical: map[string]int{},
		PeakCapital: map[string]int64{}, lastSeen: map[string]int64{}}
}

// addPair records one settled pair's outcome for the derived stats; legs say
// which venue each leg's capital sits on, for PeakCapital.
func (r *Report) addPair(canonical string, net, capital, entryTs int64, legs ...legCapital) {
	r.pairs = append(r.pairs, pairResult{canonical, net, capital, entryTs, legs})
}

// seen records a market's latest update; its last one stands in for when the
// market resolved and freed the capital held in it.
func (r *Report) seen(canonical string, ts int64) {
	if ts > r.lastSeen[canonical] {
		r.lastSeen[canonical] = ts
	}
}

// Merge folds o's raw tallies into r. It is for combining reports built over
// disjoint sets of markets (one per replay shard), so per-canonical entries
// never collide. Derived stats are left alone: Finalize the merged report once.
func (r *Report) Merge(o *Report) {
	r.GrossCaptured += o.GrossCaptured
	for v, f := range o.FeesByVenue {
		r.FeesByVenue[v] += f
	}
	r.Slippage += o.Slippage
	r.LeggingCost += o.LeggingCost
	r.NetPnL += o.NetPnL
	r.CapitalLocked += o.CapitalLocked
	r.PairsAttempted += o.PairsAttempted
	r.PairsCompleted += o.PairsCompleted
	r.PairsLegged += o.PairsLegged
	r.PairsFaded += o.PairsFaded
	r.IntraCrossings += o.IntraCrossings
	for c, n := range o.byCanonical {
		r.byCanonical[c] += n
	}
	r.pairs = append(r.pairs, o.pairs...)
	for c, ts := range o.lastSeen {
		r.seen(c, ts)
	}
}

// Finalize computes the §10.4/§10.5 return and risk stats. resolution maps a
// canonical id to its resolution date (from markets.yaml); idleYield is the
// annual hurdle to beat — capital is locked until resolution, so an arb has to
// clear what the same cash would earn sitting idle (§10.4).
func (r *Report) Finalize(resolution map[string]time.Time, idleYield float64) {
	r.IdleYield = idleYield
	if len(r.pairs) == 0 {
		return
	}
	// Walk pairs in entry order: max drawdown follows the cumulative P&L curve,
	// and a fixed order keeps the float sums identical however the replay was
	// sharded.
	sort.SliceStable(r.pairs, func(i, j int) bool {
		a, b := r.pairs[i], r.pairs[j]
		if a.entryTs != b.entryTs {
			return a.entryTs < b.entryTs
		}
		return a.canonical < b.canonical
	})
	var sumNet, sumCap, capDays float64
	rets := make([]float64, 0, len(r.pairs))
	wins := 0
	var cum, peak int64
	for _, p := range r.pairs {
		sumNet += float64(p.net)
		sumCap += float64(p.capital)
		if res, ok := resolution[p.canonical]; ok {
			days := res.Sub(time.Unix(0, p.entryTs)).Hours() / 24
			if days < 1 {
				days = 1
			}
			capDays += float64(p.capital) * days
		}
		if p.capital > 0 {
			rets = append(rets, float64(p.net)/float64(p.capital))
		}
		if p.net > 0 {
			wins++
		}
		cum += p.net
		if cum > peak {
			peak = cum
		}
		if peak-cum > r.MaxDrawdown {
			r.MaxDrawdown = peak - cum
		}
	}
	r.WinRate = float64(wins) / float64(len(r.pairs))
	if sumCap > 0 {
		raw := sumNet / sumCap
		if capDays > 0 {
			r.AvgHoldDays = capDays / sumCap
			r.Annualized = raw * 365 / r.AvgHoldDays
			r.ExcessAnnualized = r.Annualized - idleYield
		}
	}
	if len(rets) >= 2 {
		r.SharpePerTrade = sharpe(rets)
	}
	r.PeakCapital = r.peakCapital()
}

// peakCapital sweeps each venue's capital over time: every leg adds its
// capital at entry and releases it at its market's last update.
func (r *Report) peakCapital() map[string]int64 {
	type ev struct {
		ts    int64
		delta int64
	}
	evs := map[string][]ev{}
	for _, p := range r.pairs {
		release := r.lastSeen[p.canonical]
		if release < p.entryTs {
			release = p.entryTs
		}
		for _, l := range p.legs {
			if l.capital <= 0 {
				continue
			}
			evs[l.venue] = append(evs[l.venue], ev{p.entryTs, l.capital}, ev{release, -l.capital})
		}
	}
	peaks := map[string]int64{}
	for v, es := range evs {
		// At equal times release first: capital freed at t can back an entry at t.
		sort.Slice(es, func(i, j int) bool {
			if es[i].ts != es[j].ts {
				return es[i].ts < es[j].ts
			}
			return es[i].delta < es[j].delta
		})
		var held, peak int64
		for _, e := range es {
			held += e.delta
			peak = max(peak, held)
		}
		peaks[v] = peak
	}
	return peaks
}

// sharpe returns mean/stddev of the returns (sample stddev), unitless.
func sharpe(xs []float64) float64 {
	n := float64(len(xs))
	var sum float64
	for _, x := range xs {
		sum += x
	}
	mean := sum / n
	var ss float64
	for _, x := range xs {
		ss += (x - mean) * (x - mean)
	}
	std := math.Sqrt(ss / (n - 1))
	if std == 0 {
		return 0
	}
	return mean / std
}

func d(v int64) string { return "$" + core.Price(v).String() }

// String renders the §10.5 report.
func (r *Report) String() string {
	var b strings.Builder
	var feeTotal int64
	for _, v := range r.FeesByVenue {
		feeTotal += v
	}
	fmt.Fprintln(&b, "=== Backtest report (design §10.5) ===")
	fmt.Fprintf(&b, "Gross gap captured        %s\n", d(r.GrossCaptured))
	fmt.Fprintf(&b, "Fees (total)              %s\n", d(-feeTotal))
	for _, v := range sortedKeys(r.FeesByVenue) {
		fmt.Fprintf(&b, "  - %-20s  %s\n", v, d(-r.FeesByVenue[v]))
	}
	fmt.Fprintf(&b, "Slippage (fill vs top)    %s   (informational; already in gross)\n", d(-r.Slippage))
	fmt.Fprintf(&b, "Legging cost              %s\n", d(-r.LeggingCost))
	fmt.Fprintf(&b, "Net P&L                   %s\n", d(r.NetPnL))
	fmt.Fprintf(&b, "Capital locked            %s   (summed over every pair)\n", d(r.CapitalLocked))
	if len(r.PeakCapital) > 0 {
		fmt.Fprintf(&b, "Peak capital at once     ")
		for _, v := range sortedKeys(r.PeakCapital) {
			fmt.Fprintf(&b, " %s %s", v, d(r.PeakCapital[v]))
		}
		fmt.Fprintf(&b, "   (held from entry to the market's last update)\n")
	}
	fmt.Fprintln(&b, "---")
	fmt.Fprintf(&b, "Pairs attempted/completed/legged/faded  %d / %d / %d / %d\n",
		r.PairsAttempted, r.PairsCompleted, r.PairsLegged, r.PairsFaded)
	if r.PairsAttempted > 0 {
		fmt.Fprintf(&b, "Hit rate (completed/attempted)          %.1f%%\n",
			100*float64(r.PairsCompleted)/float64(r.PairsAttempted))
	}
	fmt.Fprintf(&b, "Intra-venue crossings (feed check)      %d\n", r.IntraCrossings)
	fmt.Fprintln(&b, "--- return & risk (§10.4, §10.5) ---")
	if len(r.pairs) == 0 {
		fmt.Fprintln(&b, "  (no completed pairs)")
	} else {
		fmt.Fprintf(&b, "Capital-weighted annualized return      %+.2f%%   (avg hold %.0f days)\n",
			100*r.Annualized, r.AvgHoldDays)
		fmt.Fprintf(&b, "  vs idle-balance hurdle %.1f%% -> excess   %+.2f%%\n",
			100*r.IdleYield, 100*r.ExcessAnnualized)
		if len(r.pairs) >= 2 {
			fmt.Fprintf(&b, "Sharpe (per-trade)                      %.2f\n", r.SharpePerTrade)
		} else {
			fmt.Fprintf(&b, "Sharpe (per-trade)                      n/a (need >=2 trades)\n")
		}
		fmt.Fprintf(&b, "Win rate (net>0)                        %.0f%% of %d\n", 100*r.WinRate, len(r.pairs))
		fmt.Fprintf(&b, "Max drawdown (cumulative net)           %s\n", d(-r.MaxDrawdown))
	}
	fmt.Fprintln(&b, "--- opportunities by market ---")
	for _, c := range sortedKeys2(r.byCanonical) {
		fmt.Fprintf(&b, "  %-28s %d\n", c, r.byCanonical[c])
	}
	// TODO (§10.5): capital-weighted annualized return (needs resolution dates
	// from markets.yaml), Sharpe / max drawdown (needs a per-pair P&L series),
	// resolution-mismatch exposure.
	return b.String()
}

func sortedKeys(m map[string]int64) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
func sortedKeys2(m map[string]int) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
