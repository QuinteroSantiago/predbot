// Package config loads config/config.yaml. Every field has a default written
// down in Default; the file overrides only what it sets.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"etfbot/internal/rebalance"
	"etfbot/internal/strategy"
)

type Config struct {
	Strategy  Strategy  `yaml:"strategy"`
	Rebalance Rebalance `yaml:"rebalance"`
	Risk      Risk      `yaml:"risk"`
}

type Strategy struct {
	Universe  []string `yaml:"universe"`
	Cash      string   `yaml:"cash"`
	TopN      int      `yaml:"top_n"`
	Lookbacks []int    `yaml:"lookbacks"` // trading days
	SMA       int      `yaml:"sma"`       // trading days
}

type Rebalance struct {
	MinTradeUSD float64 `yaml:"min_trade_usd"`
	CashBuffer  float64 `yaml:"cash_buffer"`
}

type Risk struct {
	// MaxWeight caps any one fund except the cash fund, which may hold everything.
	MaxWeight float64 `yaml:"max_weight"`
	// MaxOrderUSD caps one order's size in dollars.
	MaxOrderUSD float64 `yaml:"max_order_usd"`
	// MaxDrawdown engages the kill switch when equity falls this fraction
	// below its high-water mark.
	MaxDrawdown float64 `yaml:"max_drawdown"`
}

// Default is the configuration the bot runs with unless the file overrides it.
func Default() Config {
	return Config{
		Strategy: Strategy{
			Universe:  []string{"SPY", "QQQ", "IWM", "EFA", "EEM", "TLT", "IEF", "GLD", "DBC", "VNQ"},
			Cash:      "BIL",
			TopN:      3,
			Lookbacks: []int{63, 126, 252},
			SMA:       200,
		},
		Rebalance: Rebalance{MinTradeUSD: 25, CashBuffer: 0.005},
		Risk:      Risk{MaxWeight: 0.40, MaxOrderUSD: 5000, MaxDrawdown: 0.25},
	}
}

// Load reads path over the defaults. A missing file is an error: the bot
// should run on a configuration someone chose.
func Load(path string) (Config, error) {
	c := Default()
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := yaml.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("config %s: %w", path, err)
	}
	return c, c.Validate()
}

// Validate rejects a configuration the bot can't run safely.
func (c Config) Validate() error {
	s, r := c.Strategy, c.Risk
	switch {
	case len(s.Universe) == 0 || s.Cash == "":
		return fmt.Errorf("config: strategy.universe and strategy.cash are required")
	case s.TopN < 1 || s.SMA < 1 || len(s.Lookbacks) == 0:
		return fmt.Errorf("config: strategy.top_n, sma and lookbacks must be positive")
	case r.MaxWeight <= 0 || r.MaxWeight > 1:
		return fmt.Errorf("config: risk.max_weight must be in (0, 1]")
	case 1/float64(s.TopN) > r.MaxWeight+1e-9:
		return fmt.Errorf("config: one slot (1/top_n = %.2f) exceeds risk.max_weight %.2f", 1/float64(s.TopN), r.MaxWeight)
	case r.MaxOrderUSD <= 0 || r.MaxDrawdown <= 0 || r.MaxDrawdown >= 1:
		return fmt.Errorf("config: risk.max_order_usd must be positive and risk.max_drawdown in (0, 1)")
	case c.Rebalance.CashBuffer < 0 || c.Rebalance.CashBuffer >= 0.5:
		return fmt.Errorf("config: rebalance.cash_buffer must be in [0, 0.5)")
	}
	return nil
}

// Rotation builds the configured strategy.
func (c Config) Rotation() strategy.Rotation {
	s := c.Strategy
	return strategy.Rotation{Universe: s.Universe, Cash: s.Cash, TopN: s.TopN, Lookbacks: s.Lookbacks, SMA: s.SMA}
}

// Params is the rebalancer's tuning.
func (c Config) Params() rebalance.Params {
	return rebalance.Params{MinTradeUSD: c.Rebalance.MinTradeUSD, CashBuffer: c.Rebalance.CashBuffer}
}
