// Package kalshi ingests Kalshi's order-book feed: a sequence-numbered delta
// stream that must never be dropped (design §6.1, §7.1). The ingester owns the
// authoritative raw book; the *derived* snapshot may be conflated under
// backpressure because the raw book stays correct. A sequence gap triggers a
// snapshot resync, not a reconnect; a reconnect discards all books because seq
// restarts.
//
// Field names and the fixed-point conventions below follow the design doc's
// verified-2026-08-22 facts and are marked [VERIFY] there: re-check against live
// Kalshi docs before Phase 4. In particular this assumes `*_dollars_fp` levels
// are `[price_dollars_string, qty_fixedpoint_int]` where qty is already in
// 0.01-contract units (core.Qty) and delta_fp is a signed value in the same
// units.
package kalshi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"time"

	"predbot/internal/core"
	"predbot/internal/obs"
	"predbot/internal/venue"
)

const name = "kalshi"

// Ingester streams Kalshi order books.
type Ingester struct {
	dial venue.Dialer
	rec  venue.RawRecorder
	met  *obs.Metrics
	log  *slog.Logger
}

// New builds a Kalshi ingester. dial supplies an authenticated connection.
func New(dial venue.Dialer, rec venue.RawRecorder, met *obs.Metrics) *Ingester {
	return &Ingester{dial: dial, rec: rec, met: met, log: slog.With("venue", name)}
}

func (k *Ingester) Name() string { return name }

// Stream connects, subscribes, and emits normalized snapshots until ctx is
// done. It reconnects with exponential backoff; every reconnect discards all
// books because Kalshi's seq restarts (design §7.1).
func (k *Ingester) Stream(ctx context.Context, markets []string, out chan<- core.BookSnapshot) error {
	backoff := 250 * time.Millisecond
	for ctx.Err() == nil {
		conn, err := k.dial(ctx)
		if err != nil {
			k.log.Warn("dial failed", "err", err, "backoff", backoff)
			if sleep(ctx, backoff) != nil {
				return ctx.Err()
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = 250 * time.Millisecond

		st := newState() // discard everything on (re)connect
		if err := k.subscribe(ctx, conn, markets); err != nil {
			k.log.Warn("subscribe failed", "err", err)
			_ = conn.Close()
			continue
		}
		err = k.readLoop(ctx, conn, st, out)
		k.met.Reconnects.Add(ctx, 1, obs.Venue(name))
		_ = conn.Close()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		k.log.Info("disconnected, reconnecting", "err", err)
	}
	return ctx.Err()
}

// subscribe sends the orderbook subscription request. Wire format follows the
// documented `update_subscription` shape; [VERIFY] against live docs.
func (k *Ingester) subscribe(ctx context.Context, c venue.Conn, markets []string) error {
	req := map[string]any{
		"id":  1,
		"cmd": "subscribe",
		"params": map[string]any{
			"channels":       []string{"orderbook_delta"},
			"market_tickers": markets,
		},
	}
	b, _ := json.Marshal(req)
	return c.Write(ctx, b)
}

// readLoop reads frames, records them raw, applies them to the owned books, and
// emits normalized snapshots. See ProcessFrame for the per-frame decision.
func (k *Ingester) readLoop(ctx context.Context, c venue.Conn, st *State, out chan<- core.BookSnapshot) error {
	for {
		raw, err := c.Read(ctx)
		if err != nil {
			return err
		}
		now := time.Now()
		_ = k.rec.WriteRaw(name, raw, now) // record raw first: ground truth

		snap, emit, perr := st.ProcessFrame(raw, now)
		if perr == errSeqGap {
			// Kalshi's seq is per-connection; the only resync is a reconnect,
			// which re-subscribes and re-snapshots everything (design §7.1).
			k.met.SeqGaps.Add(ctx, 1, obs.Venue(name))
			return errSeqGap
		}
		if perr != nil {
			k.met.ParseErrors.Add(ctx, 1, obs.Venue(name))
			k.log.Debug("frame parse error", "err", perr)
			continue
		}
		if !emit {
			continue
		}
		if snap.TsServer != 0 {
			k.met.FeedLatency.Record(ctx, float64(now.UnixNano()-snap.TsServer)/1e9, obs.Venue(name))
		}
		select {
		case out <- snap:
			k.met.SnapshotsTotal.Add(ctx, 1, obs.Venue(name))
		case <-ctx.Done():
			return ctx.Err()
		default:
			// Consumer is behind. Skipping this emit is safe: we already applied
			// the delta to the authoritative book, so the next emit carries the
			// latest state. This is conflation, not data loss (design §7.1).
			k.met.Conflated.Add(ctx, 1, obs.Venue(name))
		}
	}
}

// errSeqGap signals a break in the global sequence; the read loop reconnects
// (which re-subscribes and re-snapshots), because Kalshi's seq is per-connection
// and cannot be resynced per-market.
var errSeqGap = errors.New("kalshi: sequence gap, resync via reconnect")

// State holds the authoritative raw books and the connection's expected sequence.
// Kalshi numbers messages with a SINGLE monotonic seq per subscription (sid),
// shared across all markets — NOT per market_ticker — so we track one counter.
// It is single-writer: only the read loop touches it.
type State struct {
	books     map[string]*rawBook
	expectSeq uint64 // next expected global seq; 0 = not yet initialized
}

func newState() *State {
	return &State{books: map[string]*rawBook{}}
}

// NewState exposes a fresh State for tests.
func NewState() *State { return newState() }

// envelope is the Kalshi WS message wrapper.
type envelope struct {
	Type string          `json:"type"`
	Seq  uint64          `json:"seq"`
	Sid  uint64          `json:"sid"`
	Msg  json.RawMessage `json:"msg"`
}

type levelFP [2]json.RawMessage // [price_dollars_string, qty_fp]

type snapMsg struct {
	Ticker string    `json:"market_ticker"`
	Yes    []levelFP `json:"yes_dollars_fp"`
	No     []levelFP `json:"no_dollars_fp"`
	TsMs   int64     `json:"ts_ms"`
	Status string    `json:"status"`
}

type deltaMsg struct {
	Ticker  string          `json:"market_ticker"`
	Price   json.RawMessage `json:"price_dollars"`
	DeltaFP json.Number     `json:"delta_fp"`
	Side    string          `json:"side"` // "yes" | "no"
	TsMs    int64           `json:"ts_ms"`
}

// ProcessFrame applies one raw frame to the books and reports what to do:
//   - snap/emit=true: a fresh normalized snapshot to publish.
//   - err == errSeqGap: a break in the global sequence; the caller must
//     reconnect (Kalshi's seq is per-connection and restarts on reconnect).
//   - emit=false, err==nil: a frame we ignore (e.g. non-book).
//
// It never returns a book that shares memory with State.
func (s *State) ProcessFrame(raw []byte, now time.Time) (snap core.BookSnapshot, emit bool, err error) {
	var env envelope
	if err = json.Unmarshal(raw, &env); err != nil {
		return snap, false, err
	}
	switch env.Type {
	case "orderbook_snapshot":
		var m snapMsg
		if err = json.Unmarshal(env.Msg, &m); err != nil {
			return snap, false, err
		}
		if s.expectSeq != 0 && env.Seq != s.expectSeq {
			return snap, false, errSeqGap
		}
		b := newRawBook()
		if err = b.applySnapshot(m); err != nil {
			return snap, false, err
		}
		s.books[m.Ticker] = b
		s.expectSeq = env.Seq + 1
		return b.normalize(m.Ticker, env.Seq, now), true, nil

	case "orderbook_delta":
		var m deltaMsg
		if err = json.Unmarshal(env.Msg, &m); err != nil {
			return snap, false, err
		}
		b := s.books[m.Ticker]
		if b == nil || (s.expectSeq != 0 && env.Seq != s.expectSeq) {
			return snap, false, errSeqGap // delta before snapshot, or a seq gap
		}
		s.expectSeq = env.Seq + 1
		if err = b.applyDelta(m); err != nil {
			return snap, false, err
		}
		return b.normalize(m.Ticker, env.Seq, now), true, nil

	default:
		return snap, false, nil
	}
}

// rawBook is Kalshi's native two-sided representation: yes-bids and no-bids.
type rawBook struct {
	yes      map[core.Price]core.Qty // YES bids
	no       map[core.Price]core.Qty // NO bids
	tsServer int64
	state    core.MarketState
}

func newRawBook() *rawBook {
	return &rawBook{yes: map[core.Price]core.Qty{}, no: map[core.Price]core.Qty{}}
}

func (b *rawBook) applySnapshot(m snapMsg) error {
	b.yes = map[core.Price]core.Qty{}
	b.no = map[core.Price]core.Qty{}
	b.tsServer = msToNanos(m.TsMs)
	b.state = parseState(m.Status)
	if err := loadLevels(b.yes, m.Yes); err != nil {
		return err
	}
	return loadLevels(b.no, m.No)
}

func (b *rawBook) applyDelta(m deltaMsg) error {
	px, err := parsePriceField(m.Price)
	if err != nil {
		return err
	}
	// delta_fp is a signed decimal contract delta.
	d, err := core.ParseQty(m.DeltaFP.String())
	if err != nil {
		return err
	}
	if m.TsMs != 0 {
		b.tsServer = msToNanos(m.TsMs)
	}
	side := b.yes
	if m.Side == "no" {
		side = b.no
	}
	q := side[px] + d
	if q <= 0 {
		delete(side, px) // level emptied
	} else {
		side[px] = q
	}
	return nil
}

// normalize renders the YES-centric view (design §6.3): YES bids as-is
// (descending), YES asks derived from NO bids as (OneDollar - p) ascending.
func (b *rawBook) normalize(ticker string, seq uint64, now time.Time) core.BookSnapshot {
	yesBids := make([]core.Level, 0, len(b.yes))
	for px, q := range b.yes {
		yesBids = append(yesBids, core.Level{Px: px, Qty: q})
	}
	sort.Slice(yesBids, func(i, j int) bool { return yesBids[i].Px > yesBids[j].Px })

	yesAsks := make([]core.Level, 0, len(b.no))
	for px, q := range b.no {
		yesAsks = append(yesAsks, core.Level{Px: core.OneDollar - px, Qty: q})
	}
	sort.Slice(yesAsks, func(i, j int) bool { return yesAsks[i].Px < yesAsks[j].Px })

	return core.BookSnapshot{
		Venue:    name,
		VenueID:  ticker,
		YesBids:  yesBids,
		YesAsks:  yesAsks,
		TsServer: b.tsServer,
		TsRecv:   now.UnixNano(),
		Seq:      seq,
		State:    b.state,
	}
}

func loadLevels(dst map[core.Price]core.Qty, levels []levelFP) error {
	for _, lv := range levels {
		px, err := parsePriceField(lv[0])
		if err != nil {
			return err
		}
		// Qty is a decimal contract string ("10306701.00"), not an integer.
		q, err := parseQtyField(lv[1])
		if err != nil {
			return err
		}
		if q > 0 {
			dst[px] = q
		}
	}
	return nil
}

// parsePriceField accepts either a JSON string ("0.55") or a JSON number.
func parsePriceField(raw json.RawMessage) (core.Price, error) {
	return core.ParsePrice(rawNumString(raw))
}

// parseQtyField parses a (possibly signed) decimal contract quantity.
func parseQtyField(raw json.RawMessage) (core.Qty, error) {
	return core.ParseQty(rawNumString(raw))
}

// rawNumString returns the string form of a JSON value that may be encoded as a
// quoted string or a bare number.
func rawNumString(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}

func msToNanos(ms int64) int64 {
	if ms == 0 {
		return 0
	}
	return ms * int64(time.Millisecond)
}

func parseState(s string) core.MarketState {
	switch s {
	case "active", "open":
		return core.StateOpen
	case "paused":
		return core.StatePaused
	case "halted":
		return core.StateHalted
	case "closed", "settled", "finalized":
		return core.StateClosed
	default:
		return core.StateUnknown
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
