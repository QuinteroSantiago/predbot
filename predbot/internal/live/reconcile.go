package live

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"predbot/internal/core"
)

// AccountClient reads authoritative account state from a venue. Positions are
// the ground truth: the trader's internal tally is optimistic (it books an
// accepted IOC as filled), so we reconcile against the venue on startup and on
// a loop, and Kill() on a mismatch we cannot explain (design §8).
type AccountClient interface {
	Venue() string
	// Positions returns venueID -> net contracts (positive = long YES).
	Positions(ctx context.Context) (map[string]float64, error)
	// OpenOrders returns the venue's resting order ids.
	OpenOrders(ctx context.Context) ([]string, error)
	// BalanceUSD returns available balance in dollars.
	BalanceUSD(ctx context.Context) (float64, error)
}

// httpAccount is a small signed-GET helper shared by both venues.
type httpAccount struct {
	venue  string
	base   string
	sign   func(method, path string) (http.Header, error)
	client *http.Client
	ua     bool // send a browser UA (Polymarket's WAF requires it)
}

func (h *httpAccount) Venue() string { return h.venue }

func (h *httpAccount) get(ctx context.Context, path string) ([]byte, error) {
	// Both venues sign the path WITHOUT the query string; including it yields
	// INCORRECT_API_KEY_SIGNATURE (verified against live APIs 2026-08-24).
	signPath := path
	if i := strings.IndexByte(signPath, '?'); i >= 0 {
		signPath = signPath[:i]
	}
	hdr, err := h.sign(http.MethodGet, signPath)
	if err != nil {
		return nil, err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, h.base+path, nil)
	for k, v := range hdr {
		req.Header[k] = v
	}
	req.Header.Set("Accept", "application/json")
	if h.ua {
		req.Header.Set("User-Agent", "Mozilla/5.0")
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, &HTTPError{Venue: h.venue, Path: path, Status: resp.StatusCode, Body: string(b)}
	}
	return b, nil
}

// HTTPError is a venue's non-2xx response. It is typed so the outage breaker can
// tell a venue that is DOWN (5xx — e.g. Polymarket maintenance answers 503 with
// gRPC code 14 UNAVAILABLE) from a request that is wrong (4xx).
type HTTPError struct {
	Venue, Path string
	Status      int
	Body        string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("%s GET %s: %d: %s", e.Venue, e.Path, e.Status, e.Body)
}

// --- Kalshi ---

// KalshiAccount reads Kalshi portfolio state.
type KalshiAccount struct{ httpAccount }

func NewKalshiAccount(base string, sign func(string, string) (http.Header, error)) *KalshiAccount {
	return &KalshiAccount{httpAccount{venue: core.VenueKalshi, base: base, sign: sign,
		client: &http.Client{Timeout: accountHTTPTimeout}}}
}

func (k *KalshiAccount) Positions(ctx context.Context) (map[string]float64, error) {
	b, err := k.get(ctx, "/portfolio/positions")
	if err != nil {
		return nil, err
	}
	// position_fp ("5.00") is what Kalshi actually returns; `position` is the
	// older integer form. Both are decoded as POINTERS so an absent key is
	// distinguishable from a genuine zero. This parser previously read only
	// `position`, which does not exist in the live response: every row decoded
	// as 0, was filtered out, and a real 5-contract book was reported as FLAT to
	// the drift monitor and the silent-fill verifier for the whole life of the
	// bot. Same class of bug as the Polymarket `quantity`/`netPosition` miss.
	var out struct {
		Cursor          string `json:"cursor"`
		MarketPositions []struct {
			Ticker     string       `json:"ticker"`
			PositionFP *json.Number `json:"position_fp"`
			Position   *json.Number `json:"position"`
		} `json:"market_positions"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	// A short read is indistinguishable from "flat" to every caller.
	if out.Cursor != "" {
		return nil, fmt.Errorf("kalshi: positions response is paginated (cursor=%q); refusing a partial view", out.Cursor)
	}
	pos := map[string]float64{}
	for _, p := range out.MarketPositions {
		n := p.PositionFP
		if n == nil {
			n = p.Position
		}
		if n == nil {
			// Never silently downgrade "I could not read this" to "flat".
			return nil, fmt.Errorf("kalshi: position row %q has neither position_fp nor position; refusing to report it as flat", p.Ticker)
		}
		f, err := n.Float64()
		if err != nil {
			return nil, fmt.Errorf("kalshi: position %q: %w", p.Ticker, err)
		}
		if f != 0 {
			pos[p.Ticker] = f
		}
	}
	return pos, nil
}

func (k *KalshiAccount) OpenOrders(ctx context.Context) ([]string, error) {
	b, err := k.get(ctx, "/portfolio/orders?status=resting")
	if err != nil {
		return nil, err
	}
	var out struct {
		Orders []struct {
			OrderID string `json:"order_id"`
		} `json:"orders"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(out.Orders))
	for _, o := range out.Orders {
		ids = append(ids, o.OrderID)
	}
	return ids, nil
}

func (k *KalshiAccount) BalanceUSD(ctx context.Context) (float64, error) {
	b, err := k.get(ctx, "/portfolio/balance")
	if err != nil {
		return 0, err
	}
	var out struct {
		Balance int64 `json:"balance"` // cents
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return 0, err
	}
	return float64(out.Balance) / 100, nil // Kalshi reports balance in cents (its wire unit, not ours)
}

// BalanceByShard returns available USD per Kalshi exchange shard. Kalshi
// balances are local to a shard ("subaccount balances are local to a specific
// exchange instance"), so a shard with no funds rejects its orders even when
// BalanceUSD's total looks healthy.
func (k *KalshiAccount) BalanceByShard(ctx context.Context) (map[int]float64, error) {
	b, err := k.get(ctx, "/portfolio/balance")
	if err != nil {
		return nil, err
	}
	var out struct {
		Breakdown []struct {
			ExchangeIndex int    `json:"exchange_index"`
			Balance       string `json:"balance"` // fixed-point dollars
		} `json:"balance_breakdown"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	res := make(map[int]float64, len(out.Breakdown))
	for _, e := range out.Breakdown {
		v, err := strconv.ParseFloat(e.Balance, 64)
		if err != nil {
			return nil, fmt.Errorf("kalshi: shard %d balance %q: %w", e.ExchangeIndex, e.Balance, err)
		}
		res[e.ExchangeIndex] = v
	}
	return res, nil
}

// --- Polymarket US ---

// PolyUSAccount reads Polymarket US portfolio state.
type PolyUSAccount struct{ httpAccount }

func NewPolyUSAccount(base string, sign func(string, string) (http.Header, error)) *PolyUSAccount {
	return &PolyUSAccount{httpAccount{venue: core.VenuePoly, base: base, sign: sign,
		client: &http.Client{Timeout: accountHTTPTimeout}, ua: true}}
}

func (p *PolyUSAccount) Positions(ctx context.Context) (map[string]float64, error) {
	b, err := p.get(ctx, "/v1/portfolio/positions")
	if err != nil {
		return nil, err
	}
	// Shape verified live 2026-08-25 against the real endpoint:
	//   {"positions":{"<slug>":{"netPosition":"-5",...}},
	//    "nextCursor":"","eof":true,"availablePositions":[]}
	//
	// The quantity field is netPosition, a STRING, signed in YES terms (-5 =
	// short 5 YES). An earlier version guessed "quantity", which does not exist:
	// the key was silently absent, every position parsed as 0 and was filtered
	// out, and this returned an empty map. That single wrong word disabled BOTH
	// safety layers built on it — the drift monitor saw no drift and the
	// silent-fill verifier saw no position — and on 2026-08-25 the bot sat on
	// two unhedged Poly shorts without noticing. Do not "simplify" this to an
	// unverified field name again.
	//
	// Two quantity fields are returned: netPositionDecimal ("-0.6000") is the
	// FRACTIONAL truth; netPosition ("-1") is the same value ROUNDED to a whole
	// contract. Both are POINTERS so an absent key is distinguishable from a
	// genuine zero (see the Kalshi position_fp/position parser above for the same
	// pattern). Prefer the decimal — see the read loop for why the rounded field
	// is dangerous.
	var out struct {
		Positions map[string]struct {
			NetPositionDecimal *json.Number `json:"netPositionDecimal"`
			NetPosition        *json.Number `json:"netPosition"`
		} `json:"positions"`
		NextCursor string `json:"nextCursor"`
		EOF        bool   `json:"eof"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	// A partial page would look exactly like "no position" to every caller, so
	// refuse it: an error makes the drift monitor log "cannot verify" and the
	// verifier treat the result as unknown, rather than silently reading short.
	if !out.EOF || out.NextCursor != "" {
		return nil, fmt.Errorf("polymarketus: positions response is paginated (eof=%v cursor=%q); refusing a partial view", out.EOF, out.NextCursor)
	}
	pos := map[string]float64{}
	for slug, v := range out.Positions {
		// Polymarket fills in FRACTIONAL shares and the bot books those fractions,
		// so reading the rounded netPosition manufactures phantom drift. On
		// 2026-09-01 a partial 0.4-contract dump left the venue at -0.6: netPosition
		// reported -1, the bot correctly believed -0.6, and the drift monitor killed
		// on the 0.4 rounding gap — halting the bot for two days over a position that
		// actually matched. Prefer netPositionDecimal; fall back to the integer only
		// when the venue omits it.
		n := v.NetPositionDecimal
		if n == nil {
			n = v.NetPosition
		}
		if n == nil {
			// Never silently downgrade "I could not read this" to "flat".
			return nil, fmt.Errorf("polymarketus: position %q has neither netPositionDecimal nor netPosition; refusing to report it as flat", slug)
		}
		f, err := n.Float64()
		if err != nil {
			return nil, fmt.Errorf("polymarketus: position %q: %w", slug, err)
		}
		if f != 0 {
			pos[slug] = f
		}
	}
	return pos, nil
}

func (p *PolyUSAccount) OpenOrders(ctx context.Context) ([]string, error) {
	b, err := p.get(ctx, "/v1/orders/open") // documented path (NOT /v1/orders?status=open)
	if err != nil {
		return nil, err
	}
	var out struct {
		Orders []struct {
			ID string `json:"id"`
		} `json:"orders"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(out.Orders))
	for _, o := range out.Orders {
		ids = append(ids, o.ID)
	}
	return ids, nil
}

// BalanceUSD returns available BUYING POWER (not just cash) — the figure the
// risk engine must budget against. Docs: GET /v1/account/balances (note:
// /v1/portfolio/balance does not exist).
func (p *PolyUSAccount) BalanceUSD(ctx context.Context) (float64, error) {
	b, err := p.get(ctx, "/v1/account/balances")
	if err != nil {
		return 0, err
	}
	// Actual shape (verified live 2026-08-24) — buyingPower is a PLAIN NUMBER
	// nested inside balances[], not a top-level {value,currency} object:
	//   {"balances":[{"currency":"USD","currentBalance":100,"buyingPower":100,
	//                 "openOrders":0,"unsettledFunds":0,"marginRequirement":0,…}]}
	var out struct {
		Balances []struct {
			Currency       string      `json:"currency"`
			BuyingPower    json.Number `json:"buyingPower"`
			CurrentBalance json.Number `json:"currentBalance"`
		} `json:"balances"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return 0, err
	}
	for _, bal := range out.Balances {
		if bal.Currency != "USD" {
			continue
		}
		if f, err := bal.BuyingPower.Float64(); err == nil {
			return f, nil
		}
		f, _ := bal.CurrentBalance.Float64()
		return f, nil
	}
	return 0, fmt.Errorf("polymarketus: no USD balance in response")
}
