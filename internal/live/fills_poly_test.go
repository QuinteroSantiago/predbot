package live

import (
	"encoding/json"
	"testing"

	"predbot/internal/core"
)

// Captured verbatim from wss://api.polymarket.us/v1/ws/private on 2026-08-25
// (trimmed to the fields the parser reads). A marketable buy produces TWO
// frames: EXECUTION_TYPE_NEW with lastShares "0.0000", then EXECUTION_TYPE_FILL
// with the real quantity. Pinned because the shape was previously unverified and
// the stream was dialed on the wrong endpoint, so nothing ever exercised it.
const polyExecNew = `{"requestId":"order-sub-0","subscriptionType":"SUBSCRIPTION_TYPE_ORDER",
"orderSubscriptionUpdate":{"execution":{"id":"C3BVVT8XJDKM",
"order":{"id":"C3BQR9KMEFSH","marketSlug":"cbpac-usfed-2026-cut","intent":"ORDER_INTENT_BUY_LONG"},
"lastShares":"0.0000","lastPx":{"value":"0.0000","currency":"USD"},"type":"EXECUTION_TYPE_NEW",
"commissionNotionalCollected":{"value":"0.0000","currency":"USD"}}}}`

const polyExecFill = `{"requestId":"order-sub-0","subscriptionType":"SUBSCRIPTION_TYPE_ORDER",
"orderSubscriptionUpdate":{"execution":{"id":"C3BVVT8XPDKM","tradeId":"C3BVVT8XMDKM",
"order":{"id":"C3BQR9KMEFSH","marketSlug":"cbpac-usfed-2026-cut","intent":"ORDER_INTENT_BUY_LONG"},
"lastShares":"1.0000","lastPx":{"value":"0.1100","currency":"USD"},"type":"EXECUTION_TYPE_FILL",
"aggressor":true,"commissionNotionalCollected":{"value":"0.0100","currency":"USD"}}}}`

// decodeExec mirrors the parsing in Run so the wire shape is tested directly.
func decodeExec(t *testing.T, raw string) (Fill, bool) {
	t.Helper()
	var env polyPrivateEnvelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.OrderUpdate == nil || env.OrderUpdate.Execution == nil {
		return Fill{}, false
	}
	e := env.OrderUpdate.Execution
	qty, err1 := core.ParseQty(e.LastShares)
	px, err2 := core.ParsePrice(e.LastPx.Value)
	if err1 != nil || err2 != nil || qty == 0 {
		return Fill{}, false
	}
	side := core.BuyYes
	if e.Order.Intent == "ORDER_INTENT_SELL_LONG" || e.Order.Intent == "ORDER_INTENT_BUY_SHORT" {
		side = core.SellYes
	}
	return Fill{Venue: "polymarketus", VenueID: e.Order.MarketSlug,
		ClientOrderID: e.Order.ID, OrderID: e.Order.ID,
		Side: side, Px: px, Qty: qty, FeeUSD: parseDollars(e.Commission.Value)}, true
}

// The NEW acknowledgement carries no quantity and must NOT be booked as a fill —
// otherwise the bot would believe it holds a position the moment it orders.
func TestPolyExecutionNewIsNotAFill(t *testing.T) {
	if _, ok := decodeExec(t, polyExecNew); ok {
		t.Error("EXECUTION_TYPE_NEW (lastShares 0) must not produce a fill")
	}
}

func TestPolyExecutionFillParses(t *testing.T) {
	f, ok := decodeExec(t, polyExecFill)
	if !ok {
		t.Fatal("EXECUTION_TYPE_FILL must produce a fill")
	}
	if f.Qty != 100 { // 1.0000 contracts in 0.01-contract units
		t.Errorf("qty = %v; want 100", f.Qty)
	}
	if f.Px != 1100 { // $0.1100 in $0.0001 units
		t.Errorf("px = %v; want 1100", f.Px)
	}
	if f.Side != core.BuyYes {
		t.Errorf("side = %v; want BuyYes", f.Side)
	}
	if f.VenueID != "cbpac-usfed-2026-cut" {
		t.Errorf("venueID = %q", f.VenueID)
	}
	// Correlation is by exchange order id — this venue has no client_order_id.
	if f.ClientOrderID != "C3BQR9KMEFSH" {
		t.Errorf("clientOrderID = %q; want the exchange order id", f.ClientOrderID)
	}
	// Fees were previously dropped entirely, so the P&L floor saw only gross.
	if f.FeeUSD != 0.01 {
		t.Errorf("feeUSD = %v; want 0.01", f.FeeUSD)
	}
}
