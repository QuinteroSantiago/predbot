package alpaca

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"etfbot/internal/market"
)

func TestPlaceSendsNotionalBuysAndQtySells(t *testing.T) {
	var got []map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("APCA-API-KEY-ID") != "k" || r.Header.Get("APCA-API-SECRET-KEY") != "s" {
			t.Error("missing auth headers")
		}
		var b map[string]string
		json.NewDecoder(r.Body).Decode(&b)
		got = append(got, b)
		io.WriteString(w, `{"id":"o1"}`)
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, KeyID: "k", Secret: "s"}
	ctx := context.Background()
	if id, err := c.Place(ctx, market.Order{Symbol: "SPY", Side: market.Buy, Notional: 1234.5}); err != nil || id != "o1" {
		t.Fatalf("buy: %v %v", id, err)
	}
	c.Place(ctx, market.Order{Symbol: "TLT", Side: market.Sell, Qty: 3.25})
	if got[0]["notional"] != "1234.50" || got[0]["side"] != "buy" || got[0]["qty"] != "" {
		t.Errorf("buy body = %v", got[0])
	}
	if got[1]["qty"] != "3.250000000" || got[1]["side"] != "sell" || got[1]["notional"] != "" {
		t.Errorf("sell body = %v", got[1])
	}
}

func TestAccountRefusesBlocked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"status":"ACTIVE","trading_blocked":true,"cash":"100","equity":"100"}`)
	}))
	defer srv.Close()
	if _, err := (&Client{BaseURL: srv.URL}).Account(context.Background()); err == nil {
		t.Fatal("a trading-blocked account must be refused")
	}
}

func TestOrderStateAndBarsPaging(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/orders/o1":
			io.WriteString(w, `{"id":"o1","status":"filled","filled_qty":"2.5","filled_avg_price":"401.2"}`)
		case r.URL.Query().Get("page_token") == "":
			io.WriteString(w, `{"bars":{"SPY":[{"t":"2024-01-02T05:00:00Z","c":470}]},"next_page_token":"p2"}`)
		default:
			io.WriteString(w, `{"bars":{"SPY":[{"t":"2024-01-03T05:00:00Z","c":468}]},"next_page_token":null}`)
		}
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, DataURL: srv.URL}
	st, err := c.Order(context.Background(), "o1")
	if err != nil || !st.Done || st.FilledQty != 2.5 || st.AvgPrice != 401.2 {
		t.Fatalf("order = %+v, %v", st, err)
	}
	bars, err := c.Daily(context.Background(), []string{"SPY"}, market.Date(2024, 1, 1), market.Date(2024, 1, 5))
	if err != nil || len(bars["SPY"]) != 2 || !bars["SPY"][1].Date.Equal(market.Date(2024, 1, 3)) {
		t.Fatalf("bars = %+v, %v", bars, err)
	}
}
