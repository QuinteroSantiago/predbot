package backtest

import (
	"reflect"
	"testing"
	"time"

	"predbot/internal/core"
)

func TestWithinWindowPrunesHourFiles(t *testing.T) {
	files := []string{
		"d/normalized/kalshi-2026-08-22T17.jsonl.gz",
		"d/normalized/kalshi-2026-08-22T18.jsonl.gz",
		"d/normalized/polymarketus-2026-08-22T19.jsonl.gz",
		"d/trades/kalshi-trades-2026-08-22T18.jsonl.gz",
		"d/normalized/garbage.jsonl.gz",
	}
	at := func(s string) time.Time { t, _ := time.Parse(time.RFC3339, s); return t }
	for _, tc := range []struct {
		name string
		w    Window
		want []string
	}{
		{"unranged keeps all", Window{}, files},
		// An hour file stays if any part of its hour overlaps the range.
		{"from mid-hour", Window{From: at("2026-08-22T18:30:00Z")}, []string{files[1], files[2], files[3]}},
		{"to is exclusive", Window{To: at("2026-08-22T18:00:00Z")}, []string{files[0]}},
		{"both", Window{From: at("2026-08-22T18:00:00Z"), To: at("2026-08-22T19:00:00Z")}, []string{files[1], files[3]}},
	} {
		if got := withinWindow(files, tc.w); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A ranged replay yields exactly the snapshots inside [From, To), in order,
// including the partial boundary hours.
func TestStreamReplayRange(t *testing.T) {
	dir, _ := writeTradingArchive(t, 10, 20_000) // 20k snapshots at 5ms: ~100s from 2026-06-01T00:00Z
	var all []int64
	if err := StreamReplay(dir, Window{}, func(s core.BookSnapshot) error { all = append(all, s.TsRecv); return nil }); err != nil {
		t.Fatal(err)
	}
	from, to := time.Unix(0, all[len(all)/4]), time.Unix(0, all[3*len(all)/4])
	var want []int64
	for _, ts := range all {
		if ts >= from.UnixNano() && ts < to.UnixNano() {
			want = append(want, ts)
		}
	}
	var got []int64
	if err := StreamReplay(dir, Window{From: from, To: to}, func(s core.BookSnapshot) error { got = append(got, s.TsRecv); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(want) == 0 || !reflect.DeepEqual(got, want) {
		t.Fatalf("ranged replay: %d snapshots, want %d", len(got), len(want))
	}
}
