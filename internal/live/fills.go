package live

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"predbot/internal/core"
	"predbot/internal/venue"
)

// Fill is a venue-reported execution, normalized across venues.
type Fill struct {
	Venue         string
	VenueID       string // market ticker / slug
	ClientOrderID string // ties the fill back to the order we placed
	OrderID       string
	Side          core.Side // BuyYes / SellYes
	Px            core.Price
	Qty           core.Qty
	FeeUSD        float64
	IsTaker       bool
	PostPosition  core.Qty // venue's authoritative position AFTER this fill
	TsMs          int64
}

// FillHandler receives normalized fills.
type FillHandler func(Fill)

// KalshiFills subscribes to Kalshi's private `fill` channel and delivers
// normalized fills. Docs (docs.kalshi.com/websockets/user-fills, verified
// 2026-08-24): channel "fill"; msg carries client_order_id, market_ticker,
// yes_price_dollars, count_fp, fee_cost, book_side ("bid"=buy YES / "ask"=sell
// YES), is_taker, post_position_fp. Omitting market_tickers subscribes to ALL
// of the user's fills, which is what we want (we must never miss a fill).
type KalshiFills struct {
	dial venue.Dialer
	log  *slog.Logger
}

func NewKalshiFills(dial venue.Dialer) *KalshiFills {
	return &KalshiFills{dial: dial, log: slog.With("component", "kalshi-fills")}
}

type kalshiFillEnvelope struct {
	Type string `json:"type"`
	Msg  struct {
		TradeID       string `json:"trade_id"`
		OrderID       string `json:"order_id"`
		ClientOrderID string `json:"client_order_id"`
		MarketTicker  string `json:"market_ticker"`
		IsTaker       bool   `json:"is_taker"`
		YesPrice      string `json:"yes_price_dollars"`
		CountFP       string `json:"count_fp"`
		FeeCost       string `json:"fee_cost"`
		BookSide      string `json:"book_side"`
		PostPosition  string `json:"post_position_fp"`
		TsMs          int64  `json:"ts_ms"`
	} `json:"msg"`
}

// Run keeps the private fill stream connected until ctx is done, invoking h for
// every fill. It reconnects with backoff; on reconnect the caller should
// reconcile positions, since fills during the gap were missed.
func (k *KalshiFills) Run(ctx context.Context, h FillHandler) {
	backoff := 500 * time.Millisecond
	for ctx.Err() == nil {
		conn, err := k.dial(ctx)
		if err != nil {
			k.log.Warn("fill stream dial failed", "err", err)
			if sleepCtx(ctx, backoff) != nil {
				return
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = 500 * time.Millisecond
		sub, _ := json.Marshal(map[string]any{
			"id": 1, "cmd": "subscribe",
			"params": map[string]any{"channels": []string{"fill"}},
		})
		if err := conn.Write(ctx, sub); err != nil {
			_ = conn.Close()
			continue
		}
		k.log.Info("private fill stream connected")
		for ctx.Err() == nil {
			raw, err := conn.Read(ctx)
			if err != nil {
				break
			}
			var env kalshiFillEnvelope
			if json.Unmarshal(raw, &env) != nil || env.Type != "fill" {
				continue
			}
			m := env.Msg
			px, err1 := core.ParsePrice(m.YesPrice)
			qty, err2 := core.ParseQty(m.CountFP)
			if err1 != nil || err2 != nil {
				k.log.Warn("unparseable fill", "px", m.YesPrice, "count", m.CountFP)
				continue
			}
			side := core.BuyYes
			if m.BookSide == "ask" { // docs: ask = sell YES
				side = core.SellYes
			}
			post, _ := core.ParseQty(m.PostPosition)
			h(Fill{
				Venue: core.VenueKalshi, VenueID: m.MarketTicker, ClientOrderID: m.ClientOrderID,
				OrderID: m.OrderID, Side: side, Px: px, Qty: qty,
				FeeUSD: parseDollars(m.FeeCost), IsTaker: m.IsTaker,
				PostPosition: post, TsMs: m.TsMs,
			})
		}
		_ = conn.Close()
		k.log.Warn("fill stream disconnected — reconcile positions (fills may have been missed)")
	}
}

func parseDollars(s string) float64 {
	p, err := core.ParsePrice(s)
	if err != nil {
		return 0
	}
	return float64(p) / 1e4
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
