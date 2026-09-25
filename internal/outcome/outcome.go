// Package outcome records how markets actually RESOLVED.
//
// Why this exists: an arbitrage backtest never needs to know who won — its P&L
// comes from the spread between venues. Any DIRECTIONAL strategy (favorite-
// longshot bias, momentum, mean reversion, model-vs-market) computes its entire
// P&L from settlement, so without resolution data such a strategy cannot be
// backtested at all. Order books alone are not enough.
//
// The recorder polls markets whose resolution date has passed, captures the
// settled result, and appends it to an append-only JSONL file. Results are
// immutable once written.
package outcome

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"predbot/internal/core"
	"strings"
	"sync"
	"time"
)

// Result is one market's final settlement.
type Result struct {
	Canonical  string `json:"canonical"`
	Venue      string `json:"venue"`
	VenueID    string `json:"venue_id"`
	Outcome    string `json:"outcome"`     // "yes" | "no"
	Status     string `json:"status"`      // venue's status string
	RecordedAt string `json:"recorded_at"` // RFC3339, when WE observed it
	CloseTime  string `json:"close_time,omitempty"`
}

// Store is an append-only JSONL sink that never records the same market twice.
type Store struct {
	path string
	mu   sync.Mutex
	seen map[string]bool
}

func NewStore(path string) (*Store, error) {
	s := &Store{path: path, seen: map[string]bool{}}
	// Load what we already have so restarts don't duplicate.
	f, err := os.Open(path)
	if err == nil {
		defer f.Close()
		dec := json.NewDecoder(f)
		for {
			var r Result
			if err := dec.Decode(&r); err != nil {
				break
			}
			s.seen[r.Venue+"|"+r.VenueID] = true
		}
	}
	return s, nil
}

// Has reports whether a market's outcome is already recorded.
func (s *Store) Has(venue, venueID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seen[venue+"|"+venueID]
}

// Put appends a result, ignoring duplicates.
func (s *Store) Put(r Result) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := r.Venue + "|" + r.VenueID
	if s.seen[k] {
		return nil
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	s.seen[k] = true
	return nil
}

// Count returns how many outcomes are recorded.
func (s *Store) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.seen)
}

// outcomeHTTPTimeout bounds one settled-outcome lookup.
const outcomeHTTPTimeout = 15 * time.Second

var httpClient = &http.Client{Timeout: outcomeHTTPTimeout}

func get(ctx context.Context, url string) ([]byte, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0") // Poly's WAF rejects default agents
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET %s: %d", url, resp.StatusCode)
	}
	return b, nil
}

// FetchKalshi returns the settled outcome for a Kalshi ticker, or ok=false if it
// has not settled yet. Verified 2026-08-24: settled markets carry
// status "finalized"/"settled" and result "yes"|"no".
func FetchKalshi(ctx context.Context, base, ticker string) (Result, bool, error) {
	b, err := get(ctx, base+"/markets/"+ticker)
	if err != nil {
		return Result{}, false, err
	}
	var out struct {
		Market struct {
			Ticker    string `json:"ticker"`
			Status    string `json:"status"`
			Result    string `json:"result"`
			CloseTime string `json:"close_time"`
		} `json:"market"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return Result{}, false, err
	}
	m := out.Market
	if m.Result != "yes" && m.Result != "no" {
		return Result{}, false, nil // not settled yet
	}
	return Result{
		Venue: core.VenueKalshi, VenueID: m.Ticker, Outcome: m.Result, Status: m.Status,
		CloseTime: m.CloseTime, RecordedAt: time.Now().UTC().Format(time.RFC3339),
	}, true, nil
}

// FetchPolyUS returns the settled outcome for a Polymarket US slug.
// [VERIFY] the settled-state field names against a real resolved market — this
// accepts a resolved/settled state plus a winning-outcome indicator.
func FetchPolyUS(ctx context.Context, base, slug string) (Result, bool, error) {
	b, err := get(ctx, base+"/v1/markets?slug="+slug)
	if err != nil {
		return Result{}, false, err
	}
	var out struct {
		Markets []struct {
			Slug    string `json:"slug"`
			Closed  bool   `json:"closed"`
			Active  bool   `json:"active"`
			Outcome string `json:"resolvedOutcome"`
			State   string `json:"state"`
		} `json:"markets"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return Result{}, false, err
	}
	for _, m := range out.Markets {
		if m.Slug != slug || !m.Closed {
			continue
		}
		o := strings.ToLower(m.Outcome)
		if o != "yes" && o != "no" {
			return Result{}, false, nil // closed but outcome not exposed here
		}
		return Result{
			Venue: core.VenuePoly, VenueID: m.Slug, Outcome: o, Status: m.State,
			RecordedAt: time.Now().UTC().Format(time.RFC3339),
		}, true, nil
	}
	return Result{}, false, nil
}
