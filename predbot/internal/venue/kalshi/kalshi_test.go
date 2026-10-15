package kalshi

import (
	"testing"
	"time"

	"predbot/internal/core"
)

// Kalshi is YES-centric: a snapshot with a NO bid at 0.40 must become a YES ask
// at 0.60 (design §6.3).
func TestSnapshotNormalization(t *testing.T) {
	st := NewState()
	frame := []byte(`{"type":"orderbook_snapshot","seq":10,"sid":1,"msg":{
		"market_ticker":"MKT",
		"yes_dollars_fp":[["0.55",200],["0.54",100]],
		"no_dollars_fp":[["0.40",300]],
		"ts_ms":1700000000000,"status":"active"}}`)
	snap, emit, err := st.ProcessFrame(frame, time.Unix(0, 5))
	if err != nil || !emit {
		t.Fatalf("snapshot: emit=%v err=%v", emit, err)
	}
	// Qty is a decimal contract count: "200" -> 200 contracts -> 20000 (0.01 units).
	if bid, _ := snap.BestYesBid(); bid.Px != 5500 || bid.Qty != 20000 {
		t.Errorf("best yes bid = %+v; want {5500 20000}", bid)
	}
	// NO bid 0.40 -> YES ask 0.60.
	if ask, _ := snap.BestYesAsk(); ask.Px != 6000 || ask.Qty != 30000 {
		t.Errorf("best yes ask = %+v; want {6000 30000}", ask)
	}
	if snap.State != core.StateOpen || snap.Seq != 10 {
		t.Errorf("state=%v seq=%d", snap.State, snap.Seq)
	}
}

// The seq is GLOBAL per connection (shared across markets), so contiguous deltas
// apply; a negative delta_fp that zeroes a level removes it.
func TestDeltaApplyAndRemove(t *testing.T) {
	st := NewState()
	st.ProcessFrame([]byte(`{"type":"orderbook_snapshot","seq":1,"sid":1,"msg":{
		"market_ticker":"MKT","yes_dollars_fp":[["0.55",200]],"no_dollars_fp":[]}}`), time.Unix(0, 1))

	// seq 2 (next global seq): add depth to the 0.55 yes bid.
	snap, emit, err := st.ProcessFrame([]byte(`{"type":"orderbook_delta","seq":2,"sid":1,"msg":{
		"market_ticker":"MKT","price_dollars":"0.55","delta_fp":50,"side":"yes"}}`), time.Unix(0, 2))
	if err != nil || !emit {
		t.Fatalf("delta apply: emit=%v err=%v", emit, err)
	}
	if bid, _ := snap.BestYesBid(); bid.Qty != 25000 { // 200 + 50 contracts
		t.Errorf("after +50 delta qty = %d; want 25000", bid.Qty)
	}

	// seq 3: remove the level entirely.
	snap, _, _ = st.ProcessFrame([]byte(`{"type":"orderbook_delta","seq":3,"sid":1,"msg":{
		"market_ticker":"MKT","price_dollars":"0.55","delta_fp":-250,"side":"yes"}}`), time.Unix(0, 3))
	if _, ok := snap.BestYesBid(); ok {
		t.Error("level should have been removed at qty 0")
	}
}

// A break in the global sequence returns errSeqGap so the caller reconnects.
func TestSeqGapTriggersResync(t *testing.T) {
	st := NewState()
	st.ProcessFrame([]byte(`{"type":"orderbook_snapshot","seq":1,"sid":1,"msg":{
		"market_ticker":"MKT","yes_dollars_fp":[["0.55",200]],"no_dollars_fp":[]}}`), time.Unix(0, 1))

	// Expected global seq is 2; a delta at seq 5 is a gap.
	_, emit, err := st.ProcessFrame([]byte(`{"type":"orderbook_delta","seq":5,"sid":1,"msg":{
		"market_ticker":"MKT","price_dollars":"0.55","delta_fp":50,"side":"yes"}}`), time.Unix(0, 5))
	if emit {
		t.Error("gapped delta must not emit")
	}
	if err != errSeqGap {
		t.Errorf("err = %v; want errSeqGap", err)
	}
}

// A delta arriving before any snapshot is a gap (reconnect), not a panic.
func TestDeltaBeforeSnapshot(t *testing.T) {
	st := NewState()
	_, emit, err := st.ProcessFrame([]byte(`{"type":"orderbook_delta","seq":2,"sid":1,"msg":{
		"market_ticker":"MKT","price_dollars":"0.55","delta_fp":50,"side":"yes"}}`), time.Unix(0, 2))
	if emit || err != errSeqGap {
		t.Errorf("delta-before-snapshot: emit=%v err=%v; want errSeqGap", emit, err)
	}
}

// Multiple markets share one global seq: interleaved snapshots then deltas.
func TestGlobalSeqAcrossMarkets(t *testing.T) {
	st := NewState()
	// Two snapshots, seq 1 and 2 (one per market).
	st.ProcessFrame([]byte(`{"type":"orderbook_snapshot","seq":1,"sid":1,"msg":{"market_ticker":"A","yes_dollars_fp":[["0.50",100]],"no_dollars_fp":[]}}`), time.Unix(0, 1))
	st.ProcessFrame([]byte(`{"type":"orderbook_snapshot","seq":2,"sid":1,"msg":{"market_ticker":"B","yes_dollars_fp":[["0.60",100]],"no_dollars_fp":[]}}`), time.Unix(0, 2))
	// Delta for A at global seq 3 (not per-market seq 2) must apply.
	_, emit, err := st.ProcessFrame([]byte(`{"type":"orderbook_delta","seq":3,"sid":1,"msg":{"market_ticker":"A","price_dollars":"0.50","delta_fp":50,"side":"yes"}}`), time.Unix(0, 3))
	if err != nil || !emit {
		t.Fatalf("interleaved delta must apply: emit=%v err=%v", emit, err)
	}
}

// Truncated / garbage frames must return an error, never panic (design §15).
func TestTruncatedFrame(t *testing.T) {
	st := NewState()
	if _, _, err := st.ProcessFrame([]byte(`{"type":"orderbook_snapshot","msg":{`), time.Now()); err == nil {
		t.Error("expected error on truncated frame")
	}
	// Non-book frame is silently ignored.
	if _, emit, err := st.ProcessFrame([]byte(`{"type":"heartbeat"}`), time.Now()); err != nil || emit {
		t.Errorf("heartbeat: emit=%v err=%v", emit, err)
	}
}
