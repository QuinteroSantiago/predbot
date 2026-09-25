package exec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"predbot/internal/core"
)

// KalshiExecutor places orders via Kalshi's Create Order (V2) endpoint.
//
// Wire format verified against docs.kalshi.com (2026-08-23):
//   - path: POST /portfolio/events/orders  (NOT the legacy /portfolio/orders,
//     which is deprecated no earlier than 2026-05-06).
//   - side: "bid" = BUY YES, "ask" = SELL YES — the V2 single-book model quotes
//     everything from the YES side (selling YES == buying NO at 1-price).
//   - price: fixed-point dollar STRING ("0.5600"), 2-4 decimals.
//   - count: fixed-point contract STRING ("10.00"), min granularity 0.01.
//   - time_in_force: fill_or_kill | good_till_canceled | immediate_or_cancel.
//   - self_trade_prevention_type is REQUIRED: taker_at_cross | maker.
//   - client_order_id is the idempotency key (design §5, §11).
//   - order_group_id ties orders to a group whose rolling 15s contract limit acts
//     as an exchange-side kill backstop (design §8).
//
// Auth is the same RSA-PSS signing as the market-data WS: sign(method, path).
type KalshiExecutor struct {
	base    string
	sign    func(method, path string) (http.Header, error)
	client  *http.Client
	groupID string // optional order group for the exchange-side backstop
}

// NewKalshiExecutor builds an executor. base is e.g.
// https://api.elections.kalshi.com/trade-api/v2. groupID may be "".
func NewKalshiExecutor(base string, sign func(method, path string) (http.Header, error), groupID string) *KalshiExecutor {
	return &KalshiExecutor{base: base, sign: sign, groupID: groupID,
		client: &http.Client{Timeout: orderHTTPTimeout}}
}

func (k *KalshiExecutor) Name() string { return core.VenueKalshi }

type kalshiCreateOrderV2 struct {
	Ticker        string `json:"ticker"`
	ClientOrderID string `json:"client_order_id,omitempty"`
	Side          string `json:"side"`  // "bid" = buy YES, "ask" = sell YES
	Count         string `json:"count"` // fixed-point contracts, e.g. "10.00"
	Price         string `json:"price"` // fixed-point dollars, e.g. "0.5600"
	TimeInForce   string `json:"time_in_force"`
	SelfTradePrev string `json:"self_trade_prevention_type"` // required
	PostOnly      bool   `json:"post_only,omitempty"`
	OrderGroupID  string `json:"order_group_id,omitempty"`
}

func kalshiSide(s core.Side) string {
	if s == core.SellYes {
		return "ask" // sell YES
	}
	return "bid" // buy YES
}

func kalshiTIF(t core.TIF) string {
	switch t {
	case core.GTC, core.GTD:
		return "good_till_canceled"
	case core.FOK:
		return "fill_or_kill"
	default:
		return "immediate_or_cancel"
	}
}

// Body builds the Create-Order-V2 payload (exported shape used by tests).
func (k *KalshiExecutor) body(o core.Order) kalshiCreateOrderV2 {
	return kalshiCreateOrderV2{
		Ticker:        o.VenueID,
		ClientOrderID: o.ClientID,
		Side:          kalshiSide(o.Side),
		Count:         o.Qty.String(), // Qty.String() emits 2 decimals ("10.00")
		Price:         o.Px.String(),  // Price.String() emits 4 decimals ("0.5600")
		TimeInForce:   kalshiTIF(o.TIF),
		SelfTradePrev: "taker_at_cross",
		OrderGroupID:  k.groupID,
	}
}

// Place submits an order. On timeout/5xx it returns an error and does NOT
// resubmit — the caller must reconcile by client_order_id first (design §11).
func (k *KalshiExecutor) Place(ctx context.Context, o core.Order) (string, error) {
	const path = "/portfolio/events/orders"
	body, err := json.Marshal(k.body(o))
	if err != nil {
		return "", err
	}
	hdr, err := k.sign(http.MethodPost, path)
	if err != nil {
		return "", fmt.Errorf("kalshi: sign: %w", err)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, k.base+path, bytes.NewReader(body))
	for key, vals := range hdr {
		req.Header[key] = vals
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := k.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("kalshi: place (reconcile by client id %q before retry): %w", o.ClientID, err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 500 {
		return "", fmt.Errorf("kalshi: 5xx (reconcile by client id %q before retry): %s", o.ClientID, rb)
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("kalshi: rejected %d: %s", resp.StatusCode, rb)
	}
	var out struct {
		Order struct {
			OrderID string `json:"order_id"`
		} `json:"order"`
		OrderID string `json:"order_id"`
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		return "", fmt.Errorf("kalshi: parse response: %w", err)
	}
	if out.Order.OrderID != "" {
		return out.Order.OrderID, nil
	}
	return out.OrderID, nil
}

// Cancel cancels a resting order (Cancel Order V2).
func (k *KalshiExecutor) Cancel(ctx context.Context, venueOrderID string) error {
	path := "/portfolio/events/orders/" + venueOrderID
	hdr, err := k.sign(http.MethodDelete, path)
	if err != nil {
		return err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, k.base+path, nil)
	for key, vals := range hdr {
		req.Header[key] = vals
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("kalshi: cancel %d: %s", resp.StatusCode, b)
	}
	return nil
}

// CreateOrderGroup creates an exchange-side backstop (design §8): a group with a
// rolling 15-second contract limit. If the bot goes insane, Kalshi cancels every
// order in the group and blocks new ones until reset — protection that works
// even when our own process is the problem.
//
// Verified against docs.kalshi.com/api-reference/order-groups/create-order-group
// (2026-08-24): POST /portfolio/order_groups/create {contracts_limit:int>=1}.
func (k *KalshiExecutor) CreateOrderGroup(ctx context.Context, contractsLimit int64) (string, error) {
	const path = "/portfolio/order_groups/create"
	body, _ := json.Marshal(map[string]any{"contracts_limit": contractsLimit})
	hdr, err := k.sign(http.MethodPost, path)
	if err != nil {
		return "", err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, k.base+path, bytes.NewReader(body))
	for key, vals := range hdr {
		req.Header[key] = vals
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := k.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("kalshi: create order group %d: %s", resp.StatusCode, rb)
	}
	var out struct {
		OrderGroupID string `json:"order_group_id"`
		ID           string `json:"id"`
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		return "", err
	}
	if out.OrderGroupID != "" {
		return out.OrderGroupID, nil
	}
	return out.ID, nil
}

// SetOrderGroup attaches subsequent orders to an order group.
func (k *KalshiExecutor) SetOrderGroup(id string) { k.groupID = id }
