package progress

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// Redirected output gets plain, newline-terminated status lines on an
// interval — no carriage returns or escape codes to litter a log file.
func TestRedirectedPrintsPlainLines(t *testing.T) {
	old := Interval
	Interval = 20 * time.Millisecond
	defer func() { Interval = old }()

	var out syncBuf
	b := start(&out, false, "pass", 100, func(done, total int64, _ time.Duration) string {
		return Count(done) + "/" + Count(total)
	})
	b.Add(40)
	time.Sleep(70 * time.Millisecond)
	b.Finish()
	b.Finish() // idempotent

	got := out.String()
	if strings.ContainsAny(got, "\r\x1b") {
		t.Fatalf("redirected output has terminal control codes: %q", got)
	}
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) < 2 {
		t.Fatalf("want periodic lines plus a final one, got %q", got)
	}
	if !strings.Contains(lines[0], " 40.0%") || !strings.Contains(lines[0], "ETA") {
		t.Errorf("periodic line = %q, want 40%% with an ETA", lines[0])
	}
	if last := lines[len(lines)-1]; !strings.Contains(last, "100.0%") || !strings.Contains(last, "done in") {
		t.Errorf("final line = %q, want 100%% and done", last)
	}
}

func TestSetNeverMovesBackwards(t *testing.T) {
	b := start(&syncBuf{}, false, "x", 10, nil)
	defer b.Finish()
	b.Set(7)
	b.Set(3)
	if got := b.Counter().Load(); got != 7 {
		t.Fatalf("done = %d after Set(7), Set(3); want 7", got)
	}
}

// A nil bar is how callers turn progress off, so every method must be safe.
func TestNilBarIsNoop(t *testing.T) {
	var b *Bar
	b.Add(1)
	b.Set(5)
	b.Finish()
	if b.Counter() != nil {
		t.Fatal("nil bar returned a counter")
	}
}

func TestCount(t *testing.T) {
	for n, want := range map[int64]string{950: "950", 12_345: "12.3k", 4_560_000: "4.56M", 2_500_000_000: "2.50G"} {
		if got := Count(n); got != want {
			t.Errorf("Count(%d) = %q, want %q", n, got, want)
		}
	}
}
