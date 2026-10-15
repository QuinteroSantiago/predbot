package venue

import (
	"context"
	"testing"
	"time"
)

func TestBackoffDoublesCapsAndResets(t *testing.T) {
	b := NewBackoff(time.Millisecond)
	ctx := context.Background()
	want := []time.Duration{1, 2, 4, 8}
	for _, w := range want {
		if b.Delay() != w*time.Millisecond {
			t.Fatalf("delay = %s; want %s", b.Delay(), w*time.Millisecond)
		}
		if err := b.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	b.cur = MaxReconnectDelay / 2
	b.cur = min(b.cur*2, MaxReconnectDelay) // what Wait does, without sleeping 15s
	if b.Delay() != MaxReconnectDelay {
		t.Errorf("delay should cap at %s, got %s", MaxReconnectDelay, b.Delay())
	}
	b.Reset()
	if b.Delay() != time.Millisecond {
		t.Errorf("reset delay = %s; want the 1ms base", b.Delay())
	}
}

func TestSleepReturnsWhenContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := Sleep(ctx, time.Hour); err == nil || time.Since(start) > time.Second {
		t.Fatalf("Sleep on a done context = %v after %s; want its error at once", err, time.Since(start))
	}
}

func TestChunkSlugs(t *testing.T) {
	slugs := make([]string, 250)
	for i := range slugs {
		slugs[i] = string(rune('a' + i%26))
	}
	c := ChunkSlugs(slugs)
	if len(c) != 3 || len(c[0]) != 100 || len(c[1]) != 100 || len(c[2]) != 50 {
		t.Fatalf("chunks = %d (%d/%d/%d); want 100/100/50", len(c), len(c[0]), len(c[1]), len(c[2]))
	}
	if &c[1][0] != &slugs[100] {
		t.Error("chunks must be in order, sharing the slice")
	}
	if ChunkSlugs(nil) != nil {
		t.Error("no slugs, no chunks")
	}
}
