// Package alpaca implements market.Broker and market.Bars against Alpaca's
// trading and market-data REST APIs (v2), plus the clock and calendar the live
// bot schedules by. Paper and live accounts differ only in BaseURL.
package alpaca

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"etfbot/internal/market"
)

const (
	PaperURL = "https://paper-api.alpaca.markets"
	LiveURL  = "https://api.alpaca.markets"
	DataURL  = "https://data.alpaca.markets"
)

// Client talks to one Alpaca account.
type Client struct {
	BaseURL, DataURL string
	KeyID, Secret    string
	Feed             string // market-data feed: "iex" (free) or "sip"
	HTTP             *http.Client
}

// Paper reports whether the client points at a paper account.
func (c *Client) Paper() bool { return c.BaseURL == PaperURL }

func (c *Client) do(ctx context.Context, method, base, path string, q url.Values, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	u := base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	req.Header.Set("APCA-API-KEY-ID", c.KeyID)
	req.Header.Set("APCA-API-SECRET-KEY", c.Secret)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("alpaca %s %s: HTTP %d: %.300s", method, path, resp.StatusCode, b)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}

// num is Alpaca's decimal-as-string.
type num string

func (n num) f() float64 {
	v, _ := strconv.ParseFloat(string(n), 64)
	return v
}

// Account returns cash and equity. It refuses an account that can't trade.
func (c *Client) Account(ctx context.Context) (market.Account, error) {
	var a struct {
		Status         string `json:"status"`
		TradingBlocked bool   `json:"trading_blocked"`
		Cash           num    `json:"cash"`
		Equity         num    `json:"equity"`
	}
	if err := c.do(ctx, "GET", c.BaseURL, "/v2/account", nil, nil, &a); err != nil {
		return market.Account{}, err
	}
	if a.Status != "ACTIVE" || a.TradingBlocked {
		return market.Account{}, fmt.Errorf("alpaca account not tradable (status %s, trading_blocked %v)", a.Status, a.TradingBlocked)
	}
	return market.Account{Cash: a.Cash.f(), Equity: a.Equity.f()}, nil
}

func (c *Client) Positions(ctx context.Context) ([]market.Position, error) {
	var ps []struct {
		Symbol string `json:"symbol"`
		Qty    num    `json:"qty"`
	}
	if err := c.do(ctx, "GET", c.BaseURL, "/v2/positions", nil, nil, &ps); err != nil {
		return nil, err
	}
	out := make([]market.Position, len(ps))
	for i, p := range ps {
		out[i] = market.Position{Symbol: p.Symbol, Qty: p.Qty.f()}
	}
	return out, nil
}

// Place submits a day market order: buys by dollar amount, sells by shares.
func (c *Client) Place(ctx context.Context, o market.Order) (string, error) {
	body := map[string]string{"symbol": o.Symbol, "side": o.Side.String(), "type": "market", "time_in_force": "day"}
	if o.Side == market.Buy {
		body["notional"] = strconv.FormatFloat(o.Notional, 'f', 2, 64)
	} else {
		body["qty"] = strconv.FormatFloat(o.Qty, 'f', 9, 64)
	}
	var r struct {
		ID string `json:"id"`
	}
	if err := c.do(ctx, "POST", c.BaseURL, "/v2/orders", nil, body, &r); err != nil {
		return "", err
	}
	return r.ID, nil
}

func (c *Client) Order(ctx context.Context, id string) (market.OrderState, error) {
	var r struct {
		ID             string `json:"id"`
		Status         string `json:"status"`
		FilledQty      num    `json:"filled_qty"`
		FilledAvgPrice *num   `json:"filled_avg_price"`
	}
	if err := c.do(ctx, "GET", c.BaseURL, "/v2/orders/"+url.PathEscape(id), nil, nil, &r); err != nil {
		return market.OrderState{}, err
	}
	st := market.OrderState{ID: r.ID, Status: r.Status, FilledQty: r.FilledQty.f()}
	if r.FilledAvgPrice != nil {
		st.AvgPrice = r.FilledAvgPrice.f()
	}
	switch r.Status {
	case "filled", "canceled", "expired", "rejected", "done_for_day":
		st.Done = true
	}
	return st, nil
}

// Clock reports whether the market is open now, and the exchange's time.
func (c *Client) Clock(ctx context.Context) (open bool, now time.Time, err error) {
	var r struct {
		IsOpen    bool      `json:"is_open"`
		Timestamp time.Time `json:"timestamp"`
	}
	err = c.do(ctx, "GET", c.BaseURL, "/v2/clock", nil, nil, &r)
	return r.IsOpen, r.Timestamp, err
}

// TradingDays lists the exchange's trading dates in [from, to].
func (c *Client) TradingDays(ctx context.Context, from, to time.Time) ([]time.Time, error) {
	var r []struct {
		Date string `json:"date"`
	}
	q := url.Values{"start": {from.Format(time.DateOnly)}, "end": {to.Format(time.DateOnly)}}
	if err := c.do(ctx, "GET", c.BaseURL, "/v2/calendar", q, nil, &r); err != nil {
		return nil, err
	}
	out := make([]time.Time, 0, len(r))
	for _, d := range r {
		t, err := time.Parse(time.DateOnly, d.Date)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// Daily returns split- and dividend-adjusted daily closes in [from, to].
func (c *Client) Daily(ctx context.Context, symbols []string, from, to time.Time) (map[string][]market.Bar, error) {
	out := map[string][]market.Bar{}
	q := url.Values{
		"symbols": {strings.Join(symbols, ",")}, "timeframe": {"1Day"}, "adjustment": {"all"},
		"start": {from.Format(time.DateOnly)}, "end": {to.Format(time.DateOnly)},
		"feed": {c.feed()}, "limit": {"10000"}, "sort": {"asc"},
	}
	for {
		var r struct {
			Bars map[string][]struct {
				T time.Time `json:"t"`
				C float64   `json:"c"`
			} `json:"bars"`
			Next *string `json:"next_page_token"`
		}
		if err := c.do(ctx, "GET", c.DataURL, "/v2/stocks/bars", q, nil, &r); err != nil {
			return nil, err
		}
		for sym, bs := range r.Bars {
			for _, b := range bs {
				out[sym] = append(out[sym], market.Bar{Date: market.DayOf(b.T), Close: b.C})
			}
		}
		if r.Next == nil || *r.Next == "" {
			return out, nil
		}
		q.Set("page_token", *r.Next)
	}
}

// Quote is a symbol's latest trade.
type Quote struct {
	Price float64
	At    time.Time
}

// Latest returns each symbol's most recent trade.
func (c *Client) Latest(ctx context.Context, symbols []string) (map[string]Quote, error) {
	var r struct {
		Trades map[string]struct {
			P float64   `json:"p"`
			T time.Time `json:"t"`
		} `json:"trades"`
	}
	q := url.Values{"symbols": {strings.Join(symbols, ",")}, "feed": {c.feed()}}
	if err := c.do(ctx, "GET", c.DataURL, "/v2/stocks/trades/latest", q, nil, &r); err != nil {
		return nil, err
	}
	out := map[string]Quote{}
	for sym, t := range r.Trades {
		out[sym] = Quote{Price: t.P, At: t.T}
	}
	return out, nil
}

func (c *Client) feed() string {
	if c.Feed == "" {
		return "iex"
	}
	return c.Feed
}
