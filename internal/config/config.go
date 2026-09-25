// Package config loads config.yaml (venues, data dir, strategy and live-trader
// tuning) and markets.yaml (canonical ID -> venue tickers). Secrets are never
// here; they come from the environment (design §14). Size caps, the loss
// floor, the mode and the leagues are NOT here: they are flags in the
// predbot-live unit, where each arming decision stays visible.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Venue API bases: the defaults for config.yaml's api_url keys, and what the
// operator tools (which load no config) use directly. Kalshi serves trading,
// account and public market data from one base; Polymarket US splits the
// authenticated API from the public gateway.
const (
	KalshiAPI   = "https://api.elections.kalshi.com/trade-api/v2"
	PolyAPI     = "https://api.polymarket.us"     // authenticated trading + account
	PolyWS      = "wss://api.polymarket.us"       // private and markets WebSockets
	PolyGateway = "https://gateway.polymarket.us" // public market and sports data
)

// Config is config.yaml.
type Config struct {
	DataDir string `yaml:"data_dir"`
	// PollIntervalMS is the public-REST poll cadence (design §16 open item #5).
	PollIntervalMS int `yaml:"poll_interval_ms"`
	Kalshi         struct {
		WSURL   string `yaml:"ws_url"`
		Path    string `yaml:"ws_path"`  // path used for request signing
		RESTURL string `yaml:"rest_url"` // public REST base (no auth)
		APIURL  string `yaml:"api_url"`  // authenticated trading/account base, default KalshiAPI
	} `yaml:"kalshi"`
	PolymarketUS struct {
		WSURL   string `yaml:"ws_url"`
		RESTURL string `yaml:"rest_url"` // public gateway base (no auth)
		APIURL  string `yaml:"api_url"`  // authenticated trading/account base, default PolyAPI
	} `yaml:"polymarketus"`

	// Strategy is shared by the live trader, the recorder's paper trader and
	// the backtester, so all three judge an opportunity the same way.
	Strategy Strategy `yaml:"-"`
	// Live is the live trader's timing and tolerance tuning.
	Live Live `yaml:"live"`

	// The YAML key was "backtest:" until 2026-09-25, although the live trader
	// read it too. Both are accepted (not together); see resolveStrategy.
	StrategyYAML *Strategy `yaml:"strategy"`
	LegacyYAML   *Strategy `yaml:"backtest"`
}

// Strategy is how an opportunity is priced and filtered.
type Strategy struct {
	LatencyMS     int     `yaml:"latency_ms"`      // backtest only: §10.2 fill delay, default 150
	MinEdge       int64   `yaml:"min_edge"`        // §9.1 per-contract net edge, $0.0001, default 100 (1¢)
	MaxClip       int64   `yaml:"max_clip"`        // paper/backtest clip, Qty units, default 5000 (50 contracts); live uses -max-order-contracts
	MinDepth      int64   `yaml:"min_depth"`       // paper/backtest liquidity floor, Qty units (0 = off); live uses -min-depth-contracts
	KalshiFeeMult float64 `yaml:"kalshi_fee_mult"` // §6.1, default 0.07
	MaxBookAgeMS  int64   `yaml:"max_book_age_ms"` // stale-book guard, default 5000
	MaxSkewMS     int64   `yaml:"max_skew_ms"`     // max cross-venue capture-time skew to act on, default 1000
	IdleYield     float64 `yaml:"idle_yield"`      // backtest only: §10.4 idle-balance annual yield, default 0.04
}

// Live is the live trader's timing and tolerance tuning. A zero field takes
// its DefaultLive value; Validate enforces the rules that tie them together.
type Live struct {
	// FillWait is how long an order waits for venue fill events before the
	// venue's positions are asked instead.
	FillWait time.Duration `yaml:"fill_wait"`
	// RestPoll is how often a working maker rest's venue position is checked.
	RestPoll time.Duration `yaml:"rest_poll"`
	// HedgeGrace is how long a legged pair gets to complete its missing side
	// before the naked leg is dumped; HedgeRetry is how often it re-attempts.
	HedgeGrace time.Duration `yaml:"hedge_grace"`
	HedgeRetry time.Duration `yaml:"hedge_retry"`
	// HedgeSlip is how far a hedge/exit IOC crosses past the touch so it fills
	// on a moved book; MaxCompletionLoss caps how far underwater a completing
	// hedge may be before the leg is dumped instead. Both $0.0001 per contract.
	HedgeSlip         int64 `yaml:"hedge_slip"`
	MaxCompletionLoss int64 `yaml:"max_completion_loss"`
	// MaxUnhedgedFor kills the engine when naked contracts are held this long.
	MaxUnhedgedFor time.Duration `yaml:"max_unhedged_for"`
	// MaxErrors venue errors within ErrorWindow kill the engine.
	MaxErrors   int           `yaml:"max_errors"`
	ErrorWindow time.Duration `yaml:"error_window"`
	// SlowAck pauses new pairs on a venue for SlowAckPause after an order ack
	// slower than this (a pair's hedge lands only as fast as its ack).
	SlowAck      time.Duration `yaml:"slow_ack"`
	SlowAckPause time.Duration `yaml:"slow_ack_pause"`
	// SportsEntryTTL is how long after a game's start (or the entry, if later)
	// a sports market stays marked entered — hold to resolution, never re-enter.
	SportsEntryTTL time.Duration `yaml:"sports_entry_ttl"`
	// LoopTick is the trading loop's evaluation cadence.
	LoopTick time.Duration `yaml:"loop_tick"`
}

// DefaultLive is the live tuning the trader runs with unless config.yaml
// overrides a field. It is the one place these defaults are written down.
func DefaultLive() Live {
	return Live{
		// 1s: a live fill event arrives in ~1ms; anything slower is a lost
		// event, which the venue position check resolves better than waiting.
		FillWait:   1 * time.Second,
		RestPoll:   2 * time.Second,
		HedgeGrace: 30 * time.Second,
		HedgeRetry: 2 * time.Second,
		// 2c: cross two 1c levels — enough to clear a touch that repriced a
		// tick or a thin top level, small enough never to be costly.
		HedgeSlip: 200,
		// 2c/contract: wider than normal fee drag, tighter than a dislocation.
		MaxCompletionLoss: 200,
		MaxUnhedgedFor:    60 * time.Second,
		MaxErrors:         10,
		ErrorWindow:       60 * time.Second,
		SlowAck:           1500 * time.Millisecond,
		SlowAckPause:      10 * time.Minute,
		SportsEntryTTL:    12 * time.Hour,
		LoopTick:          500 * time.Millisecond,
	}
}

// withDefaults fills every zero field from DefaultLive.
func (l Live) withDefaults() Live {
	d := DefaultLive()
	setDur := func(v *time.Duration, def time.Duration) {
		if *v == 0 {
			*v = def
		}
	}
	setDur(&l.FillWait, d.FillWait)
	setDur(&l.RestPoll, d.RestPoll)
	setDur(&l.HedgeGrace, d.HedgeGrace)
	setDur(&l.HedgeRetry, d.HedgeRetry)
	setDur(&l.MaxUnhedgedFor, d.MaxUnhedgedFor)
	setDur(&l.ErrorWindow, d.ErrorWindow)
	setDur(&l.SlowAck, d.SlowAck)
	setDur(&l.SlowAckPause, d.SlowAckPause)
	setDur(&l.SportsEntryTTL, d.SportsEntryTTL)
	setDur(&l.LoopTick, d.LoopTick)
	if l.HedgeSlip == 0 {
		l.HedgeSlip = d.HedgeSlip
	}
	if l.MaxCompletionLoss == 0 {
		l.MaxCompletionLoss = d.MaxCompletionLoss
	}
	if l.MaxErrors == 0 {
		l.MaxErrors = d.MaxErrors
	}
	return l
}

// Validate rejects tuning the trader can't run safely.
func (l Live) Validate() error {
	for name, d := range map[string]time.Duration{
		"fill_wait": l.FillWait, "rest_poll": l.RestPoll, "hedge_grace": l.HedgeGrace,
		"hedge_retry": l.HedgeRetry, "max_unhedged_for": l.MaxUnhedgedFor,
		"error_window": l.ErrorWindow, "slow_ack": l.SlowAck, "slow_ack_pause": l.SlowAckPause,
		"sports_entry_ttl": l.SportsEntryTTL, "loop_tick": l.LoopTick,
	} {
		if d < 0 {
			return fmt.Errorf("config: live.%s must not be negative (got %s)", name, d)
		}
	}
	// The hedge must get its full grace window before the unhedged kill can
	// fire, or the bot kills itself in the middle of its own recovery.
	if l.HedgeGrace >= l.MaxUnhedgedFor {
		return fmt.Errorf("config: live.hedge_grace (%s) must be shorter than live.max_unhedged_for (%s)",
			l.HedgeGrace, l.MaxUnhedgedFor)
	}
	if l.HedgeRetry >= l.HedgeGrace {
		return fmt.Errorf("config: live.hedge_retry (%s) must be shorter than live.hedge_grace (%s)",
			l.HedgeRetry, l.HedgeGrace)
	}
	if l.HedgeSlip < 0 || l.MaxCompletionLoss < 0 || l.MaxErrors < 1 {
		return fmt.Errorf("config: live.hedge_slip and max_completion_loss must be >= 0 and max_errors >= 1")
	}
	return nil
}

// resolveStrategy picks the strategy block from "strategy:" or, for configs
// written before 2026-09-25, "backtest:".
func (c *Config) resolveStrategy() error {
	switch {
	case c.StrategyYAML != nil && c.LegacyYAML != nil:
		return fmt.Errorf("config: both strategy: and backtest: are set — keep only strategy:")
	case c.StrategyYAML != nil:
		c.Strategy = *c.StrategyYAML
	case c.LegacyYAML != nil:
		slog.Warn("config: the backtest: block is deprecated — rename it to strategy: (the live trader reads it too)")
		c.Strategy = *c.LegacyYAML
	}
	c.StrategyYAML, c.LegacyYAML = nil, nil
	return nil
}

// applyStrategyDefaults fills unset strategy tunables (design §10, §14).
func (c *Config) applyStrategyDefaults() {
	b := &c.Strategy
	if b.LatencyMS == 0 {
		b.LatencyMS = 150
	}
	if b.MinEdge == 0 {
		b.MinEdge = 100
	}
	if b.MaxClip == 0 {
		b.MaxClip = 5000
	}
	if b.KalshiFeeMult == 0 {
		b.KalshiFeeMult = 0.07
	}
	if b.MaxBookAgeMS == 0 {
		b.MaxBookAgeMS = 5000
	}
	if b.MaxSkewMS == 0 {
		b.MaxSkewMS = 1000
	}
	if b.IdleYield == 0 {
		b.IdleYield = 0.04 // ~idle-balance yield [VERIFY Kalshi rate]
	}
}

// Market is one canonical market mapped to venue-specific identifiers (§12).
// resolves_note documents whether both venues resolve on the same source; a
// mismatch is not tradeable regardless of edge.
type Market struct {
	Canonical      string `yaml:"canonical"`
	ResolvesNote   string `yaml:"resolves_note"`
	ResolutionDate string `yaml:"resolution_date"`
	Venues         struct {
		Kalshi struct {
			Ticker string `yaml:"ticker"`
		} `yaml:"kalshi"`
		PolymarketUS struct {
			Slug string `yaml:"slug"`
		} `yaml:"polymarketus"`
	} `yaml:"venues"`
}

// Load reads and validates config.yaml.
func Load(path string) (*Config, error) {
	c := &Config{}
	if err := readYAML(path, c); err != nil {
		return nil, err
	}
	if c.DataDir == "" {
		return nil, fmt.Errorf("config: data_dir is required")
	}
	if c.Kalshi.WSURL == "" && c.Kalshi.RESTURL == "" {
		return nil, fmt.Errorf("config: kalshi needs ws_url or rest_url")
	}
	if c.PolymarketUS.WSURL == "" && c.PolymarketUS.RESTURL == "" {
		return nil, fmt.Errorf("config: polymarketus needs ws_url or rest_url")
	}
	if c.PollIntervalMS <= 0 {
		c.PollIntervalMS = 3000
	}
	if c.Kalshi.APIURL == "" {
		c.Kalshi.APIURL = KalshiAPI
	}
	if c.PolymarketUS.APIURL == "" {
		c.PolymarketUS.APIURL = PolyAPI
	}
	if err := c.resolveStrategy(); err != nil {
		return nil, err
	}
	c.applyStrategyDefaults()
	c.Live = c.Live.withDefaults()
	if err := c.Live.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// LoadMarkets reads markets.yaml.
func LoadMarkets(path string) ([]Market, error) {
	var m []Market
	if err := readYAML(path, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// CanonicalByVenueID maps every venue-specific market id (Kalshi ticker,
// Polymarket slug) back to its canonical market. Venue APIs report positions by
// their own id, so this is what turns a venue position into something the risk
// engine — which keys by canonical market — can account for.
func CanonicalByVenueID(ms []Market) map[string]string {
	out := make(map[string]string, len(ms)*2)
	for _, m := range ms {
		if t := m.Venues.Kalshi.Ticker; t != "" {
			out[t] = m.Canonical
		}
		if s := m.Venues.PolymarketUS.Slug; s != "" {
			out[s] = m.Canonical
		}
	}
	return out
}

// KalshiTickers returns the Kalshi market tickers to subscribe to.
func KalshiTickers(ms []Market) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		if t := m.Venues.Kalshi.Ticker; t != "" {
			out = append(out, t)
		}
	}
	return out
}

// PolyUSSlugs returns the Polymarket US slugs to subscribe to.
func PolyUSSlugs(ms []Market) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		if s := m.Venues.PolymarketUS.Slug; s != "" {
			out = append(out, s)
		}
	}
	return out
}

func readYAML(path string, dst any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(b, dst); err != nil {
		return fmt.Errorf("config: parse %s: %w", path, err)
	}
	return nil
}
