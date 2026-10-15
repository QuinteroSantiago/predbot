package live

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"predbot/internal/risk"
)

type fakeAcct struct {
	venue  string
	pos    map[string]float64
	orders []string
}

func (f *fakeAcct) Venue() string                                         { return f.venue }
func (f *fakeAcct) Positions(context.Context) (map[string]float64, error) { return f.pos, nil }
func (f *fakeAcct) OpenOrders(context.Context) ([]string, error)          { return f.orders, nil }
func (f *fakeAcct) BalanceUSD(context.Context) (float64, error)           { return 100, nil }

// A position the bot knows about must NOT kill.
func TestDriftIgnoresKnownPosition(t *testing.T) {
	eng := risk.New(risk.Config{}, nil, "")
	a := &fakeAcct{venue: "kalshi", pos: map[string]float64{"K": 5}}
	d := NewDriftMonitor([]AccountClient{a}, eng, func() map[string]float64 {
		return map[string]float64{"K": 5} // the bot believes exactly this
	})
	d.check(context.Background())
	if eng.Killed() {
		t.Error("a known, expected position must NOT trip the kill switch")
	}
}

// A position the bot does NOT know about must kill.
func TestDriftKillsOnUnexplainedPosition(t *testing.T) {
	eng := risk.New(risk.Config{}, nil, "")
	a := &fakeAcct{venue: "polymarketus", pos: map[string]float64{"P": -5}}
	d := NewDriftMonitor([]AccountClient{a}, eng, func() map[string]float64 {
		return map[string]float64{} // bot believes it holds nothing
	})
	d.check(context.Background())
	if !eng.Killed() {
		t.Error("an unexplained venue position MUST trip the kill switch")
	}
}

// A latched kill must stop the monitor. One real drift event on 2026-08-25
// produced 61 identical ERROR lines because Run had no Killed() guard, and a
// flooded journal is what hid the original incident.
func TestDriftMonitorStandsDownAfterKill(t *testing.T) {
	var polls int64
	a := &countingAcct{fakeAcct{venue: "polymarketus", pos: map[string]float64{"P": 5}}, &polls}
	eng := risk.New(risk.Config{}, nil, "")
	d := NewDriftMonitor([]AccountClient{a}, eng, func() map[string]float64 { return nil })
	d.Interval = 10 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()

	select {
	case <-done: // Run returned on its own — correct
	case <-ctx.Done():
		t.Fatal("drift monitor kept polling after the kill instead of standing down")
	}
	if !eng.Killed() {
		t.Fatal("expected the drift to have killed the engine")
	}
	// One poll detects and kills; the next tick sees Killed() and returns.
	if n := atomic.LoadInt64(&polls); n > 2 {
		t.Errorf("polled %d times after a latched kill; want <= 2", n)
	}
}

type countingAcct struct {
	fakeAcct
	n *int64
}

func (c *countingAcct) Positions(ctx context.Context) (map[string]float64, error) {
	atomic.AddInt64(c.n, 1)
	return c.fakeAcct.Positions(ctx)
}

// A market with a working maker rest is the rest lifecycle's to reconcile;
// drift must not race it to a kill. Other markets are still checked.
func TestDriftSkipsRestingMarket(t *testing.T) {
	eng := risk.New(risk.Config{}, nil, "")
	a := &fakeAcct{venue: "polymarketus", pos: map[string]float64{"P": -5}}
	d := NewDriftMonitor([]AccountClient{a}, eng, func() map[string]float64 { return nil })
	d.Skip = func(id string) bool { return id == "P" }
	d.check(context.Background())
	if eng.Killed() {
		t.Fatal("drift killed on a market its rest lifecycle owns")
	}
	a.pos = map[string]float64{"P": -5, "Q": 2}
	d.check(context.Background())
	if !eng.Killed() {
		t.Fatal("unexplained drift on a non-resting market must still kill")
	}
}
