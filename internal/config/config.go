// Package config loads config.yaml (venues, data dir, risk limits) and
// markets.yaml (canonical ID -> venue tickers). Secrets are never here; they
// come from the environment (design §14). Phase 1 uses only the venue endpoints,
// data dir, and market map.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config is config.yaml. Risk limits are parsed but unused until Phase 2.
type Config struct {
	DataDir string `yaml:"data_dir"`
	// PollIntervalMS is the public-REST poll cadence (design §16 open item #5).
	PollIntervalMS int `yaml:"poll_interval_ms"`
	Kalshi         struct {
		WSURL   string `yaml:"ws_url"`
		Path    string `yaml:"ws_path"`  // path used for request signing
		RESTURL string `yaml:"rest_url"` // public REST base (no auth)
	} `yaml:"kalshi"`
	PolymarketUS struct {
		WSURL   string `yaml:"ws_url"`
		RESTURL string `yaml:"rest_url"` // public gateway base (no auth)
	} `yaml:"polymarketus"`
	Risk struct {
		MaxOrderQty int64 `yaml:"max_order_qty"`
		MaxUnhedged int64 `yaml:"max_unhedged"`
	} `yaml:"risk"`
	Backtest struct {
		LatencyMS     int     `yaml:"latency_ms"`      // §10.2 fill delay, default 150
		MinEdge       int64   `yaml:"min_edge"`        // §9.1 per-contract net edge, $0.0001, default 100 (1¢)
		MaxClip       int64   `yaml:"max_clip"`        // Qty units, default 5000 (50 contracts)
		KalshiFeeMult float64 `yaml:"kalshi_fee_mult"` // §6.1, default 0.07
		MaxBookAgeMS  int64   `yaml:"max_book_age_ms"` // stale-book guard, default 5000
		MaxSkewMS     int64   `yaml:"max_skew_ms"`     // max cross-venue capture-time skew to act on, default 1000
		IdleYield     float64 `yaml:"idle_yield"`      // §10.4 hurdle: idle-balance annual yield, default 0.04
	} `yaml:"backtest"`
}

// applyBacktestDefaults fills unset backtest tunables (design §10, §14).
func (c *Config) applyBacktestDefaults() {
	b := &c.Backtest
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
	c.applyBacktestDefaults()
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
