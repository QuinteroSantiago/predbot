package public

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"predbot/internal/core"
	"predbot/internal/obs"
	"predbot/internal/venue"
	"predbot/internal/venue/polymarketus"
)

const polyName = "polymarketus"

// PolyUSREST polls Polymarket US's public gateway order-book endpoint:
//
//	GET {base}/v1/markets/{slug}/book  ->  {"marketData":{...}}
//
// The inner marketData object has the same shape as the WS MARKET_DATA frame, so
// normalization reuses polymarketus.ParseMarketData verbatim.
type PolyUSREST struct {
	base     string
	interval time.Duration
	rec      venue.RawRecorder
	met      *obs.Metrics
	log      *slog.Logger
}

func NewPolyUSREST(base string, interval time.Duration, rec venue.RawRecorder, met *obs.Metrics) *PolyUSREST {
	return &PolyUSREST{base: base, interval: interval, rec: rec, met: met,
		log: slog.With("venue", polyName, "source", "rest")}
}

func (p *PolyUSREST) Name() string { return polyName }

func (p *PolyUSREST) Stream(ctx context.Context, slugs []string, out chan<- core.BookSnapshot) error {
	for ctx.Err() == nil {
		for _, s := range slugs {
			snap, ok := p.poll(ctx, s)
			if ok {
				emit(ctx, out, snap, p.met, polyName)
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
		if sleepCtx(ctx, p.interval) != nil {
			return ctx.Err()
		}
	}
	return ctx.Err()
}

func (p *PolyUSREST) poll(ctx context.Context, slug string) (core.BookSnapshot, bool) {
	url := fmt.Sprintf("%s/v1/markets/%s/book", p.base, slug)
	body, code, err := getJSON(ctx, url)
	now := time.Now()
	if err != nil {
		p.log.Warn("poll failed", "slug", slug, "err", err)
		return core.BookSnapshot{}, false
	}
	if code != 200 {
		p.log.Warn("non-200 from book", "slug", slug, "code", code)
		return core.BookSnapshot{}, false
	}
	_ = p.rec.WriteRaw(polyName, body, now)

	// Unwrap {"marketData":{...}} and reuse the WS parser on the inner object.
	var w struct {
		MarketData json.RawMessage `json:"marketData"`
	}
	if err := json.Unmarshal(body, &w); err != nil || len(w.MarketData) == 0 {
		p.met.ParseErrors.Add(ctx, 1, obs.Venue(polyName))
		return core.BookSnapshot{}, false
	}
	snap, ok, err := polymarketus.ParseMarketData(w.MarketData, now)
	if err != nil || !ok {
		p.met.ParseErrors.Add(ctx, 1, obs.Venue(polyName))
		p.log.Debug("parse error", "slug", slug, "err", err, "ok", ok)
		return core.BookSnapshot{}, false
	}
	return snap, true
}
