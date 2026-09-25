// Trade-feed ingestion. Order books alone cannot answer the question that
// decides the maker-rebate thesis: when a resting order fills, does the price
// then move against you (adverse selection)? A book snapshot cannot tell a
// TRADE from a CANCELLATION — the quantity at the touch drops either way — so
// fills are simply not observable from books. Both venues publish trade prints;
// recording them makes adverse selection measurable, and also gives real traded
// volume (true capacity) rather than the depth books merely advertise.
package venue

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"predbot/internal/core"
)

// Trade is a venue-agnostic trade print.
type Trade struct {
	Venue     string     `json:"venue"`
	VenueID   string     `json:"venue_id"`
	Px        core.Price `json:"px"`
	Qty       core.Qty   `json:"qty"`
	TakerSide core.Side  `json:"taker_side"` // which side CROSSED (the aggressor)
	TsServer  int64      `json:"ts_server"`
	TsRecv    int64      `json:"ts_recv"`
}

// TradeHandler receives normalized trades.
type TradeHandler func(Trade)

// KalshiTrades streams Kalshi's public `trade` channel.
// Verified against docs.kalshi.com/websockets/public-trades (2026-08-25):
// channel "trade"; msg carries market_ticker, yes_price_dollars, count_fp,
// taker_side, ts.
type KalshiTrades struct {
	dial Dialer
	rec  RawRecorder
	log  *slog.Logger
}

func NewKalshiTrades(dial Dialer, rec RawRecorder) *KalshiTrades {
	return &KalshiTrades{dial: dial, rec: rec, log: slog.With("component", "kalshi-trades")}
}

type kalshiTradeMsg struct {
	Type string `json:"type"`
	Msg  struct {
		MarketTicker string `json:"market_ticker"`
		YesPrice     string `json:"yes_price_dollars"`
		CountFP      string `json:"count_fp"`
		TakerSide    string `json:"taker_side"` // "yes" | "no"
		TsMs         int64  `json:"ts_ms"`
		Ts           int64  `json:"ts"`
	} `json:"msg"`
}

// Run keeps the trade stream connected until ctx is done.
func (k *KalshiTrades) Run(ctx context.Context, markets []string, h TradeHandler) {
	backoff := NewBackoff(StreamReconnectDelay)
	for ctx.Err() == nil {
		conn, err := k.dial(ctx)
		if err != nil {
			if backoff.Wait(ctx) != nil {
				return
			}
			continue
		}
		backoff.Reset()
		sub, _ := json.Marshal(map[string]any{
			"id": 1, "cmd": "subscribe",
			"params": map[string]any{"channels": []string{"trade"}, "market_tickers": markets},
		})
		if err := conn.Write(ctx, sub); err != nil {
			_ = conn.Close()
			continue
		}
		k.log.Info("trade stream connected", "markets", len(markets))
		for ctx.Err() == nil {
			raw, err := conn.Read(ctx)
			if err != nil {
				break
			}
			now := time.Now()
			_ = k.rec.WriteRaw("kalshi-trades", raw, now)
			var m kalshiTradeMsg
			if json.Unmarshal(raw, &m) != nil || m.Type != "trade" {
				continue
			}
			px, e1 := core.ParsePrice(m.Msg.YesPrice)
			qty, e2 := core.ParseQty(m.Msg.CountFP)
			if e1 != nil || e2 != nil {
				continue
			}
			// taker_side "yes" means the aggressor bought YES.
			side := core.BuyYes
			if m.Msg.TakerSide == "no" {
				side = core.SellYes
			}
			ts := m.Msg.TsMs * int64(time.Millisecond)
			if ts == 0 {
				ts = m.Msg.Ts * int64(time.Second)
			}
			h(Trade{Venue: core.VenueKalshi, VenueID: m.Msg.MarketTicker, Px: px, Qty: qty,
				TakerSide: side, TsServer: ts, TsRecv: now.UnixNano()})
		}
		_ = conn.Close()
	}
}

// PolyUSTrades streams Polymarket US trade prints.
// Verified against docs.polymarket.us/api-reference/websocket/markets (2026-08-25):
// SUBSCRIPTION_TYPE_TRADE; the message carries an explicit maker/taker side,
// which is exactly what adverse-selection analysis needs.
type PolyUSTrades struct {
	dial Dialer
	rec  RawRecorder
	log  *slog.Logger
}

func NewPolyUSTrades(dial Dialer, rec RawRecorder) *PolyUSTrades {
	return &PolyUSTrades{dial: dial, rec: rec, log: slog.With("component", "polyus-trades")}
}

type polyTradeMsg struct {
	SubscriptionType string `json:"subscriptionType"`
	Trade            *struct {
		MarketSlug string `json:"marketSlug"`
		Price      struct {
			Value string `json:"value"`
		} `json:"price"`
		Quantity struct {
			Value string `json:"value"`
		} `json:"quantity"`
		TradeTime string `json:"tradeTime"`
		Taker     struct {
			Side   string `json:"side"`
			Intent string `json:"intent"`
		} `json:"taker"`
	} `json:"trade"`
}

func (p *PolyUSTrades) Run(ctx context.Context, markets []string, h TradeHandler) {
	backoff := NewBackoff(StreamReconnectDelay)
	for ctx.Err() == nil {
		conn, err := p.dial(ctx)
		if err != nil {
			if backoff.Wait(ctx) != nil {
				return
			}
			continue
		}
		backoff.Reset()
		// EXPLICIT slug list: an empty list subscribes to NOTHING on this venue
		// (learned the hard way — see the private fill stream).
		for i := 0; i < len(markets); i += 100 {
			end := min(i+100, len(markets))
			sub, _ := json.Marshal(map[string]any{
				"subscribe": map[string]any{
					"requestId":        fmt.Sprintf("trade-sub-%d", i/100),
					"subscriptionType": "SUBSCRIPTION_TYPE_TRADE",
					"marketSlugs":      markets[i:end],
				},
			})
			_ = conn.Write(ctx, sub)
		}
		p.log.Info("trade stream connected", "markets", len(markets))
		for ctx.Err() == nil {
			raw, err := conn.Read(ctx)
			if err != nil {
				break
			}
			now := time.Now()
			_ = p.rec.WriteRaw("polymarketus-trades", raw, now)
			var m polyTradeMsg
			if json.Unmarshal(raw, &m) != nil || m.Trade == nil {
				continue
			}
			px, e1 := core.ParsePrice(m.Trade.Price.Value)
			qty, e2 := core.ParseQty(m.Trade.Quantity.Value)
			if e1 != nil || e2 != nil {
				continue
			}
			side := core.BuyYes
			if m.Trade.Taker.Intent == "ORDER_INTENT_SELL_LONG" || m.Trade.Taker.Side == "ORDER_SIDE_SELL" {
				side = core.SellYes
			}
			var ts int64
			if t, err := time.Parse(time.RFC3339Nano, m.Trade.TradeTime); err == nil {
				ts = t.UnixNano()
			}
			h(Trade{Venue: core.VenuePoly, VenueID: m.Trade.MarketSlug, Px: px, Qty: qty,
				TakerSide: side, TsServer: ts, TsRecv: now.UnixNano()})
		}
		_ = conn.Close()
	}
}
