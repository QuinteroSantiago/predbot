package exec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"

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
//   - Kalshi is sharded (docs: getting_started/exchange_sharding): since
//     2026-08-24 new baseball events live on exchange_index 3 (basketball since
//     2026-09-10), most other sports on 0. Orders auto-route by ticker, but an
//     order group lives on ONE shard and "order groups do not function across
//     exchange instances": a shard-0 group on an MLB order is rejected 404
//     order_group_not_found (every MLB lead from 2026-08-31 to 2026-09-26).
//     So there is one group per shard, picked by the ticker's shard.
//
// Auth is the same RSA-PSS signing as the market-data WS: sign(method, path).
type KalshiExecutor struct {
	base    string
	sign    func(method, path string) (http.Header, error)
	client  *http.Client
	groupID string // single order group, used when no per-shard groups are set

	mu     sync.RWMutex
	groups map[int]string // exchange shard -> order group id (the backstop)
	shards map[string]int // market ticker -> exchange shard; absent = shard 0
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
	}
}

// groupFor returns the order group for ticker's exchange shard. With per-shard
// groups set, a shard that has no group is an error: placing the order would
// either be rejected by Kalshi or, worse, go through without the backstop.
func (k *KalshiExecutor) groupFor(ticker string) (string, error) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	if len(k.groups) == 0 {
		return k.groupID, nil
	}
	shard := k.shards[ticker]
	id, ok := k.groups[shard]
	if !ok {
		return "", fmt.Errorf("kalshi: no order group on exchange shard %d for %s — refusing to place without the backstop", shard, ticker)
	}
	return id, nil
}

// Place submits an order. On timeout/5xx it returns an error and does NOT
// resubmit — the caller must reconcile by client_order_id first (design §11).
func (k *KalshiExecutor) Place(ctx context.Context, o core.Order) (string, error) {
	const path = "/portfolio/events/orders"
	b := k.body(o)
	gid, err := k.groupFor(o.VenueID)
	if err != nil {
		return "", err
	}
	b.OrderGroupID = gid
	body, err := json.Marshal(b)
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
// (2026-08-24, exchange_index 2026-09-28): POST /portfolio/order_groups/create
// {contracts_limit:int>=1, exchange_index:int (default 0)}. A group only covers
// orders on its own shard.
func (k *KalshiExecutor) CreateOrderGroup(ctx context.Context, contractsLimit int64, shard int) (string, error) {
	const path = "/portfolio/order_groups/create"
	body, _ := json.Marshal(map[string]any{"contracts_limit": contractsLimit, "exchange_index": shard})
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

// SetOrderGroups attaches subsequent orders to the group for their shard.
func (k *KalshiExecutor) SetOrderGroups(groups map[int]string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.groups = groups
}

// SetShards records which exchange shard each ticker trades on (from market
// discovery). Merged, so a refresh that drops a ticker keeps its last shard.
func (k *KalshiExecutor) SetShards(shards map[string]int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.shards == nil {
		k.shards = make(map[string]int, len(shards))
	}
	for t, s := range shards {
		k.shards[t] = s
	}
}
