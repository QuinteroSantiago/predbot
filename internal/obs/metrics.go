package obs

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Metrics is the Phase-1 instrument set (design §13). Phase 1 records feeds
// only, so the trading gauges (unhedged, buying power, pnl, killed) are defined
// for wiring completeness but exercised in later phases.
//
// Counter instrument names deliberately omit the `_total` suffix: the OTLP →
// Prometheus translation in the LGTM stack appends `_total` itself, so e.g.
// `predbot_seq_gaps` is scraped as `predbot_seq_gaps_total`. Naming the
// instrument with `_total` here would yield a doubled `_total_total`. Histogram
// names keep their `_seconds` unit and are exposed as `_bucket`/`_sum`/`_count`.
type Metrics struct {
	BookAge        metric.Float64Histogram // -> predbot_book_age_seconds_*{venue}
	FeedLatency    metric.Float64Histogram // -> predbot_feed_latency_seconds_*{venue} (ts_recv - ts_server)
	SeqGaps        metric.Int64Counter     // -> predbot_seq_gaps_total{venue}
	Conflated      metric.Int64Counter     // -> predbot_conflated_snapshots_total{venue}
	Dropped        metric.Int64Counter     // -> predbot_dropped_snapshots_total{venue}
	Reconnects     metric.Int64Counter     // -> predbot_reconnects_total{venue}
	SnapshotsTotal metric.Int64Counter     // -> predbot_snapshots_total{venue} (Phase-1 throughput)
	ParseErrors    metric.Int64Counter     // -> predbot_parse_errors_total{venue}
}

// NewMetrics creates the instruments on the global meter provider. Any creation
// error is fatal-adjacent: telemetry from line one is a design goal, so we
// prefer to surface it rather than run blind.
func NewMetrics() (*Metrics, error) {
	m := otel.Meter(serviceName)
	var err error
	// Both latency histograms (book age, feed latency) carry real values from
	// sub-millisecond (an in-process channel hop, ~0.1ms) up to multi-second
	// stalls. OTel's DEFAULT explicit buckets start [0, 5, 10, 25, …]s, so 100%
	// of observations land in the single (0,5]s bucket and histogram_quantile
	// can only interpolate its edges — pinning every quantile panel to p50=2.5s
	// / p95=4.75s regardless of the true ms-scale latency (diagnosed 2026-08-26).
	// These fine boundaries resolve the ms range where the real distribution is.
	// NOTE: pre-existing series keep the old `le` buckets until they age out of
	// the query window; new series use these from the next recorder restart.
	latencyBuckets := []float64{
		0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5,
	}
	mk := func(name, desc, unit string) metric.Float64Histogram {
		h, e := m.Float64Histogram(name, metric.WithDescription(desc), metric.WithUnit(unit),
			metric.WithExplicitBucketBoundaries(latencyBuckets...))
		if e != nil {
			err = e
		}
		return h
	}
	ct := func(name, desc string) metric.Int64Counter {
		c, e := m.Int64Counter(name, metric.WithDescription(desc))
		if e != nil {
			err = e
		}
		return c
	}

	out := &Metrics{
		BookAge:        mk("predbot_book_age_seconds", "age of the freshest snapshot per venue", "s"),
		FeedLatency:    mk("predbot_feed_latency_seconds", "ts_recv - ts_server per venue", "s"),
		SeqGaps:        ct("predbot_seq_gaps", "Kalshi orderbook sequence gaps"),
		Conflated:      ct("predbot_conflated_snapshots", "snapshots skipped because the consumer was behind (book still authoritative)"),
		Dropped:        ct("predbot_dropped_snapshots", "stale full snapshots dropped on backpressure"),
		Reconnects:     ct("predbot_reconnects", "websocket reconnects per venue"),
		SnapshotsTotal: ct("predbot_snapshots", "normalized snapshots emitted per venue"),
		ParseErrors:    ct("predbot_parse_errors", "unparseable frames per venue"),
	}
	return out, err
}

// Venue returns a reusable attribute-set option for a venue label.
func Venue(v string) metric.MeasurementOption {
	return metric.WithAttributes(attribute.String("venue", v))
}

// MarketEdge is one market's current cross-venue gross edge, in cents, with the
// fillable size (contracts) behind it and the age of the last actual trade. A
// wide edge is a phantom in two ways that its quotes cannot reveal: near-zero
// size against a thin book, OR a quote that repriced far on ZERO volume (a lone
// maker moving/pulling the top of an illiquid book). Both qualifiers travel with
// the edge so consumers gate on them: a real opportunity has size AND has traded
// recently. TradeAgeSec is large (NoTradeAgeSec) when the market has not printed.
type MarketEdge struct {
	Market      string
	Category    string
	Cents       float64
	Size        float64 // fillable contracts at that edge (min of both tops)
	TradeAgeSec float64 // seconds since the last trade on either venue (NoTradeAgeSec if never)
}

// NoTradeAgeSec is the TradeAgeSec sentinel for a market with no observed trade.
// It is deliberately huge so any freshness gate (trade_age < window) rejects it.
const NoTradeAgeSec = 999999

// RegisterCrossEdgeGauge installs async gauges that, on each collection, ask
// snapshot() for the current cross-venue edge of every fresh market and report
// it as predbot_cross_edge_cents{market,category} plus the fillable size behind
// it as predbot_cross_edge_size{market,category}. This is the live arb surface:
// an edge above the ~3.25c fee wall is only a real opportunity when the size is
// also non-trivial — a wide edge on a one-lot book is not executable.
func RegisterCrossEdgeGauge(snapshot func() []MarketEdge) error {
	m := otel.Meter(serviceName)
	attrs := func(e MarketEdge) metric.MeasurementOption {
		return metric.WithAttributes(
			attribute.String("market", e.Market),
			attribute.String("category", e.Category))
	}
	_, errC := m.Float64ObservableGauge("predbot_cross_edge_cents",
		metric.WithDescription("current best cross-venue gross edge per market, in cents"),
		metric.WithUnit("cents"),
		metric.WithFloat64Callback(func(_ context.Context, o metric.Float64Observer) error {
			for _, e := range snapshot() {
				o.Observe(e.Cents, attrs(e))
			}
			return nil
		}))
	_, errS := m.Float64ObservableGauge("predbot_cross_edge_size",
		metric.WithDescription("fillable contracts behind the best cross-venue edge (min of both tops)"),
		// Unit "{contract}" is an OTel annotation the OTLP→Prometheus translator
		// drops from the name; plain "1" would append a bogus `_ratio` suffix.
		metric.WithUnit("{contract}"),
		metric.WithFloat64Callback(func(_ context.Context, o metric.Float64Observer) error {
			for _, e := range snapshot() {
				o.Observe(e.Size, attrs(e))
			}
			return nil
		}))
	// Trade-age is the third qualifier: a wide, deep-looking edge whose quotes
	// last moved with ZERO volume behind them is a phantom (a lone maker repricing
	// an illiquid book). A real opportunity has edge AND size AND recent volume, so
	// the dashboard's "real opportunities" panel gates on all three.
	_, errT := m.Float64ObservableGauge("predbot_cross_edge_trade_age_seconds",
		metric.WithDescription("seconds since the last actual trade on either venue for this market (large when the quote repriced on zero volume)"),
		metric.WithUnit("s"),
		metric.WithFloat64Callback(func(_ context.Context, o metric.Float64Observer) error {
			for _, e := range snapshot() {
				o.Observe(e.TradeAgeSec, attrs(e))
			}
			return nil
		}))
	return errors.Join(errC, errS, errT)
}

// Position is one venue+market holding, as the venue's own account reports it.
// Contracts is signed: positive is long YES, negative a short/naked leg.
type Position struct {
	Venue     string
	Market    string // canonical id when known, else the venue's own market id
	Contracts float64
}

// RegisterPositionsGauge installs an async gauge reporting live positions read
// from the venues themselves, as predbot_position_contracts{venue,market}. It is
// the authoritative "what do we actually hold right now" surface — worth watching
// even when the bot is idle, since a naked leg is an emergency.
func RegisterPositionsGauge(snapshot func() []Position) error {
	m := otel.Meter(serviceName)
	_, err := m.Float64ObservableGauge("predbot_position_contracts",
		metric.WithDescription("net contracts held per venue+market, from the venue's own account (signed)"),
		metric.WithUnit("{contract}"), // annotation unit → no Prometheus name suffix (see cross_edge_size)
		metric.WithFloat64Callback(func(_ context.Context, o metric.Float64Observer) error {
			for _, p := range snapshot() {
				o.Observe(p.Contracts, metric.WithAttributes(
					attribute.String("venue", p.Venue),
					attribute.String("market", p.Market)))
			}
			return nil
		}))
	return err
}

// VenueBalance is one venue's available balance in dollars (Kalshi cash /
// Polymarket buying power). Summing across venues gives total account value —
// the cash side of the portfolio, not marked-to-market open positions.
type VenueBalance struct {
	Venue string
	USD   float64
}

// RegisterBalanceGauge publishes each venue's available balance as
// predbot_balance_usd{venue}. A Grafana panel sums the series for total
// portfolio value. Lives in the always-on recorder (not the trade-only live
// bot) so the number is visible even when the bot is idle. On a read error the
// poller keeps the last-known value rather than reporting zero (see cmd/record).
func RegisterBalanceGauge(snapshot func() []VenueBalance) error {
	m := otel.Meter(serviceName)
	_, err := m.Float64ObservableGauge("predbot_balance_usd",
		metric.WithDescription("available balance per venue in USD (Kalshi cash / Poly buying power); sum = total portfolio cash"),
		metric.WithUnit("usd"),
		metric.WithFloat64Callback(func(_ context.Context, o metric.Float64Observer) error {
			for _, b := range snapshot() {
				o.Observe(b.USD, metric.WithAttributes(attribute.String("venue", b.Venue)))
			}
			return nil
		}))
	return err
}

// RegisterUnhedgedGauge publishes the live unhedged (naked-leg) exposure per
// market as predbot_unhedged_contracts{canonical}. Any nonzero value is a leg
// that filled without its hedge — the exact failure this surfaces on a
// dashboard. snapshot() returns canonical -> contracts.
func RegisterUnhedgedGauge(snapshot func() map[string]float64) error {
	m := otel.Meter(serviceName)
	_, err := m.Float64ObservableGauge("predbot_unhedged_contracts",
		metric.WithDescription("current unhedged (naked-leg) contracts per market — nonzero = a leg without its hedge"),
		metric.WithUnit("{contract}"),
		metric.WithFloat64Callback(func(_ context.Context, o metric.Float64Observer) error {
			for canonical, qty := range snapshot() {
				o.Observe(qty, metric.WithAttributes(attribute.String("canonical", canonical)))
			}
			return nil
		}))
	return err
}

// PaperStats is the paper account's state for the metrics gauges (design §17
// Phase 3): live-simulated P&L, no real money.
type PaperStats struct {
	NetUSD        float64
	GrossUSD      float64
	FeesUSD       float64
	CapitalUSD    float64
	Opportunities int
	OpenPositions int
	Faded         int
}

// RegisterPaperGauges installs async gauges reporting the paper account state,
// read from snap() on each collection.
func RegisterPaperGauges(snap func() PaperStats) error {
	m := otel.Meter(serviceName)
	g := func(name, desc, unit string, get func(PaperStats) float64) error {
		_, err := m.Float64ObservableGauge(name, metric.WithDescription(desc), metric.WithUnit(unit),
			metric.WithFloat64Callback(func(_ context.Context, o metric.Float64Observer) error {
				o.Observe(get(snap()))
				return nil
			}))
		return err
	}
	return errors.Join(
		g("predbot_paper_net_usd", "paper net P&L (live SimExecutor)", "usd", func(s PaperStats) float64 { return s.NetUSD }),
		g("predbot_paper_gross_usd", "paper gross captured", "usd", func(s PaperStats) float64 { return s.GrossUSD }),
		g("predbot_paper_fees_usd", "paper fees paid", "usd", func(s PaperStats) float64 { return s.FeesUSD }),
		g("predbot_paper_capital_usd", "paper capital locked", "usd", func(s PaperStats) float64 { return s.CapitalUSD }),
		g("predbot_paper_opportunities", "paper pairs taken", "1", func(s PaperStats) float64 { return float64(s.Opportunities) }),
		g("predbot_paper_open_positions", "paper open positions", "1", func(s PaperStats) float64 { return float64(s.OpenPositions) }),
		g("predbot_paper_faded", "paper pairs that faded within latency", "1", func(s PaperStats) float64 { return float64(s.Faded) }),
	)
}

// RegisterDataSizeGauge installs an async gauge reporting the on-disk size of
// the recorder's data dir, split by stream (raw/normalized). It's observed on
// each metric collection by walking the tree — cheap for the hourly-rotated,
// few-dozen-MB/day footprint here. Exposed as predbot_data_bytes{stream}.
func RegisterDataSizeGauge(dir string) error {
	m := otel.Meter(serviceName)
	_, err := m.Int64ObservableGauge("predbot_data_bytes",
		metric.WithDescription("bytes on disk in the recorder data dir, by stream"),
		metric.WithUnit("By"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			for _, stream := range []string{"raw", "normalized"} {
				o.Observe(dirBytes(filepath.Join(dir, stream)),
					metric.WithAttributes(attribute.String("stream", stream)))
			}
			return nil
		}))
	return err
}

// dirBytes sums the sizes of all regular files under root; 0 if root is absent.
func dirBytes(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil // skip unreadable entries rather than aborting the walk
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// Nop returns a Metrics whose instruments are all no-ops, for tests. It uses the
// global (possibly no-op) provider, so calls are safe even when OTel is unset.
func Nop() *Metrics {
	m, _ := NewMetrics()
	return m
}
