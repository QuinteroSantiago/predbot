package backtest

import (
	"testing"

	"predbot/internal/core"
)

// Streaming must yield the SAME snapshots, in the same order, as the in-memory
// loader — just without holding them all.
func TestStreamMatchesLoad(t *testing.T) {
	dir := "/home/santiago/backups/predbot/data"
	want, err := LoadReplayHours(dir, 2)
	if err != nil || len(want) == 0 {
		t.Skip("no recent data to compare")
	}
	var got []core.BookSnapshot
	if err := StreamReplay(dir, 2, func(s core.BookSnapshot) error {
		got = append(got, s)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("stream yielded %d snapshots, loader yielded %d", len(got), len(want))
	}
	// Order must be non-decreasing in receive time.
	for i := 1; i < len(got); i++ {
		if got[i].TsRecv < got[i-1].TsRecv {
			t.Fatalf("stream out of order at %d: %d < %d", i, got[i].TsRecv, got[i-1].TsRecv)
		}
	}
	t.Logf("stream and loader agree: %d snapshots, correctly ordered", len(got))
}
