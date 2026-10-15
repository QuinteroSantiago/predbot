// Package yahoo downloads daily history from Yahoo Finance's chart endpoint,
// for backtests. It is unofficial and unauthenticated, so the live bot uses
// the broker's data instead. Adjusted closes (splits and dividends) are used,
// so returns are total returns.
package yahoo

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"etfbot/internal/market"
)

// Client fetches history, caching each symbol's full series as CSV in CacheDir
// for MaxAge so repeated backtests don't refetch.
type Client struct {
	HTTP     *http.Client
	Base     string // default https://query1.finance.yahoo.com
	CacheDir string // "" disables the cache
	MaxAge   time.Duration
}

// Daily returns each symbol's bars within [from, to].
func (c *Client) Daily(ctx context.Context, symbols []string, from, to time.Time) (map[string][]market.Bar, error) {
	out := map[string][]market.Bar{}
	for _, sym := range symbols {
		all, err := c.series(ctx, sym)
		if err != nil {
			return nil, fmt.Errorf("yahoo %s: %w", sym, err)
		}
		var in []market.Bar
		for _, b := range all {
			if !b.Date.Before(from) && !b.Date.After(to) {
				in = append(in, b)
			}
		}
		out[sym] = in
	}
	return out, nil
}

func (c *Client) series(ctx context.Context, sym string) ([]market.Bar, error) {
	path := filepath.Join(c.CacheDir, sym+".csv")
	if c.CacheDir != "" {
		if fi, err := os.Stat(path); err == nil && time.Since(fi.ModTime()) < c.MaxAge {
			return readCSV(path)
		}
	}
	bars, err := c.fetch(ctx, sym)
	if err != nil {
		return nil, err
	}
	if c.CacheDir != "" {
		if err := os.MkdirAll(c.CacheDir, 0o750); err != nil {
			return nil, err
		}
		if err := writeCSV(path, bars); err != nil {
			return nil, err
		}
	}
	return bars, nil
}

func (c *Client) fetch(ctx context.Context, sym string) ([]market.Bar, error) {
	base := c.Base
	if base == "" {
		base = "https://query1.finance.yahoo.com"
	}
	q := url.Values{"period1": {"0"}, "period2": {strconv.FormatInt(time.Now().Unix(), 10)},
		"interval": {"1d"}, "events": {"div,split"}, "includeAdjustedClose": {"true"}}
	req, err := http.NewRequestWithContext(ctx, "GET", base+"/v8/finance/chart/"+url.PathEscape(sym)+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0") // the endpoint refuses Go's default agent
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %.200s", resp.StatusCode, body)
	}
	return parse(body)
}

func parse(body []byte) ([]market.Bar, error) {
	var r struct {
		Chart struct {
			Result []struct {
				Timestamp  []int64 `json:"timestamp"`
				Indicators struct {
					AdjClose []struct {
						AdjClose []*float64 `json:"adjclose"`
					} `json:"adjclose"`
				} `json:"indicators"`
			} `json:"result"`
			Error *struct {
				Description string `json:"description"`
			} `json:"error"`
		} `json:"chart"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	if r.Chart.Error != nil {
		return nil, fmt.Errorf("%s", r.Chart.Error.Description)
	}
	if len(r.Chart.Result) != 1 || len(r.Chart.Result[0].Indicators.AdjClose) != 1 {
		return nil, fmt.Errorf("unexpected response shape")
	}
	res := r.Chart.Result[0]
	adj := res.Indicators.AdjClose[0].AdjClose
	if len(adj) != len(res.Timestamp) {
		return nil, fmt.Errorf("%d timestamps but %d closes", len(res.Timestamp), len(adj))
	}
	var out []market.Bar
	for i, ts := range res.Timestamp {
		if adj[i] == nil || *adj[i] <= 0 {
			continue // a day Yahoo has no price for
		}
		d := market.DayOf(time.Unix(ts, 0))
		if n := len(out); n > 0 && !d.After(out[n-1].Date) {
			continue // duplicate or out-of-order day
		}
		out = append(out, market.Bar{Date: d, Close: *adj[i]})
	}
	return out, nil
}

func writeCSV(path string, bars []market.Bar) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	for _, b := range bars {
		w.Write([]string{b.Date.Format(time.DateOnly), strconv.FormatFloat(b.Close, 'g', -1, 64)})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readCSV(path string) ([]market.Bar, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		return nil, err
	}
	out := make([]market.Bar, 0, len(rows))
	for _, r := range rows {
		d, err := time.Parse(time.DateOnly, r[0])
		if err != nil {
			return nil, err
		}
		px, err := strconv.ParseFloat(r[1], 64)
		if err != nil {
			return nil, err
		}
		out = append(out, market.Bar{Date: d, Close: px})
	}
	return out, nil
}
