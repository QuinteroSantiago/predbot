package journal

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"predbot/internal/core"
)

func TestRecorderRoundTrip(t *testing.T) {
	dir := t.TempDir()
	rec, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}

	ts := time.Date(2026, 8, 22, 14, 30, 0, 0, time.UTC)
	if err := rec.WriteRaw("kalshi", []byte(`{"type":"heartbeat"}`), ts); err != nil {
		t.Fatal(err)
	}
	snap := core.BookSnapshot{Venue: "kalshi", VenueID: "MKT", TsRecv: ts.UnixNano(),
		YesBids: []core.Level{{Px: 5500, Qty: 200}}}
	if err := rec.WriteNormalized(snap); err != nil {
		t.Fatal(err)
	}
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}

	// Raw file is named by venue and UTC hour, gzip, one JSONL line.
	rawPath := filepath.Join(dir, "raw", "kalshi-2026-08-22T14.jsonl.gz")
	lines := readGz(t, rawPath)
	if len(lines) != 1 {
		t.Fatalf("raw lines = %d; want 1", len(lines))
	}
	var rl rawLine
	if err := json.Unmarshal([]byte(lines[0]), &rl); err != nil {
		t.Fatal(err)
	}
	if rl.Venue != "kalshi" || rl.TsRecvNs != ts.UnixNano() {
		t.Errorf("raw line meta wrong: %+v", rl)
	}

	// Normalized file round-trips the snapshot.
	normPath := filepath.Join(dir, "normalized", "kalshi-2026-08-22T14.jsonl.gz")
	nlines := readGz(t, normPath)
	var got core.BookSnapshot
	if err := json.Unmarshal([]byte(nlines[0]), &got); err != nil {
		t.Fatal(err)
	}
	if got.VenueID != "MKT" || len(got.YesBids) != 1 || got.YesBids[0].Px != 5500 {
		t.Errorf("normalized round trip wrong: %+v", got)
	}
}

func TestRecorderHourlyRotation(t *testing.T) {
	dir := t.TempDir()
	rec, _ := New(dir)
	h1 := time.Date(2026, 8, 22, 14, 59, 0, 0, time.UTC)
	h2 := time.Date(2026, 8, 22, 15, 0, 0, 0, time.UTC)
	rec.WriteRaw("poly", []byte(`{"a":1}`), h1)
	rec.WriteRaw("poly", []byte(`{"a":2}`), h2)
	rec.Close()

	for _, hour := range []string{"14", "15"} {
		p := filepath.Join(dir, "raw", "poly-2026-08-22T"+hour+".jsonl.gz")
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected rotated file %s: %v", p, err)
		}
	}
}

func readGz(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	sc := bufio.NewScanner(gz)
	for sc.Scan() {
		if sc.Text() != "" {
			out = append(out, sc.Text())
		}
	}
	return out
}
