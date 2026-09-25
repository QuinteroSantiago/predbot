package live

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"predbot/internal/core"
	"predbot/internal/venue"
)

// PolyUSFills subscribes to Polymarket US's private WebSocket order stream and
// delivers normalized fills.
//
// Docs (docs.polymarket.us/api-reference/websocket/private, verified 2026-08-24):
//   - endpoint wss://api.polymarket.us/v1/ws/private, same Ed25519 handshake auth.
//   - subscribe {"subscribe":{requestId, subscriptionType:"SUBSCRIPTION_TYPE_ORDER",
//     marketSlugs:[...]}} — see the CRITICAL note below: the list must be EXPLICIT.
//   - executions arrive as orderSubscriptionUpdate.execution with `lastShares`
//     (decimal STRING), `lastPx` {value,currency}, `type` (EXECUTION_TYPE_*),
//     and the parent `order` (id, marketSlug, intent).
//
// IMPORTANT: Polymarket's create-order API has NO client_order_id field, so
// fills are correlated by the EXCHANGE-ASSIGNED order id returned from Place()
// — unlike Kalshi, which echoes our client_order_id.
//
// CRITICAL (found the hard way 2026-08-24): the docs say "leave marketSlugs
// empty to subscribe to all markets", but an EMPTY LIST SUBSCRIBES TO NOTHING.
// The connection succeeds, the subscription registers, and no execution is ever
// delivered — the bot filled an order and never knew. Always subscribe with an
// EXPLICIT slug list, and re-subscribe when the traded set changes.
//
// SUBSCRIPTION_TYPE_ORDER does not ack, so ORDER_SNAPSHOT is sent alongside to
// get a liveness signal (it acks with the current open orders, or an empty list).
//
// ROOT CAUSE, 2026-08-25: this stream must be dialed on /v1/ws/private. It was
// being handed the MARKET-DATA dialer (/v1/ws/markets), which connects,
// authenticates, and accepts the subscribe — then never emits an execution. That
// silence was mistaken for a venue quirk for days while the bot filled orders it
// could not see, leaving a naked leg every time. Verified against the live venue:
// on /v1/ws/private a marketable order produces EXECUTION_TYPE_NEW followed by
// EXECUTION_TYPE_FILL (lastShares/lastPx populated) within ~1ms.
//
// If executions ever stop arriving again, check the ENDPOINT before the parser.
// The DriftMonitor remains the authoritative backstop either way.
type PolyUSFills struct {
	dial  venue.Dialer
	slugs func() []string // markets to subscribe to; re-read on each refresh
	log   *slog.Logger

	// poke requests an immediate re-subscribe (coalesced, size 1) so a newly
	// traded slug is covered without waiting for the periodic ticker.
	poke chan struct{}

	// everCovered flips true the first time we subscribe to a non-empty slug set.
	// An empty set BEFORE that is expected — startup and off-hours simply have no
	// MLB games discovered yet, and with zero tradeable markets there are zero
	// orders to miss. An empty set AFTER we've had coverage is a real regression
	// (we lost the markets we were watching), so only that case is loud.
	everCovered atomic.Bool

	mu sync.Mutex
	// covered is the slug set the stream has subscribed to on the CURRENT live
	// connection. Empty while disconnected. The trading loop gates Polymarket
	// orders on this: placing on an uncovered slug fills with no execution event
	// = a silent fill / naked leg (the 2026-08-25/-26 incidents).
	covered map[string]bool
}

// NewPolyUSFills builds the stream. slugs must return the CURRENT set of market
// slugs being traded (it is re-read on each subscribe so newly discovered sports
// games are picked up).
func NewPolyUSFills(dial venue.Dialer, slugs func() []string) *PolyUSFills {
	return &PolyUSFills{dial: dial, slugs: slugs,
		poke:    make(chan struct{}, 1),
		covered: map[string]bool{},
		log:     slog.With("component", "polyus-fills")}
}

// Covers reports whether the fill stream is currently subscribed to slug on the
// live connection. Returns false while disconnected or before the first
// subscribe. The trading loop MUST gate Polymarket orders on this.
func (p *PolyUSFills) Covers(slug string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.covered[slug]
}

// Resubscribe requests an immediate (re)subscribe to the current market set.
// Non-blocking and coalescing: call it whenever the traded set changes (e.g. a
// new sports game) so a freshly added slug becomes covered promptly instead of
// waiting up to the periodic re-subscribe interval.
func (p *PolyUSFills) Resubscribe() {
	select {
	case p.poke <- struct{}{}:
	default:
	}
}

func (p *PolyUSFills) setCovered(slugs []string) {
	next := make(map[string]bool, len(slugs))
	for _, s := range slugs {
		next[s] = true
	}
	p.mu.Lock()
	p.covered = next
	p.mu.Unlock()
}

func (p *PolyUSFills) clearCovered() {
	p.mu.Lock()
	p.covered = map[string]bool{}
	p.mu.Unlock()
}

type polyPrivateEnvelope struct {
	SubscriptionType string `json:"subscriptionType"`
	OrderUpdate      *struct {
		Execution *struct {
			ID         string `json:"id"`
			LastShares string `json:"lastShares"`
			LastPx     struct {
				Value string `json:"value"`
			} `json:"lastPx"`
			Type    string `json:"type"`
			TradeID string `json:"tradeId"`
			// The venue reports the fee per execution. Without this the P&L
			// floor only ever saw gross P&L for Polymarket legs.
			Commission struct {
				Value string `json:"value"`
			} `json:"commissionNotionalCollected"`
			Order struct {
				ID         string `json:"id"`
				MarketSlug string `json:"marketSlug"`
				Intent     string `json:"intent"`
			} `json:"order"`
		} `json:"execution"`
	} `json:"orderSubscriptionUpdate"`
}

// Run keeps the private order stream connected until ctx is done, invoking h for
// every execution. On reconnect the caller should reconcile positions, since
// executions during the gap were missed.
func (p *PolyUSFills) Run(ctx context.Context, h FillHandler) {
	backoff := venue.NewBackoff(venue.StreamReconnectDelay)
	var downSince time.Time // zero until the first disconnect; the blind window
	for ctx.Err() == nil {
		conn, err := p.dial(ctx)
		if err != nil {
			p.clearCovered() // not subscribed to anything while down
			p.log.Warn("private stream dial failed", "err", err)
			if backoff.Wait(ctx) != nil {
				return
			}
			continue
		}
		backoff.Reset()
		subscribe := func() int {
			slugs := []string{}
			if p.slugs != nil {
				slugs = p.slugs()
			}
			if len(slugs) == 0 {
				if p.everCovered.Load() {
					// We were watching markets and now have none — genuine loss of
					// coverage while the bot may still hold/trade a leg.
					p.log.Error("market slug set went EMPTY after coverage — fills would be invisible")
				} else {
					// No games discovered yet (startup / off-hours). Nothing to trade,
					// so nothing to miss; the periodic re-subscribe will pick games up.
					p.log.Info("no markets to subscribe yet — none discovered", "subscribed_markets", 0)
				}
				return 0
			}
			p.everCovered.Store(true)
			// Chunk to stay within the documented 100-market subscription limit.
			for i := 0; i < len(slugs); i += 100 {
				end := min(i+100, len(slugs))
				sub, _ := json.Marshal(map[string]any{
					"subscribe": map[string]any{
						"requestId":        fmt.Sprintf("order-sub-%d", i/100),
						"subscriptionType": "SUBSCRIPTION_TYPE_ORDER",
						"marketSlugs":      slugs[i:end], // EXPLICIT — empty means NOTHING
					},
				})
				_ = conn.Write(ctx, sub)
			}
			// ORDER_SNAPSHOT acks, giving us a liveness signal for the connection.
			snap, _ := json.Marshal(map[string]any{
				"subscribe": map[string]any{
					"requestId": "order-snap", "subscriptionType": "SUBSCRIPTION_TYPE_ORDER_SNAPSHOT",
				},
			})
			_ = conn.Write(ctx, snap)
			// Mark these slugs covered only AFTER the subscribe is on the wire, so
			// the trading-loop gate never sees a slug as covered before we asked
			// the venue to stream its fills.
			p.setCovered(slugs)
			return len(slugs)
		}
		n := subscribe()
		if downSince.IsZero() {
			p.log.Info("private order stream connected", "subscribed_markets", n)
		} else {
			// The gap between disconnect and here is a window in which a resting
			// leg could have filled with no execution delivered — RECONCILE now.
			p.log.Info("private order stream reconnected — reconcile positions; fills in the gap were invisible",
				"subscribed_markets", n, "down_for", time.Since(downSince).Round(time.Millisecond).String())
		}

		// Re-subscribe periodically so newly discovered markets are covered.
		reSub, stopReSub := context.WithCancel(ctx)
		go func() {
			t := time.NewTicker(2 * time.Minute)
			defer t.Stop()
			for {
				select {
				case <-reSub.Done():
					return
				case <-t.C:
					subscribe()
				case <-p.poke:
					// Event-driven: the traded set changed (e.g. a new sports
					// game), so cover it now instead of waiting for the ticker.
					subscribe()
				}
			}
		}()
		defer stopReSub()
		for ctx.Err() == nil {
			raw, err := conn.Read(ctx)
			if err != nil {
				break
			}
			var env polyPrivateEnvelope
			if json.Unmarshal(raw, &env) != nil || env.OrderUpdate == nil || env.OrderUpdate.Execution == nil {
				continue
			}
			e := env.OrderUpdate.Execution
			qty, err1 := core.ParseQty(e.LastShares)
			px, err2 := core.ParsePrice(e.LastPx.Value)
			if err1 != nil || err2 != nil || qty == 0 {
				continue
			}
			side := core.BuyYes
			if e.Order.Intent == "ORDER_INTENT_SELL_LONG" || e.Order.Intent == "ORDER_INTENT_BUY_SHORT" {
				side = core.SellYes
			}
			// Log every execution we actually see. If a drift/reconcile kill later
			// finds a Poly position with NO matching line here, the fill arrived
			// while the stream was down (see the reconnect "down_for" below) — the
			// naked-leg signature. A line here but no hedge points at the hedge
			// path instead.
			p.log.Info("private order execution",
				"market", e.Order.MarketSlug, "order_id", e.Order.ID,
				"side", side.String(), "qty", qty.String(), "px", px.String(),
				"exec_type", e.Type)
			h(Fill{
				Venue: core.VenuePoly, VenueID: e.Order.MarketSlug,
				// No client_order_id on this venue — correlate by exchange order id.
				ClientOrderID: e.Order.ID, OrderID: e.Order.ID,
				Side: side, Px: px, Qty: qty, TsMs: time.Now().UnixMilli(),
				FeeUSD: parseDollars(e.Commission.Value),
			})
		}
		_ = conn.Close()
		p.clearCovered() // subscriptions die with the connection
		downSince = time.Now()
		p.log.Warn("private order stream disconnected — reconcile positions (fills may have been missed)")
	}
}
