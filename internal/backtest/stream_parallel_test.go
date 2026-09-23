package backtest

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"predbot/internal/core"
)

// writeSyntheticArchive lays out `hours` hourly normalized files for two venues
// ending at the current hour, with `perHour` snapshots split between them.
//
// The timestamps carry deliberate jitter: the recorder's two ingesters share one
// output channel, so write order differs from receive order by microseconds, and
// perHour is kept above reorderWindow so the merge's reorder buffer is really
// exercised rather than the whole hour landing in the final drain.
func writeSyntheticArchive(t testing.TB, hours, perHour int) string {
	t.Helper()
	dir := t.TempDir()
	norm := filepath.Join(dir, "normalized")
	if err := os.MkdirAll(norm, 0o755); err != nil {
		t.Fatal(err)
	}
	venues := []string{"kalshi", "polymarketus"}
	base := time.Now().UTC().Truncate(time.Hour)
	for h := 0; h < hours; h++ {
		hourStart := base.Add(-time.Duration(hours-1-h) * time.Hour)
		stamp := hourStart.Format("2006-01-02T15")
		writers := make([]*gzip.Writer, len(venues))
		for i, v := range venues {
			f, err := os.Create(filepath.Join(norm, fmt.Sprintf("%s-%s.jsonl.gz", v, stamp)))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			writers[i] = gzip.NewWriter(f)
			defer writers[i].Close()
		}
		for i := 0; i < perHour; i++ {
			vi := i % len(venues)
			// Jitter of a few microseconds, alternating sign.
			jitter := int64((i%7)-3) * int64(time.Microsecond)
			ts := hourStart.Add(time.Duration(i)*time.Millisecond).UnixNano() + jitter
			s := core.BookSnapshot{
				Venue:    venues[vi],
				VenueID:  fmt.Sprintf("MKT-%d", i%11),
				YesBids:  []core.Level{{Px: core.Price(40 + i%5), Qty: core.Qty(100)}},
				YesAsks:  []core.Level{{Px: core.Price(60 + i%5), Qty: core.Qty(100)}},
				TsServer: ts - int64(time.Millisecond),
				TsRecv:   ts,
				Seq:      uint64(i),
				State:    core.StateOpen,
			}
			line, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := writers[vi].Write(append(line, '\n')); err != nil {
				t.Fatal(err)
			}
		}
		for _, w := range writers {
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
	return dir
}

func collect(t testing.TB, byHour map[string][]string, hours []string, workers int) []core.BookSnapshot {
	t.Helper()
	var got []core.BookSnapshot
	if err := streamHours(byHour, hours, workers, func(s core.BookSnapshot) error {
		got = append(got, s)
		return nil
	}); err != nil {
		t.Fatalf("workers=%d: %v", workers, err)
	}
	return got
}

// The whole point of the parallel decode: identical output to the serial walk.
// Not "equivalent" or "same set" — the sim is order-dependent, so every field of
// every snapshot must match at the same index.
func TestParallelDecodeMatchesSerial(t *testing.T) {
	dir := writeSyntheticArchive(t, 6, 10_000)
	byHour, err := hourlyFiles(dir, Window{Hours: 24})
	if err != nil {
		t.Fatal(err)
	}
	hours := sortedHours(byHour)
	if len(hours) != 6 {
		t.Fatalf("expected 6 hours of synthetic data, got %d", len(hours))
	}

	want := collect(t, byHour, hours, 1)
	if len(want) != 6*10_000 {
		t.Fatalf("serial walk yielded %d snapshots, want %d", len(want), 6*10_000)
	}
	for i := 1; i < len(want); i++ {
		if want[i].TsRecv < want[i-1].TsRecv {
			t.Fatalf("serial walk itself out of order at %d", i)
		}
	}

	for _, workers := range []int{2, 3, 4, 8, 16} {
		got := collect(t, byHour, hours, workers)
		if len(got) != len(want) {
			t.Fatalf("workers=%d: %d snapshots, want %d", workers, len(got), len(want))
		}
		for i := range got {
			// BookSnapshot holds level slices, so it is not comparable with ==.
			if !reflect.DeepEqual(got[i], want[i]) {
				g, _ := json.Marshal(got[i])
				w, _ := json.Marshal(want[i])
				t.Fatalf("workers=%d: snapshot %d differs\n got %s\nwant %s", workers, i, g, w)
			}
		}
	}
}

// An fn error must stop the walk and propagate unchanged, with no goroutine or
// file-handle leak behind it (the -race build and t.TempDir cleanup catch those).
func TestParallelDecodeStopsOnCallbackError(t *testing.T) {
	dir := writeSyntheticArchive(t, 4, 3_000)
	byHour, err := hourlyFiles(dir, Window{Hours: 24})
	if err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("stop here")
	for _, workers := range []int{1, 4, 16} {
		n := 0
		err := streamHours(byHour, sortedHours(byHour), workers, func(core.BookSnapshot) error {
			n++
			if n == 2_500 {
				return sentinel
			}
			return nil
		})
		if !errors.Is(err, sentinel) {
			t.Fatalf("workers=%d: got %v, want the callback's error", workers, err)
		}
		if n != 2_500 {
			t.Fatalf("workers=%d: callback ran %d times after returning an error", workers, n)
		}
	}
}

// A missing/torn hour must not abort the walk: the recorder's current-hour file
// is routinely mid-write, and that tolerance predates the parallel decode.
func TestParallelDecodeToleratesTornFile(t *testing.T) {
	dir := writeSyntheticArchive(t, 3, 1_000)
	byHour, err := hourlyFiles(dir, Window{Hours: 24})
	if err != nil {
		t.Fatal(err)
	}
	hours := sortedHours(byHour)
	// Truncate one venue file of the last hour to a partial gzip stream.
	victim := byHour[hours[len(hours)-1]][0]
	if err := os.WriteFile(victim, []byte{0x1f, 0x8b, 0x08, 0x00}, 0o644); err != nil {
		t.Fatal(err)
	}
	serial := collect(t, byHour, hours, 1)
	parallel := collect(t, byHour, hours, 8)
	if len(parallel) != len(serial) {
		t.Fatalf("torn file: parallel yielded %d, serial %d", len(parallel), len(serial))
	}
	if len(serial) == 0 {
		t.Fatal("torn file swallowed the whole archive")
	}
}

// BenchmarkReplayWorkers is the measurement that says whether more cores help on
// a given box: go test ./internal/backtest -run xxx -bench ReplayWorkers
func BenchmarkReplayWorkers(b *testing.B) {
	dir := writeSyntheticArchive(b, 8, 20_000)
	byHour, err := hourlyFiles(dir, Window{Hours: 24})
	if err != nil {
		b.Fatal(err)
	}
	hours := sortedHours(byHour)
	for _, workers := range []int{1, 2, 4, 8, 16} {
		b.Run(fmt.Sprintf("workers=%d", workers), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				n := 0
				if err := streamHours(byHour, hours, workers, func(core.BookSnapshot) error {
					n++
					return nil
				}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
