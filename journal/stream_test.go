package journal

import (
	"bufio"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStreamRotatesHourlyAndAppends(t *testing.T) {
	dir := t.TempDir()
	h14 := time.Date(2026, 8, 22, 14, 30, 0, 0, time.UTC)
	s, err := NewStream(dir, "games")
	if err != nil {
		t.Fatal(err)
	}
	s.Write("mlb", []byte(`{"n":1}`), h14)
	s.Write("mlb", []byte(`{"n":2}`), h14.Add(time.Hour))
	s.Close()
	// A restart within the hour appends, never truncates.
	s, _ = NewStream(dir, "games")
	s.Write("mlb", []byte(`{"n":3}`), h14)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if got := readGz(t, filepath.Join(dir, "games", "mlb-2026-08-22T14.jsonl.gz")); len(got) != 2 || got[1] != `{"n":3}` {
		t.Fatalf("14:00 file = %q; want both writes in order", got)
	}
	if got := readGz(t, filepath.Join(dir, "games", "mlb-2026-08-22T15.jsonl.gz")); len(got) != 1 {
		t.Fatalf("15:00 file = %q; want one line", got)
	}
}

// readGz reads every line of a (possibly multi-member) gzip file.
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
	var lines []string
	sc := bufio.NewScanner(gz)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines
}
