package live

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The exact body returned by GET /v1/portfolio/positions on 2026-08-25, trimmed
// to the fields the parser reads. Pinned verbatim because the original parser
// GUESSED the field name ("quantity"), the key silently did not exist, every
// position parsed as zero, and both the drift monitor and the silent-fill
// verifier went blind. A fixture from the live venue is the only thing that
// catches that class of bug.
const polyPositionsLive = `{"positions":{
  "cpc-btc-above-yr-12-31-2026-250k":{"netPosition":"-5","qtyBought":"0","qtySold":"5",
    "avgPx":{"value":"0.9520","currency":"USD"},"netPositionDecimal":"-5.0000"},
  "ewc-usse-me-2026-11-03-dem":{"netPosition":"-5","qtyBought":"0","qtySold":"5",
    "avgPx":{"value":"0.2000","currency":"USD"},"netPositionDecimal":"-5.0000"}
},"nextCursor":"","eof":true,"availablePositions":[]}`

func polyAcctServing(t *testing.T, body string) *PolyUSAccount {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewPolyUSAccount(srv.URL, func(m, p string) (http.Header, error) {
		return http.Header{}, nil
	})
}

func TestPolyUSPositionsParsesLiveShape(t *testing.T) {
	pos, err := polyAcctServing(t, polyPositionsLive).Positions(context.Background())
	if err != nil {
		t.Fatalf("Positions: %v", err)
	}
	if len(pos) != 2 {
		t.Fatalf("want 2 positions, got %d: %v", len(pos), pos)
	}
	// Signed in YES terms: -5 is a short 5 YES, which is what the bot held
	// unhedged. A parser that reads the wrong key returns 0 here and the whole
	// safety net silently disengages.
	for _, slug := range []string{"cpc-btc-above-yr-12-31-2026-250k", "ewc-usse-me-2026-11-03-dem"} {
		if got := pos[slug]; got != -5 {
			t.Errorf("%s: want -5, got %v", slug, got)
		}
	}
}

// The fractional case that killed the bot on 2026-09-01: a partial 0.4-contract
// dump left the venue at -0.6. Polymarket reports netPosition ROUNDED to "-1"
// but the true netPositionDecimal is "-0.6000". The bot books fractional fills,
// so it correctly believed -0.6; reading the rounded field made the drift
// monitor see -1 vs -0.6 and self-kill over a position that actually matched.
// Positions() must report the fractional truth.
func TestPolyUSPositionsPrefersFractionalDecimal(t *testing.T) {
	body := `{"positions":{"aec-mlb-ath-sea-2026-09-03":{"netPosition":"-1",` +
		`"netPositionDecimal":"-0.6000"}},"nextCursor":"","eof":true}`
	pos, err := polyAcctServing(t, body).Positions(context.Background())
	if err != nil {
		t.Fatalf("Positions: %v", err)
	}
	if got := pos["aec-mlb-ath-sea-2026-09-03"]; got != -0.6 {
		t.Errorf("want -0.6 (the fractional truth), got %v (rounded netPosition would be -1)", got)
	}
}

// When the venue omits the decimal field entirely, fall back to the integer
// netPosition rather than dropping the row (which would read as flat).
func TestPolyUSPositionsFallsBackToInteger(t *testing.T) {
	body := `{"positions":{"a":{"netPosition":"-3"}},"nextCursor":"","eof":true}`
	pos, err := polyAcctServing(t, body).Positions(context.Background())
	if err != nil {
		t.Fatalf("Positions: %v", err)
	}
	if got := pos["a"]; got != -3 {
		t.Errorf("want -3 from the fallback, got %v", got)
	}
}

// A row with NEITHER quantity field must ERROR, never report flat — the same
// "don't downgrade unreadable to zero" rule the Kalshi parser enforces.
func TestPolyUSPositionsRefusesRowWithNoQuantity(t *testing.T) {
	body := `{"positions":{"a":{"avgPx":{"value":"0.5"}}},"nextCursor":"","eof":true}`
	if _, err := polyAcctServing(t, body).Positions(context.Background()); err == nil {
		t.Fatal("want an error for a row with no quantity field, got nil")
	}
}

// A paginated response must be an ERROR, never a partial map: a short read is
// indistinguishable from "flat" to every caller, which is the exact failure
// mode this whole file exists to prevent.
func TestPolyUSPositionsRefusesPartialPage(t *testing.T) {
	body := `{"positions":{"a":{"netPosition":"3"}},"nextCursor":"abc","eof":false}`
	if _, err := polyAcctServing(t, body).Positions(context.Background()); err == nil {
		t.Fatal("want an error for a paginated response, got nil")
	}
}

// The exact body returned by GET /portfolio/positions on 2026-08-25 while
// Kalshi held 5 contracts. The previous fixture here was WRITTEN FROM THE SAME
// GUESS AS THE PARSER ("position": 4) — so it passed, and confirmed the bug
// instead of catching it. Kalshi returns position_fp, a fixed-point STRING.
const kalshiPositionsLive = `{"cursor":"","event_positions":[
  {"event_ticker":"KXGROK-GROK5","event_exposure_dollars":"0.700000","total_cost_shares_fp":"5.00"}],
"market_positions":[
  {"ticker":"KXGROK-GROK5-26OCT01","position_fp":"5.00","exchange_index":0,
   "market_exposure_dollars":"0.700000","fees_paid_dollars":"0.042200"}]}`

func kalshiAcctServing(t *testing.T, body string) *KalshiAccount {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewKalshiAccount(srv.URL, func(m, p string) (http.Header, error) {
		return http.Header{}, nil
	})
}

func TestKalshiPositionsParsesLiveShape(t *testing.T) {
	pos, err := kalshiAcctServing(t, kalshiPositionsLive).Positions(context.Background())
	if err != nil {
		t.Fatalf("Positions: %v", err)
	}
	// Reading the wrong key returns an EMPTY map here, which every caller reads
	// as "flat". That is how a real 5-contract Kalshi book stayed invisible to
	// the drift monitor and the silent-fill verifier for the life of the bot.
	if len(pos) != 1 {
		t.Fatalf("want 1 position, got %d: %v", len(pos), pos)
	}
	if got := pos["KXGROK-GROK5-26OCT01"]; got != 5 {
		t.Errorf("KXGROK-GROK5-26OCT01: want 5, got %v", got)
	}
}

// The older integer `position` shape must still parse, and a genuine zero must
// still be filtered out.
func TestKalshiPositionsAcceptsLegacyIntegerShape(t *testing.T) {
	body := `{"cursor":"","market_positions":[
		{"ticker":"KXBTC-26DEC31-250K","position":4},
		{"ticker":"KXSENATE-ME","position":-2},
		{"ticker":"KXFLAT","position":0}]}`
	pos, err := kalshiAcctServing(t, body).Positions(context.Background())
	if err != nil {
		t.Fatalf("Positions: %v", err)
	}
	if len(pos) != 2 { // the zero position is filtered out
		t.Fatalf("want 2, got %d: %v", len(pos), pos)
	}
	if pos["KXBTC-26DEC31-250K"] != 4 || pos["KXSENATE-ME"] != -2 {
		t.Errorf("bad parse: %v", pos)
	}
}

// position_fp wins when both are present: it carries the fractional part.
func TestKalshiPositionsPrefersFixedPoint(t *testing.T) {
	body := `{"cursor":"","market_positions":[{"ticker":"T","position":0,"position_fp":"5.00"}]}`
	pos, err := kalshiAcctServing(t, body).Positions(context.Background())
	if err != nil {
		t.Fatalf("Positions: %v", err)
	}
	if pos["T"] != 5 {
		t.Errorf("want 5 from position_fp, got %v", pos["T"])
	}
}

// If Kalshi renames the field AGAIN, fail loudly rather than reporting flat.
// "I could not read this" must never silently become "there is nothing there".
func TestKalshiPositionsRefusesUnreadableRow(t *testing.T) {
	body := `{"cursor":"","market_positions":[{"ticker":"KXGROK-GROK5-26OCT01","size":"5.00"}]}`
	_, err := kalshiAcctServing(t, body).Positions(context.Background())
	if err == nil {
		t.Fatal("want an error for a row with no readable position field, got nil")
	}
	if !strings.Contains(err.Error(), "KXGROK-GROK5-26OCT01") {
		t.Errorf("error should name the ticker, got: %v", err)
	}
}

// Same rule as Polymarket: a partial page is indistinguishable from "flat".
func TestKalshiPositionsRefusesPartialPage(t *testing.T) {
	body := `{"cursor":"abc","market_positions":[{"ticker":"T","position_fp":"3.00"}]}`
	if _, err := kalshiAcctServing(t, body).Positions(context.Background()); err == nil {
		t.Fatal("want an error for a paginated response, got nil")
	}
}
