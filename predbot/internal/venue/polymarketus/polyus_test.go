package polymarketus

import (
	"testing"
	"time"

	"predbot/internal/core"
)

func TestParseMarketData(t *testing.T) {
	frame := []byte(`{"type":"MARKET_DATA","marketSlug":"fed-cut","state":"open",
		"transactTime":1700000000000,
		"bids":[{"px":{"value":"0.55","currency":"USD"},"qty":"300.00"}],
		"offers":[{"px":{"value":"0.60","currency":"USD"},"qty":"150"}]}`)
	snap, ok, err := ParseMarketData(frame, time.Unix(0, 7))
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if snap.VenueID != "fed-cut" {
		t.Errorf("slug = %q", snap.VenueID)
	}
	if bid, _ := snap.BestYesBid(); bid.Px != 5500 || bid.Qty != 30000 {
		t.Errorf("bid = %+v; want {5500 30000}", bid)
	}
	if ask, _ := snap.BestYesAsk(); ask.Px != 6000 || ask.Qty != 15000 {
		t.Errorf("ask = %+v; want {6000 15000}", ask)
	}
	// NO bid derived from YES ask: 1 - 0.60 = 0.40.
	if nb, _ := snap.BestNoBid(); nb.Px != 4000 {
		t.Errorf("no bid = %+v; want px 4000", nb)
	}
	if snap.Seq != 0 {
		t.Errorf("poly seq must be 0, got %d", snap.Seq)
	}
}

// The public gateway sends transactTime as an RFC3339 string and state as
// MARKET_STATE_OPEN (found by running the recorder, 2026-08-22). Both must parse.
func TestParseGatewayFormat(t *testing.T) {
	frame := []byte(`{"marketSlug":"fed-cut","state":"MARKET_STATE_OPEN",
		"transactTime":"2026-08-22T17:05:58.304480956Z",
		"bids":[{"px":{"value":"0.8440","currency":"USD"},"qty":"819.0000"}],
		"offers":[{"px":{"value":"0.8450","currency":"USD"},"qty":"9988.0000"}]}`)
	snap, ok, err := ParseMarketData(frame, time.Unix(0, 7))
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if snap.State != core.StateOpen {
		t.Errorf("state = %v; want open", snap.State)
	}
	if snap.TsServer == 0 {
		t.Error("RFC3339 transactTime did not parse")
	}
	if bid, _ := snap.BestYesBid(); bid.Px != 8440 || bid.Qty != 81900 {
		t.Errorf("bid = %+v; want {8440 81900}", bid)
	}
}

func TestParseNonMarketData(t *testing.T) {
	// A TRADE frame is not book data.
	if _, ok, err := ParseMarketData([]byte(`{"type":"TRADE","marketSlug":"x"}`), time.Now()); err != nil || ok {
		t.Errorf("trade frame: ok=%v err=%v", ok, err)
	}
	// Truncated frame errors, does not panic.
	if _, _, err := ParseMarketData([]byte(`{"type":"MARKET_DATA"`), time.Now()); err == nil {
		t.Error("expected error on truncated frame")
	}
}
