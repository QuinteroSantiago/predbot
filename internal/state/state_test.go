package state

import (
	"path/filepath"
	"testing"
)

func TestRoundTripAndDiff(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	if _, found, err := Load(p); found || err != nil {
		t.Fatalf("missing file: found=%v err=%v; want a first run", found, err)
	}
	want := State{Holdings: map[string]float64{"SPY": 3.5}, PeakEquity: 1000}
	if err := Save(p, want); err != nil {
		t.Fatal(err)
	}
	got, found, err := Load(p)
	if !found || err != nil || got.Holdings["SPY"] != 3.5 || got.PeakEquity != 1000 {
		t.Fatalf("load = %+v %v %v", got, found, err)
	}
	if d := Diff(got.Holdings, map[string]float64{"SPY": 3.5}); len(d) != 0 {
		t.Fatalf("diff = %v; want none", d)
	}
	if d := Diff(got.Holdings, map[string]float64{"SPY": 3, "QQQ": 1}); len(d) != 2 {
		t.Fatalf("diff = %v; want SPY and QQQ", d)
	}
}
