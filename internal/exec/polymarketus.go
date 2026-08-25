package exec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
}

func NewPolyUSExecutor(base string, sign func(method, path string) (http.Header, error)) *PolyUSExecutor {
	return &PolyUSExecutor{base: base, sign: sign, client: &http.Client{Timeout: 5 * time.Second}}
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
	return out.ID, nil
}

// Cancel cancels one order by its exchange-assigned id.
//
// Docs (verified 2026-08-25): POST /v1/order/{orderId}/cancel with a
// {"marketSlug":...} body. NOT DELETE /v1/orders/{id} — that was an unverified
// guess and returned 404 against the live venue, so cancels silently never
// worked. Note the path segment is SINGULAR ("order"), unlike the plural
// "/v1/orders" used to place.
func (p *PolyUSExecutor) Cancel(ctx context.Context, venueOrderID string) error {
	return p.CancelInMarket(ctx, venueOrderID, "")
}

// CancelInMarket cancels an order, supplying the market slug the API requires.
func (p *PolyUSExecutor) CancelInMarket(ctx context.Context, venueOrderID, marketSlug string) error {
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
