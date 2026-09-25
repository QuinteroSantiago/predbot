package public

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"predbot/internal/core"
	"predbot/internal/obs"
	"predbot/internal/venue"
)

const kalshiName = core.VenueKalshi

// KalshiREST polls Kalshi's public order-book endpoint:
//
//	GET {base}/markets/{ticker}/orderbook?depth=N
//	-> {"orderbook_fp":{"yes_dollars":[[px,qty],...],"no_dollars":[[px,qty],...]}}
//
// yes_dollars are YES bids, no_dollars are NO bids; both are [price$, qty]
// decimal strings. Normalized to the YES instrument exactly as the WS path does
// (design §6.3): YES asks come from NO bids as (1 - p).
type KalshiREST struct {
	base     string
	depth    int
	interval time.Duration
	rec      venue.RawRecorder
	met      *obs.Metrics
	log      *slog.Logger
}

func NewKalshiREST(base string, depth int, interval time.Duration, rec venue.RawRecorder, met *obs.Metrics) *KalshiREST {
	return &KalshiREST{base: base, depth: depth, interval: interval, rec: rec, met: met,
		log: slog.With("venue", kalshiName, "source", "rest")}
}

func (k *KalshiREST) Name() string { return kalshiName }

func (k *KalshiREST) Stream(ctx context.Context, tickers []string, out chan<- core.BookSnapshot) error {
	for ctx.Err() == nil {
		for _, t := range tickers {
			snap, ok := k.poll(ctx, t)
			if ok {
				emit(ctx, out, snap, k.met, kalshiName)
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
		if sleepCtx(ctx, k.interval) != nil {
			return ctx.Err()
		}
	}
	return ctx.Err()
}

func (k *KalshiREST) poll(ctx context.Context, ticker string) (core.BookSnapshot, bool) {
	url := fmt.Sprintf("%s/markets/%s/orderbook?depth=%d", k.base, ticker, k.depth)
	body, code, err := getJSON(ctx, url)
	now := time.Now()
	if err != nil {
		k.log.Warn("poll failed", "ticker", ticker, "err", err)
		return core.BookSnapshot{}, false
	}
	if code != 200 {
		k.log.Warn("non-200 from orderbook", "ticker", ticker, "code", code)
		return core.BookSnapshot{}, false
	}
	_ = k.rec.WriteRaw(kalshiName, body, now)

	snap, err := normalizeKalshiOB(body, ticker, now)
	if err != nil {
		k.met.ParseErrors.Add(ctx, 1, obs.Venue(kalshiName))
		k.log.Debug("parse error", "ticker", ticker, "err", err)
		return core.BookSnapshot{}, false
	}
	return snap, true
}

type kalshiOB struct {
	OrderbookFP struct {
		Yes [][2]json.RawMessage `json:"yes_dollars"`
		No  [][2]json.RawMessage `json:"no_dollars"`
	} `json:"orderbook_fp"`
}

func normalizeKalshiOB(body []byte, ticker string, now time.Time) (core.BookSnapshot, error) {
	var ob kalshiOB
	if err := json.Unmarshal(body, &ob); err != nil {
		return core.BookSnapshot{}, err
	}
	yesBids, err := kLevels(ob.OrderbookFP.Yes, false)
	if err != nil {
		return core.BookSnapshot{}, err
	}
	// NO bids -> YES asks at (OneDollar - p) (design §6.3).
	yesAsks, err := kLevels(ob.OrderbookFP.No, true)
	if err != nil {
		return core.BookSnapshot{}, err
	}
	sort.Slice(yesBids, func(i, j int) bool { return yesBids[i].Px > yesBids[j].Px })
	sort.Slice(yesAsks, func(i, j int) bool { return yesAsks[i].Px < yesAsks[j].Px })
	return core.BookSnapshot{
		Venue:   kalshiName,
		VenueID: ticker,
		YesBids: yesBids,
		YesAsks: yesAsks,
		TsRecv:  now.UnixNano(),
		State:   core.StateOpen, // public orderbook implies an open market
	}, nil
}

// kLevels parses [price$, qty] pairs; invert=true maps a NO price p to (1 - p).
func kLevels(pairs [][2]json.RawMessage, invert bool) ([]core.Level, error) {
	out := make([]core.Level, 0, len(pairs))
	for _, p := range pairs {
		px, err := parseStrPrice(p[0])
		if err != nil {
			return nil, err
		}
		qty, err := parseStrQty(p[1])
		if err != nil {
			return nil, err
		}
		if invert {
			px = core.OneDollar - px
		}
		out = append(out, core.Level{Px: px, Qty: qty})
	}
	return out, nil
}

func parseStrPrice(raw json.RawMessage) (core.Price, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return core.ParsePrice(string(raw)) // tolerate a bare number
	}
	return core.ParsePrice(s)
}

func parseStrQty(raw json.RawMessage) (core.Qty, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return core.ParseQty(string(raw))
	}
	return core.ParseQty(s)
}
