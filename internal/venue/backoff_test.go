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
