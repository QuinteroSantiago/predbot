// Package polymarketus ingests Polymarket US market data: a full top-of-book
// snapshot on every update, where dropping a stale snapshot under backpressure
// is correct because the next message supersedes it entirely (design §6.2,
// §7.2). One instrument per market (YES); NO is derived. Only top levels are
// carried, which the backtester's fill model treats as the depth ceiling.
//
// This is Polymarket US (api.polymarket.us, CFTC-regulated) — NOT Polymarket.com
// (design §1). Field names follow the verified-2026-08-22 facts, [VERIFY] before
// Phase 4.
package polymarketus

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"predbot/internal/core"
	"predbot/internal/obs"
	"predbot/internal/venue"
)

const name = "polymarketus"

// Ingester streams Polymarket US order books.
type Ingester struct {
	dial venue.Dialer
	rec  venue.RawRecorder
	met  *obs.Metrics
	log  *slog.Logger
}

func New(dial venue.Dialer, rec venue.RawRecorder, met *obs.Metrics) *Ingester {
	return &Ingester{dial: dial, rec: rec, met: met, log: slog.With("venue", name)}
}

func (p *Ingester) Name() string { return name }

// Stream connects, subscribes (batches of <=100 slugs, design §6.2), and emits
// normalized snapshots until ctx is done, reconnecting with backoff.
func (p *Ingester) Stream(ctx context.Context, markets []string, out chan<- core.BookSnapshot) error {
	backoff := 250 * time.Millisecond
	for ctx.Err() == nil {
		conn, err := p.dial(ctx)
		if err != nil {
			p.log.Warn("dial failed", "err", err, "backoff", backoff)
			if sleepCtx(ctx, backoff) != nil {
				return ctx.Err()
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = 250 * time.Millisecond
		if err := p.subscribe(ctx, conn, markets); err != nil {
			p.log.Warn("subscribe failed", "err", err)
			_ = conn.Close()
			continue
		}
		err = p.readLoop(ctx, conn, out)
		p.met.Reconnects.Add(ctx, 1, obs.Venue(name))
		_ = conn.Close()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		p.log.Info("disconnected, reconnecting", "err", err)
	}
	return ctx.Err()
}

// subscribe requests MARKET_DATA for up to 100 slugs per subscription. For >100
// markets it sends multiple subscribe frames on the same connection (design §6.2).
// Wire format per docs.polymarket.us: everything nests under a "subscribe" key.
func (p *Ingester) subscribe(ctx context.Context, c venue.Conn, markets []string) error {
	const batch = 100
	for i := 0; i < len(markets); i += batch {
		end := min(i+batch, len(markets))
		req := map[string]any{
			"subscribe": map[string]any{
				"requestId":        fmt.Sprintf("md-sub-%d", i/batch),
				"subscriptionType": "SUBSCRIPTION_TYPE_MARKET_DATA",
				"marketSlugs":      markets[i:end],
			},
		}
		b, _ := json.Marshal(req)
		if err := c.Write(ctx, b); err != nil {
			return err
		}
	}
	return nil
}

func (p *Ingester) readLoop(ctx context.Context, c venue.Conn, out chan<- core.BookSnapshot) error {
	for {
		raw, err := c.Read(ctx)
		if err != nil {
			return err
		}
		now := time.Now()
		_ = p.rec.WriteRaw(name, raw, now)

		snap, ok, perr := ParseMarketData(raw, now)
		if perr != nil {
			p.met.ParseErrors.Add(ctx, 1, obs.Venue(name))
			p.log.Debug("frame parse error", "err", perr)
			continue
		}
		if !ok {
			continue // not a market-data frame
		}
		if snap.TsServer != 0 {
			p.met.FeedLatency.Record(ctx, float64(now.UnixNano()-snap.TsServer)/1e9, obs.Venue(name))
		}
		select {
		case out <- snap:
			p.met.SnapshotsTotal.Add(ctx, 1, obs.Venue(name))
		case <-ctx.Done():
			return ctx.Err()
		default:
			// Stale full snapshot; the next message supersedes it. Dropping is
			// correct here (design §7.2), unlike Kalshi's delta stream.
			p.met.Dropped.Add(ctx, 1, obs.Venue(name))
		}
	}
}

type pxField struct {
	Value    json.Number `json:"value"`
	Currency string      `json:"currency"`
}

type levelMsg struct {
	Px  pxField     `json:"px"`
	Qty json.Number `json:"qty"`
}

type marketData struct {
	Type   string          `json:"type"`
	Slug   string          `json:"marketSlug"`
	Bids   []levelMsg      `json:"bids"`
	Offers []levelMsg      `json:"offers"`
	State  string          `json:"state"`
	// transactTime is an RFC3339 string on the public gateway
	// (e.g. "2026-08-22T17:05:58.304480956Z") but may be epoch ms on the WS
	// feed [VERIFY]; parseTransactTime accepts either.
	TransactTime json.RawMessage `json:"transactTime"`
}

// ParseMarketData turns a raw Polymarket US frame into a normalized snapshot.
// ok=false means the frame is not market data (e.g. an ack or a TRADE). Already
// YES-centric: bids -> YesBids, offers -> YesAsks (design §6.3).
func ParseMarketData(raw []byte, now time.Time) (snap core.BookSnapshot, ok bool, err error) {
	// The WS data frame wraps the book under "marketData"
	// ({requestId, subscriptionType, marketData:{...}}); the REST caller already
	// passes the inner object. Unwrap when the envelope is present.
	var env struct {
		MarketData json.RawMessage `json:"marketData"`
	}
	if json.Unmarshal(raw, &env) == nil && len(env.MarketData) > 0 {
		raw = env.MarketData
	}
	var m marketData
	if err = json.Unmarshal(raw, &m); err != nil {
		return snap, false, err
	}
	// Accept the documented MARKET_DATA type; ignore lite/trade/ack frames.
	if m.Type != "" && m.Type != "MARKET_DATA" && m.Type != "SUBSCRIPTION_TYPE_MARKET_DATA" {
		return snap, false, nil
	}
	if m.Slug == "" || (m.Bids == nil && m.Offers == nil) {
		return snap, false, nil
	}

	yesBids, err := levels(m.Bids)
	if err != nil {
		return snap, false, err
	}
	yesAsks, err := levels(m.Offers)
	if err != nil {
		return snap, false, err
	}
	return core.BookSnapshot{
		Venue:    name,
		VenueID:  m.Slug,
		YesBids:  yesBids, // sent best-first (descending) by the venue
		YesAsks:  yesAsks, // sent best-first (ascending) by the venue
		TsServer: parseTransactTime(m.TransactTime),
		TsRecv:   now.UnixNano(),
		Seq:      0, // Polymarket has no sequence number
		State:    parseState(m.State),
	}, true, nil
}

func levels(in []levelMsg) ([]core.Level, error) {
	out := make([]core.Level, 0, len(in))
	for _, lv := range in {
		px, err := core.ParsePrice(lv.Px.Value.String())
		if err != nil {
			return nil, err
		}
		qty, err := core.ParseQty(lv.Qty.String())
		if err != nil {
			return nil, err
		}
		out = append(out, core.Level{Px: px, Qty: qty})
	}
	return out, nil
}

// parseTransactTime accepts an RFC3339 string (public gateway) or an epoch-ms
// number (assumed WS form), returning unix nanos; 0 when absent/unparseable.
func parseTransactTime(raw json.RawMessage) int64 {
	if len(raw) == 0 || string(raw) == "null" {
		return 0
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t.UnixNano()
		}
		return 0
	}
	var ms int64
	if err := json.Unmarshal(raw, &ms); err == nil && ms != 0 {
		return ms * int64(time.Millisecond)
	}
	return 0
}

func parseState(s string) core.MarketState {
	// The gateway prefixes states with MARKET_STATE_; the WS feed may use short
	// forms [VERIFY]. Normalize both.
	s = strings.TrimPrefix(strings.ToLower(s), "market_state_")
	switch s {
	case "open", "active":
		return core.StateOpen
	case "paused":
		return core.StatePaused
	case "halted", "suspended":
		return core.StateHalted
	case "closed", "settled", "resolved":
		return core.StateClosed
	default:
		return core.StateUnknown
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
