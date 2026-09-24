package exec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"predbot/internal/core"
)

// PolyUSExecutor places orders on Polymarket US via POST /v1/orders.
//
// Wire format verified against docs.polymarket.us (2026-08-23):
//   - auth: X-PM-Access-Key / X-PM-Timestamp / X-PM-Signature, Ed25519 over
//     "{timestamp}{METHOD}{path}" — same scheme as the market-data WS.
//   - body: {marketSlug, type, price:{value,currency}, quantity, tif, intent}
//   - intent: ORDER_INTENT_BUY_LONG = buy YES, ORDER_INTENT_SELL_LONG = sell YES.
//   - quantity is in CONTRACTS (decimals allowed where minimumTradeQty < 1).
//   - price.value is a decimal string in dollars, e.g. "0.55".
//
// A browser-ish User-Agent is required: the WAF 403s (error 1010) the default
// Go/py agent even with valid auth.
type PolyUSExecutor struct {
	base   string
	sign   func(method, path string) (http.Header, error)
	client *http.Client

	// markets remembers each placed order's market slug: the cancel endpoint
	// requires it, and callers of the Executor interface only hold the id.
	mu      sync.Mutex
	markets map[string]string // venue order id -> market slug
}

func NewPolyUSExecutor(base string, sign func(method, path string) (http.Header, error)) *PolyUSExecutor {
	return &PolyUSExecutor{base: base, sign: sign, client: &http.Client{Timeout: 5 * time.Second},
		markets: map[string]string{}}
}

func (p *PolyUSExecutor) Name() string { return "polymarketus" }

type polyAmount struct {
	Value    string `json:"value"`
	Currency string `json:"currency"`
}

type polyCreateOrder struct {
	MarketSlug string     `json:"marketSlug"`
	Type       string     `json:"type"`
	Price      polyAmount `json:"price"`
	Quantity   float64    `json:"quantity"`
	TIF        string     `json:"tif"`
	Intent     string     `json:"intent"`
	Manual     string     `json:"manualOrderIndicator,omitempty"`
	// PostOnly (participateDontInitiate): the venue rejects the order if it
	// would match on arrival. Set on every resting order, so a "rest" priced
	// through the book can never fill as a taker (2026-09-24, CHW).
	PostOnly bool `json:"participateDontInitiate,omitempty"`
}

func polyIntent(side core.Side) string {
	if side == core.SellYes {
		return "ORDER_INTENT_SELL_LONG" // sell YES
	}
	return "ORDER_INTENT_BUY_LONG" // buy YES
}

func polyTIF(t core.TIF) string {
	switch t {
	case core.GTC:
		return "TIME_IN_FORCE_GOOD_TILL_CANCEL"
	case core.GTD:
		return "TIME_IN_FORCE_GOOD_TILL_DATE"
	case core.FOK:
		return "TIME_IN_FORCE_FILL_OR_KILL"
	default:
		return "TIME_IN_FORCE_IMMEDIATE_OR_CANCEL"
	}
}

// body builds the create-order payload for an order (also used by Preview).
func (p *PolyUSExecutor) body(o core.Order) polyCreateOrder {
	return polyCreateOrder{
		MarketSlug: o.VenueID,
		Type:       "ORDER_TYPE_LIMIT",
		Price:      polyAmount{Value: o.Px.String(), Currency: "USD"},
		Quantity:   float64(o.Qty) / 100, // 0.01-contract units -> contracts
		TIF:        polyTIF(o.TIF),
		Intent:     polyIntent(o.Side),
		Manual:     "MANUAL_ORDER_INDICATOR_AUTOMATED",
		PostOnly:   o.TIF == core.GTC || o.TIF == core.GTD,
	}
}

// Preview validates an order WITHOUT placing it. Use it to verify
// market/side/price/size semantics before ever sending a real order.
// Docs: POST /v1/order/preview (singular "order"), and the body nests the
// CreateOrderRequest under a "request" key — unlike POST /v1/orders.
func (p *PolyUSExecutor) Preview(ctx context.Context, o core.Order) (string, error) {
	return p.post(ctx, "/v1/order/preview", map[string]any{"request": p.body(o)})
}

// Place submits a real order. On timeout/5xx it returns an error and does NOT
// resubmit — the caller reconciles against open orders first (design §11).
func (p *PolyUSExecutor) Place(ctx context.Context, o core.Order) (string, error) {
	raw, err := p.post(ctx, "/v1/orders", p.body(o))
	if err != nil {
		return "", err
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return "", fmt.Errorf("polymarketus: parse create response: %w", err)
	}
	p.mu.Lock()
	p.markets[out.ID] = o.VenueID
	p.mu.Unlock()
	return out.ID, nil
}

// Cancel cancels one order by its exchange-assigned id.
//
// Docs (verified 2026-08-25): POST /v1/order/{orderId}/cancel with a
// {"marketSlug":...} body. NOT DELETE /v1/orders/{id} — that was an unverified
// guess and returned 404 against the live venue, so cancels silently never
// worked. Note the path segment is SINGULAR ("order"), unlike the plural
// "/v1/orders" used to place.
//
// The market slug comes from Place. Until 2026-09-24 this passed "" and every
// cancel failed with a 400 ("unable to process"), so each maker rest the bot
// "cancelled" stayed on the book until it filled — one filled 6 minutes after
// the bot had abandoned it, leaving an unhedged short.
func (p *PolyUSExecutor) Cancel(ctx context.Context, venueOrderID string) error {
	p.mu.Lock()
	slug := p.markets[venueOrderID]
	p.mu.Unlock()
	if slug == "" {
		return fmt.Errorf("polymarketus: cancel %s: market unknown (not placed by this executor) — use CancelInMarket", venueOrderID)
	}
	if err := p.CancelInMarket(ctx, venueOrderID, slug); err != nil {
		return err
	}
	p.mu.Lock()
	delete(p.markets, venueOrderID)
	p.mu.Unlock()
	return nil
}

// CancelInMarket cancels an order, supplying the market slug the API requires.
func (p *PolyUSExecutor) CancelInMarket(ctx context.Context, venueOrderID, marketSlug string) error {
	if marketSlug == "" {
		return fmt.Errorf("polymarketus: cancel %s: marketSlug is required", venueOrderID)
	}
	path := "/v1/order/" + venueOrderID + "/cancel"
	hdr, err := p.sign(http.MethodPost, path)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]string{"marketSlug": marketSlug})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, p.base+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	applyHeaders(req, hdr)
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("polymarketus: cancel %d: %s", resp.StatusCode, b)
	}
	return nil
}

// CancelAllOpen cancels every open order, or only those in slugs if given.
// Docs: POST /v1/orders/open/cancel {"slugs": [...]}; the response echoes the
// ids it cancelled. Callers must re-list open orders to confirm.
func (p *PolyUSExecutor) CancelAllOpen(ctx context.Context, slugs []string) ([]string, error) {
	if slugs == nil {
		slugs = []string{}
	}
	raw, err := p.post(ctx, "/v1/orders/open/cancel", map[string]any{"slugs": slugs})
	if err != nil {
		return nil, err
	}
	var out struct {
		CanceledOrderIDs []string `json:"canceledOrderIds"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("polymarketus: parse cancel-all response: %w", err)
	}
	return out.CanceledOrderIDs, nil
}

func (p *PolyUSExecutor) post(ctx context.Context, path string, payload any) (string, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	hdr, err := p.sign(http.MethodPost, path)
	if err != nil {
		return "", fmt.Errorf("polymarketus: sign: %w", err)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, p.base+path, bytes.NewReader(body))
	applyHeaders(req, hdr)
	resp, err := p.client.Do(req)
	if err != nil {
		// Network/timeout: the order MAY have landed. Do not resubmit — reconcile.
		return "", fmt.Errorf("polymarketus: %s (reconcile before retry): %w", path, err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 500 {
		return "", fmt.Errorf("polymarketus: %s 5xx (reconcile before retry): %s", path, rb)
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("polymarketus: %s rejected %d: %s", path, resp.StatusCode, rb)
	}
	return string(rb), nil
}

func applyHeaders(req *http.Request, hdr http.Header) {
	for k, v := range hdr {
		req.Header[k] = v
	}
	req.Header.Set("Content-Type", "application/json")
	// The WAF rejects non-browser agents with 403 (error 1010) even when the
	// Ed25519 signature is valid.
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Accept", "application/json")
}
