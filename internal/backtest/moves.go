package backtest

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"predbot/internal/core"
	"predbot/internal/venue"
)

// MoveStudy is the overreaction study: after a sharp price move, or a burst of
// one-sided trading, does the price keep going (momentum) or snap back
// (reversion) — and does trading either way pay after the spread and fees?
//
// Read-only. Each (venue, market) is its own series. An event opens a probe
// that resolves at each horizon against the first book at or after it, the same
// way adverse selection does, so memory stays O(open probes), not O(archive).
// Settlement outcomes, when loaded, add a hold-to-resolution horizon.
type MoveStudy struct {
	fees     Fees
	outcomes map[string]int // "venue|venueID" -> 1 (yes) / 0 (no); may be nil

	lookback      int64   // seconds a move is measured over
	cooldown      int64   // seconds before the same series can trigger again
	moveCents     float64 // minimum move that triggers
	flowWindow    int64   // seconds of trade flow summed
	flowContracts float64 // minimum net taker flow that triggers
	horizons      []int64 // seconds

	series map[string]*mvSeries
	done   []*mvEvent // every event, resolved or not
}

type mvPoint struct {
	sec int64
	mid float64 // cents
}

type mvFlow struct {
	sec int64
	qty float64 // contracts, + = takers bought YES
}

type mvSeries struct {
	venue    string
	hist     []mvPoint // recent mids, one per second, plus one older than the lookback
	flow     []mvFlow
	flowSum  float64
	bid, ask core.Price
	ok       bool
	lastMove int64 // sec of the last event of each trigger, for the cooldown
	lastFlow int64
	haveMove bool
	haveFlow bool
	open     []*mvEvent
}

type mvEvent struct {
	trigger  string // "move" or "flow"
	key      string // "venue|venueID"
	venue    string
	sec      int64
	dir      int     // +1 the price rose / takers bought; -1 the opposite
	size     float64 // cents moved, or contracts of net flow
	bid, ask core.Price
	res      map[int64]mvRes // horizon (s) -> result; 0 = settlement
}

// mvRes is one event at one horizon, in cents per contract.
type mvRes struct{ cont, follow, fade float64 }

// NewMoveStudy uses the default triggers: a 5¢ move within 60s, or 20
// contracts of net taker flow within 60s, with a 2-minute cooldown.
func NewMoveStudy(fees Fees, outcomes map[string]int) *MoveStudy {
	return &MoveStudy{
		fees: fees, outcomes: outcomes,
		lookback: 60, cooldown: 120, moveCents: 5,
		flowWindow: 60, flowContracts: 20,
		horizons: []int64{30, 120, 600, 1800},
		series:   map[string]*mvSeries{},
	}
}

func (m *MoveStudy) get(venueName, venueID string) *mvSeries {
	k := venueName + "|" + venueID
	s := m.series[k]
	if s == nil {
		s = &mvSeries{venue: venueName}
		m.series[k] = s
	}
	return s
}

// feeCents is the taker fee on one contract at px, in cents.
func (m *MoveStudy) feeCents(venueName string, px core.Price) float64 {
	return float64(m.fees.Fee(venueName, px, 100, false)) / 100
}

func cents(p core.Price) float64 { return float64(p) / 100 }

// roundTrip is the per-contract P&L of a taker trade in direction d (+1 buy
// YES, -1 sell YES) entered against (bid0, ask0) and exited against (bid1, ask1).
func (m *MoveStudy) roundTrip(venueName string, d int, bid0, ask0, bid1, ask1 core.Price) float64 {
	if d > 0 {
		return cents(bid1) - cents(ask0) - m.feeCents(venueName, ask0) - m.feeCents(venueName, bid1)
	}
	return cents(bid0) - cents(ask1) - m.feeCents(venueName, bid0) - m.feeCents(venueName, ask1)
}

// ObserveBook feeds one book snapshot.
func (m *MoveStudy) ObserveBook(b core.BookSnapshot) {
	bid, okB := b.BestYesBid()
	ask, okA := b.BestYesAsk()
	if !okB || !okA || ask.Px <= bid.Px {
		return // one-sided or crossed: no tradeable price
	}
	s := m.get(b.Venue, b.VenueID)
	s.bid, s.ask, s.ok = bid.Px, ask.Px, true
	sec := b.TsRecv / 1e9
	mid := (cents(bid.Px) + cents(ask.Px)) / 2

	// Resolve open probes whose horizon has arrived, against this book.
	kept := s.open[:0]
	for _, e := range s.open {
		pending := false
		for _, h := range m.horizons {
			if _, done := e.res[h]; done {
				continue
			}
			if sec < e.sec+h {
				pending = true
				continue
			}
			e.res[h] = mvRes{
				cont:   (mid - (cents(e.bid)+cents(e.ask))/2) * float64(e.dir),
				follow: m.roundTrip(e.venue, e.dir, e.bid, e.ask, bid.Px, ask.Px),
				fade:   m.roundTrip(e.venue, -e.dir, e.bid, e.ask, bid.Px, ask.Px),
			}
		}
		if pending {
			kept = append(kept, e)
		}
	}
	s.open = kept

	// Keep one sample per second, and exactly one older than the lookback: the
	// reference the current mid is compared against.
	if n := len(s.hist); n > 0 && s.hist[n-1].sec == sec {
		s.hist[n-1].mid = mid
	} else {
		s.hist = append(s.hist, mvPoint{sec, mid})
	}
	for len(s.hist) > 1 && s.hist[1].sec <= sec-m.lookback {
		s.hist = s.hist[1:]
	}
	if ref := s.hist[0]; ref.sec <= sec-m.lookback {
		d := mid - ref.mid
		if math.Abs(d) >= m.moveCents && (!s.haveMove || sec-s.lastMove >= m.cooldown) {
			s.lastMove, s.haveMove = sec, true
			m.open("move", b.Venue+"|"+b.VenueID, s, sec, sign(d), math.Abs(d))
		}
	}
}

// ObserveTrade feeds one trade print. Trades must arrive in time order,
// interleaved with the books.
func (m *MoveStudy) ObserveTrade(t venue.Trade) {
	s := m.series[t.Venue+"|"+t.VenueID]
	if s == nil || !s.ok {
		return // no book yet to price an entry against
	}
	ts := t.TsRecv
	if ts == 0 {
		ts = t.TsServer
	}
	sec := ts / 1e9
	q := float64(t.Qty) / 100
	if t.TakerSide == core.SellYes {
		q = -q
	}
	s.flow = append(s.flow, mvFlow{sec, q})
	s.flowSum += q
	for len(s.flow) > 0 && s.flow[0].sec <= sec-m.flowWindow {
		s.flowSum -= s.flow[0].qty
		s.flow = s.flow[1:]
	}
	if math.Abs(s.flowSum) >= m.flowContracts && (!s.haveFlow || sec-s.lastFlow >= m.cooldown) {
		s.lastFlow, s.haveFlow = sec, true
		m.open("flow", t.Venue+"|"+t.VenueID, s, sec, sign(s.flowSum), math.Abs(s.flowSum))
	}
}

func (m *MoveStudy) open(trigger, key string, s *mvSeries, sec int64, dir int, size float64) {
	e := &mvEvent{trigger: trigger, key: key, venue: s.venue, sec: sec, dir: dir, size: size,
		bid: s.bid, ask: s.ask, res: map[int64]mvRes{}}
	s.open = append(s.open, e)
	m.done = append(m.done, e)
}

func sign(x float64) int {
	if x < 0 {
		return -1
	}
	return 1
}

// MoveResult is the overreaction study, for the text report and the backtest
// runs page. Money and moves are in cents per contract.
type MoveResult struct {
	LookbackS     int64     `json:"lookback_s"`
	CooldownS     int64     `json:"cooldown_s"`
	MoveCents     float64   `json:"move_cents"`
	FlowWindowS   int64     `json:"flow_window_s"`
	FlowContracts float64   `json:"flow_contracts"`
	HorizonsS     []int64   `json:"horizons_s"` // 0 = hold to settlement
	Settlement    bool      `json:"settlement"` // outcomes were loaded
	MoveEvents    int       `json:"move_events"`
	FlowEvents    int       `json:"flow_events"`
	Rows          []MoveRow `json:"rows"`
}

// MoveRow aggregates events of one trigger, venue and size bucket at one
// horizon. SE values are clustered by market: events inside one game aren't
// independent, so each market counts once toward the uncertainty.
type MoveRow struct {
	Trigger  string  `json:"trigger"` // "move" or "flow"
	Venue    string  `json:"venue"`
	Bucket   string  `json:"bucket"` // size band, e.g. "5–10¢"
	BucketLo float64 `json:"bucket_lo"`
	HorizonS int64   `json:"horizon_s"` // 0 = hold to settlement
	Events   int     `json:"events"`
	Markets  int     `json:"markets"`
	// Cont is how far the mid kept going in the trigger's direction (+ =
	// momentum, − = reversion), before costs.
	Cont   float64 `json:"cont"`
	ContSE float64 `json:"cont_se"`
	// Follow and Fade are the net P&L of a taker round trip with and against
	// the trigger: entry and exit at the real bid/ask, both venues' fees paid.
	Follow   float64 `json:"follow"`
	FollowSE float64 `json:"follow_se"`
	Fade     float64 `json:"fade"`
	FadeSE   float64 `json:"fade_se"`
}

var moveBuckets = []struct {
	lo    float64
	label string
}{{5, "5–10¢"}, {10, "10–20¢"}, {20, "20¢+"}}

var flowBuckets = []struct {
	lo    float64
	label string
}{{20, "20–100"}, {100, "100–500"}, {500, "500+"}}

func bucketOf(trigger string, size float64) (string, float64) {
	bs := moveBuckets
	if trigger == "flow" {
		bs = flowBuckets
	}
	label, lo := bs[0].label, bs[0].lo
	for _, b := range bs {
		if size >= b.lo {
			label, lo = b.label, b.lo
		}
	}
	return label, lo
}

// Result aggregates every event into rows.
func (m *MoveStudy) Result() MoveResult {
	res := MoveResult{
		LookbackS: m.lookback, CooldownS: m.cooldown, MoveCents: m.moveCents,
		FlowWindowS: m.flowWindow, FlowContracts: m.flowContracts,
		HorizonsS: append([]int64{}, m.horizons...), Settlement: len(m.outcomes) > 0,
	}
	if res.Settlement {
		res.HorizonsS = append(res.HorizonsS, 0)
	}
	type sample struct {
		market string
		r      mvRes
	}
	type rowKey struct {
		trigger, venue, bucket string
		lo                     float64
		h                      int64
	}
	groups := map[rowKey][]sample{}
	for _, e := range m.done {
		if e.trigger == "move" {
			res.MoveEvents++
		} else {
			res.FlowEvents++
		}
		if res.Settlement {
			if y, ok := m.outcomes[e.key]; ok {
				pay := 100 * float64(y)
				long := pay - cents(e.ask) - m.feeCents(e.venue, e.ask)
				short := cents(e.bid) - pay - m.feeCents(e.venue, e.bid)
				r := mvRes{cont: (pay - (cents(e.bid)+cents(e.ask))/2) * float64(e.dir), follow: long, fade: short}
				if e.dir < 0 {
					r.follow, r.fade = short, long
				}
				e.res[0] = r
			}
		}
		label, lo := bucketOf(e.trigger, e.size)
		for h, r := range e.res {
			k := rowKey{e.trigger, e.venue, label, lo, h}
			groups[k] = append(groups[k], sample{e.key, r})
		}
	}
	for k, ss := range groups {
		row := MoveRow{Trigger: k.trigger, Venue: k.venue, Bucket: k.bucket, BucketLo: k.lo, HorizonS: k.h, Events: len(ss)}
		pick := []func(mvRes) float64{
			func(r mvRes) float64 { return r.cont },
			func(r mvRes) float64 { return r.follow },
			func(r mvRes) float64 { return r.fade },
		}
		out := make([][2]float64, len(pick))
		markets := map[string]bool{}
		for _, s := range ss {
			markets[s.market] = true
		}
		row.Markets = len(markets)
		for i, f := range pick {
			xs := make([]float64, len(ss))
			ms := make([]string, len(ss))
			for j, s := range ss {
				xs[j], ms[j] = f(s.r), s.market
			}
			out[i][0], out[i][1] = clusteredMean(xs, ms)
		}
		row.Cont, row.ContSE = out[0][0], out[0][1]
		row.Follow, row.FollowSE = out[1][0], out[1][1]
		row.Fade, row.FadeSE = out[2][0], out[2][1]
		res.Rows = append(res.Rows, row)
	}
	sort.Slice(res.Rows, func(i, j int) bool {
		a, b := res.Rows[i], res.Rows[j]
		if a.Trigger != b.Trigger {
			return a.Trigger > b.Trigger // "move" before "flow"
		}
		if a.Venue != b.Venue {
			return a.Venue < b.Venue
		}
		if a.BucketLo != b.BucketLo {
			return a.BucketLo < b.BucketLo
		}
		ha, hb := a.HorizonS, b.HorizonS
		if ha == 0 {
			ha = math.MaxInt64 // settlement last
		}
		if hb == 0 {
			hb = math.MaxInt64
		}
		return ha < hb
	})
	return res
}

// clusteredMean returns the mean of xs and its standard error clustered by
// group (the Liang–Zeger sandwich with the usual small-sample factor). With
// fewer than two groups there's no honest error to give, and SE is 0; callers
// treat Markets < 2 as unreadable.
func clusteredMean(xs []float64, groups []string) (mean, se float64) {
	n := float64(len(xs))
	if n == 0 {
		return 0, 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	mean = sum / n
	resid := map[string]float64{}
	for i, x := range xs {
		resid[groups[i]] += x - mean
	}
	g := float64(len(resid))
	if g < 2 {
		return mean, 0
	}
	var ss float64
	for _, r := range resid {
		ss += r * r
	}
	return mean, math.Sqrt(g/(g-1)*ss) / n
}

// MoveEdge reports whether a row shows a tradeable edge: enough independent
// markets, and a 95% range that clears zero after costs.
func MoveEdge(r MoveRow) (strategy string, ok bool) {
	if r.Markets < 10 || r.Events < 30 {
		return "", false
	}
	if r.Fade-1.96*r.FadeSE > 0 {
		return "fade", true
	}
	if r.Follow-1.96*r.FollowSE > 0 {
		return "follow", true
	}
	return "", false
}

func horizonLabel(h int64) string {
	switch {
	case h == 0:
		return "settle"
	case h >= 60:
		return fmt.Sprintf("%dm", h/60)
	default:
		return fmt.Sprintf("%ds", h)
	}
}

// Report renders the study as text.
func (m *MoveStudy) Report() string {
	res := m.Result()
	var b strings.Builder
	fmt.Fprintf(&b, "OVERREACTION  (after a sharp move or one-sided trading: momentum or reversion?)\n")
	fmt.Fprintf(&b, "  move trigger: mid moves ≥%.0f¢ within %ds;  flow trigger: net taker flow ≥%.0f contracts within %ds;  cooldown %ds\n",
		res.MoveCents, res.LookbackS, res.FlowContracts, res.FlowWindowS, res.CooldownS)
	fmt.Fprintf(&b, "  events: %d moves, %d flow bursts%s\n", res.MoveEvents, res.FlowEvents,
		map[bool]string{true: "", false: "  (no outcomes loaded: no hold-to-settlement horizon)"}[res.Settlement])
	fmt.Fprintf(&b, "  cont = how far the mid kept going (+ momentum, − reversion); follow/fade = net ¢/contract of a taker\n")
	fmt.Fprintf(&b, "  round trip with/against the trigger after spread and fees; ± = 95%% range clustered by market\n\n")
	if len(res.Rows) == 0 {
		b.WriteString("  no events: no market moved enough, or traded one-sidedly enough, to trigger.\n")
		return b.String()
	}
	fmt.Fprintf(&b, "  %-5s %-13s %-8s %-6s %6s %5s %16s %16s %16s\n",
		"trig", "venue", "size", "after", "events", "mkts", "cont", "follow", "fade")
	edges := 0
	for _, r := range res.Rows {
		mark := ""
		if s, ok := MoveEdge(r); ok {
			mark = "  <- " + s + " edge"
			edges++
		}
		ci := func(v, se float64) string {
			if r.Markets < 2 {
				return fmt.Sprintf("%+7.2f  n/a    ", v) // one market: no honest error bar
			}
			return fmt.Sprintf("%+7.2f ±%-7.2f", v, 1.96*se)
		}
		fmt.Fprintf(&b, "  %-5s %-13s %-8s %-6s %6d %5d %s %s %s%s\n",
			r.Trigger, r.Venue, r.Bucket, horizonLabel(r.HorizonS), r.Events, r.Markets,
			ci(r.Cont, r.ContSE), ci(r.Follow, r.FollowSE), ci(r.Fade, r.FadeSE), mark)
	}
	if edges == 0 {
		b.WriteString("\n  no edge: no row with 10+ markets has a follow or fade P&L whose 95% range clears zero.\n")
	} else {
		fmt.Fprintf(&b, "\n  %d row(s) clear zero after costs. Many rows were tested, so expect some by chance —\n"+
			"  check that a candidate holds across neighbouring sizes and horizons, and in each week.\n", edges)
	}
	return b.String()
}
