package live

import (
	"context"
	"strings"
	"testing"

	"predbot/internal/risk"
)

// Reconcile used to WARN about pre-existing positions and return nil, so
// preflight printed "reconcile OK" and the bot armed — and then the drift
// monitor, reading the identical number 30s later, killed. On 2026-08-25 that
// left a 30-second window in which the bot was live and free to trade while
// holding a position it was about to kill itself over.
func TestReconcileRefusesPreExistingPositions(t *testing.T) {
	eng := risk.New(risk.Config{}, nil, "")
	sup := NewSupervisor([]AccountClient{
		&fakeAcct{venue: "kalshi", pos: map[string]float64{"KXGROK-GROK5-26OCT01": 5}},
		&fakeAcct{venue: "polymarketus", pos: nil},
	}, eng, nil)

	err := sup.Reconcile(context.Background())
	if err == nil {
		t.Fatal("want an error for a pre-existing position, got nil")
	}
	// The operator has to know WHICH position, on WHICH venue, and what to do.
	for _, want := range []string{"kalshi", "KXGROK-GROK5-26OCT01", "adopt-positions"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}

// A clean start must still be clean.
func TestReconcileAcceptsFlatVenues(t *testing.T) {
	eng := risk.New(risk.Config{}, nil, "")
	sup := NewSupervisor([]AccountClient{
		&fakeAcct{venue: "kalshi", pos: nil},
		&fakeAcct{venue: "polymarketus", pos: map[string]float64{}},
	}, eng, nil)
	if err := sup.Reconcile(context.Background()); err != nil {
		t.Fatalf("flat venues must reconcile cleanly, got: %v", err)
	}
}

// Opting in proceeds AND hands the positions back, so the caller can seed the
// ledger. Adopting without seeding would just move the kill 30 seconds later.
func TestAdoptedPositionsAreReportedForSeeding(t *testing.T) {
	eng := risk.New(risk.Config{}, nil, "")
	sup := NewSupervisor([]AccountClient{
		&fakeAcct{venue: "kalshi", pos: map[string]float64{"K": 5}},
		&fakeAcct{venue: "polymarketus", pos: map[string]float64{"P": -5}},
	}, eng, nil)
	sup.AdoptExisting = true

	if err := sup.Reconcile(context.Background()); err != nil {
		t.Fatalf("adopt must not refuse: %v", err)
	}
	got := sup.PreExisting()
	if got["K"] != 5 || got["P"] != -5 {
		t.Fatalf("PreExisting = %v; want K=5 P=-5", got)
	}
}

// The seeded ledger must make the drift monitor quiet — that is the whole point
// of adopting rather than refusing.
func TestAdoptedPositionsDoNotReadAsDrift(t *testing.T) {
	eng := risk.New(risk.Config{}, nil, "")
	tr := NewTrader(Executors{}, eng, nil, 0)
	tr.Adopt(map[string]float64{"KXGROK-GROK5-26OCT01": 5})

	if got := tr.Positions()["KXGROK-GROK5-26OCT01"]; got != 5 {
		t.Fatalf("adopted position = %v; want 5", got)
	}
	a := &fakeAcct{venue: "kalshi", pos: map[string]float64{"KXGROK-GROK5-26OCT01": 5}}
	d := NewDriftMonitor([]AccountClient{a}, eng, tr.Positions)
	d.check(context.Background())
	if eng.Killed() {
		t.Error("adopted position engaged the kill switch")
	}
}

// An open order left on a venue can fill at any moment with no pair behind it
// (2026-09-24), so startup refuses it even when positions are adoptable.
func TestReconcileRefusesOpenOrders(t *testing.T) {
	eng := risk.New(risk.Config{}, nil, "")
	sup := NewSupervisor([]AccountClient{
		&fakeAcct{venue: "kalshi"},
		&fakeAcct{venue: "polymarketus", orders: []string{"CPN9G2VCMXDQ"}},
	}, eng, nil)
	sup.AdoptExisting = true
	err := sup.Reconcile(context.Background())
	if err == nil || !strings.Contains(err.Error(), "CPN9G2VCMXDQ") {
		t.Fatalf("want a refusal naming the open order, got %v", err)
	}
}
