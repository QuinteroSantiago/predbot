// Package venue defines the ingester contract and a pluggable WebSocket
// transport. Each concrete venue (kalshi, polymarketus) owns its raw book
// reconstruction and emits venue-agnostic core.BookSnapshot values (design §3,
// §6.3, §7).
package venue

import (
	"context"

	"predbot/internal/core"
)

// Ingester streams one venue's normalized order books until ctx is cancelled.
// It reconnects with backoff internally and never returns a mutable book.
type Ingester interface {
	Name() string
	// Stream subscribes to markets and emits normalized snapshots on out. It
	// blocks until ctx is done, returning ctx.Err().
	Stream(ctx context.Context, markets []string, out chan<- core.BookSnapshot) error
}

// Conn is the minimal WebSocket surface the ingesters use. Abstracting it keeps
// the frame-processing logic testable with an in-memory fake and no network or
// venue credentials (design §15 chaos tests).
type Conn interface {
	// Read returns the next text/binary frame payload.
	Read(ctx context.Context) ([]byte, error)
	// Write sends a frame (used for subscribe / get_snapshot requests).
	Write(ctx context.Context, b []byte) error
	Close() error
}

// Dialer opens an authenticated connection to a venue WS endpoint. Auth headers
// (Kalshi RSA signature, Polymarket US API key) are the caller's concern; a
// Dialer that lacks credentials must return an error rather than dialing
// unauthenticated — both venues require auth even for public channels
// (design §6.1, §6.2).
type Dialer func(ctx context.Context) (Conn, error)
