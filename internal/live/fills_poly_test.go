package live

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"

	"predbot/internal/core"
	"predbot/internal/venue"
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

// --- silent-fill guard: coverage set (2026-08-26) --------------------------

// The gate must default CLOSED (cover nothing until a subscribe lands) and go
// closed again on disconnect — otherwise the trading loop could place a Poly
// order on a slug whose fills we are not streaming, i.e. the naked-leg bug.
func TestPolyFillsCoveredDefaultsClosedAndClears(t *testing.T) {
	pf := NewPolyUSFills(nil, nil)
	if pf.Covers("x") {
		t.Error("a fresh stream must cover nothing")
	}
	pf.setCovered([]string{"x", "y"})
	if !pf.Covers("x") || !pf.Covers("y") {
		t.Error("setCovered should mark the given slugs covered")
	}
	if pf.Covers("z") {
		t.Error("only subscribed slugs are covered")
	}
	pf.clearCovered() // what a disconnect does
	if pf.Covers("x") || pf.Covers("y") {
		t.Error("clearCovered should uncover everything (disconnect => not streaming)")
	}
}

// fakePrivConn is a Conn whose Read blocks until ctx is cancelled (a stable,
// never-closing connection) and whose Write is discarded.
type fakePrivConn struct{ closed chan struct{} }

func (c *fakePrivConn) Read(ctx context.Context) ([]byte, error) {
	<-ctx.Done()
	return nil, io.EOF
}
func (c *fakePrivConn) Write(context.Context, []byte) error { return nil }
func (c *fakePrivConn) Close() error                        { return nil }

func waitUntil(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

// End to end over Run: the initial subscribe covers the current slugs, an
// unknown slug is never covered, and Resubscribe() (the event-driven path) picks
// up a newly added market — the exact case a dynamic sports game hits.
func TestPolyFillsRunCoversAndResubscribes(t *testing.T) {
	var mu sync.Mutex
	slugs := []string{"aec-a", "aec-b"}
	getSlugs := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), slugs...)
	}
	dial := func(context.Context) (venue.Conn, error) { return &fakePrivConn{}, nil }
	pf := NewPolyUSFills(dial, getSlugs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pf.Run(ctx, func(Fill) {})

	waitUntil(t, func() bool { return pf.Covers("aec-a") }, "aec-a covered after initial subscribe")
	if !pf.Covers("aec-b") {
		t.Error("aec-b should be covered by the initial subscribe")
	}
	if pf.Covers("aec-c") {
		t.Error("aec-c is not subscribed and must not be covered")
	}

	// A new market appears (dynamic sports discovery). Without a re-subscribe it
	// would stay uncovered until the periodic ticker; Resubscribe covers it now.
	mu.Lock()
	slugs = append(slugs, "aec-c")
	mu.Unlock()
	pf.Resubscribe()
	waitUntil(t, func() bool { return pf.Covers("aec-c") }, "aec-c covered after Resubscribe")
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
