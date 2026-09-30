package yahoo

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"etfbot/internal/market"
)

// 2024-01-02 and 2024-01-03 at 09:30 New York; the null day is skipped.
const fixture = `{"chart":{"result":[{"timestamp":[1704205800,1704292200,1704378600],
 "indicators":{"quote":[{"close":[472.65,468.79,null]}],
 "adjclose":[{"adjclose":[460.1,456.3,null]}]}}],"error":null}}`

func TestDailyParsesAndCaches(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		io.WriteString(w, fixture)
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, CacheDir: t.TempDir(), MaxAge: time.Hour}
	for range 2 {
		got, err := c.Daily(context.Background(), []string{"SPY"}, market.Date(2024, 1, 1), market.Date(2024, 12, 31))
		if err != nil {
			t.Fatal(err)
		}
		b := got["SPY"]
		if len(b) != 2 || !b[0].Date.Equal(market.Date(2024, 1, 2)) || b[1].Close != 456.3 {
			t.Fatalf("bars = %+v; want Jan 2 and 3 adjusted closes", b)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("fetched %d times; want 1 (second read from cache)", hits.Load())
	}
}

func TestDailyReportsYahooError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"chart":{"result":null,"error":{"code":"Not Found","description":"No data found, symbol may be delisted"}}}`)
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL}
	if _, err := c.Daily(context.Background(), []string{"NOPE"}, time.Time{}, time.Now()); err == nil {
		t.Fatal("want an error for an unknown symbol")
	}
}
