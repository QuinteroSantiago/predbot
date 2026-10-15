package strategy

import (
	"fmt"
	"sort"
	"time"

	"etfbot/internal/market"
)

// Rotation holds the TopN symbols with the strongest momentum, each only while
// its price is above its moving average; a slot that fails the trend check
// goes to Cash. Momentum is the average of the total returns over Lookbacks.
type Rotation struct {
	Universe  []string
	Cash      string // short-term Treasury ETF standing in for cash, e.g. BIL
	TopN      int
	Lookbacks []int // trading days, e.g. 63, 126, 252 (3, 6, 12 months)
	SMA       int   // trend filter length in trading days, e.g. 200
}

func (r Rotation) Name() string { return fmt.Sprintf("rotation top%d", r.TopN) }

func (r Rotation) Symbols() []string { return append(append([]string{}, r.Universe...), r.Cash) }

// need is how many bars a symbol must have before it can be ranked.
func (r Rotation) need() int {
	n := r.SMA
	for _, lb := range r.Lookbacks {
		n = max(n, lb+1)
	}
	return n
}

func (r Rotation) Targets(asOf time.Time, hist map[string][]market.Bar) (map[string]float64, error) {
	if r.TopN <= 0 || len(r.Lookbacks) == 0 || r.SMA <= 0 {
		return nil, fmt.Errorf("rotation: TopN, Lookbacks and SMA must be set")
	}
	type cand struct {
		sym   string
		score float64
		trend bool
	}
	var cands []cand
	for _, sym := range r.Universe {
		bars := market.Upto(hist[sym], asOf)
		if len(bars) < r.need() {
			continue // not enough history yet (a fund that launched recently)
		}
		last := bars[len(bars)-1].Close
		var score float64
		for _, lb := range r.Lookbacks {
			score += last/bars[len(bars)-1-lb].Close - 1
		}
		var sum float64
		for _, b := range bars[len(bars)-r.SMA:] {
			sum += b.Close
		}
		cands = append(cands, cand{sym, score / float64(len(r.Lookbacks)), last > sum/float64(r.SMA)})
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].score > cands[j].score })

	w := 1 / float64(r.TopN)
	out := map[string]float64{}
	for i := range r.TopN {
		sym := r.Cash
		if i < len(cands) && cands[i].trend {
			sym = cands[i].sym
		}
		if sym == r.Cash && len(market.Upto(hist[r.Cash], asOf)) == 0 {
			continue // no cash fund yet: the slot stays in cash
		}
		out[sym] += w
	}
	return out, nil
}
