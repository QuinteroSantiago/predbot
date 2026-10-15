package venue

import (
	"context"
	"time"
)

// Reconnect delays for the venue WebSocket loops. A failed dial waits the
// current delay, which doubles up to MaxReconnectDelay; a successful dial
// resets it. Market-data feeds retry sooner than the private fill and trade
// streams: a missing book stops trading on that market until it's back.
const (
	FeedReconnectDelay   = 250 * time.Millisecond
	StreamReconnectDelay = 500 * time.Millisecond
	MaxReconnectDelay    = 30 * time.Second
)

// Backoff is a reconnect loop's exponential delay.
type Backoff struct {
	base, cur time.Duration
}

// NewBackoff starts a backoff at base.
func NewBackoff(base time.Duration) *Backoff { return &Backoff{base: base, cur: base} }

// Delay is how long the next Wait will sleep.
func (b *Backoff) Delay() time.Duration { return b.cur }

// Wait sleeps the current delay (or until ctx is done, returning its error),
// then doubles the delay up to MaxReconnectDelay.
func (b *Backoff) Wait(ctx context.Context) error {
	err := Sleep(ctx, b.cur)
	b.cur = min(b.cur*2, MaxReconnectDelay)
	return err
}

// Reset returns the delay to its base after a successful dial.
func (b *Backoff) Reset() { b.cur = b.base }

// Sleep waits d, or returns ctx's error if ctx is done first.
func Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
