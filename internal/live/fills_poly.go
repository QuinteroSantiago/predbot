package live

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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
}

// NewPolyUSFills builds the stream. slugs must return the CURRENT set of market
// slugs being traded (it is re-read periodically so newly discovered sports
// games are picked up).
func NewPolyUSFills(dial venue.Dialer, slugs func() []string) *PolyUSFills {
	return &PolyUSFills{dial: dial, slugs: slugs, log: slog.With("component", "polyus-fills")}
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
	backoff := 500 * time.Millisecond
	for ctx.Err() == nil {
		conn, err := p.dial(ctx)
		if err != nil {
			p.log.Warn("private stream dial failed", "err", err)
			if sleepCtx(ctx, backoff) != nil {
				return
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = 500 * time.Millisecond
		subscribe := func() int {
			slugs := []string{}
			if p.slugs != nil {
				slugs = p.slugs()
			}
			if len(slugs) == 0 {
				p.log.Error("NO market slugs to subscribe to — fills would be invisible")
				return 0
			}
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
			return len(slugs)
		}
		n := subscribe()
		p.log.Info("private order stream connected", "subscribed_markets", n)

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
			h(Fill{
				Venue: "polymarketus", VenueID: e.Order.MarketSlug,
				// No client_order_id on this venue — correlate by exchange order id.
				ClientOrderID: e.Order.ID, OrderID: e.Order.ID,
				Side: side, Px: px, Qty: qty, TsMs: time.Now().UnixMilli(),
				FeeUSD: parseDollars(e.Commission.Value),
			})
		}
		_ = conn.Close()
		p.log.Warn("private order stream disconnected — reconcile positions (fills may have been missed)")
	}
}
