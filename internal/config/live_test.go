package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const venues = `data_dir: /tmp/x
kalshi: {rest_url: "https://k"}
polymarketus: {rest_url: "https://p"}
`

func load(t *testing.T, body string) (*Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(venues+body), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

func TestStrategyKeyAndLegacyBacktestKey(t *testing.T) {
	for _, key := range []string{"strategy", "backtest"} {
		c, err := load(t, key+":\n  min_depth: 1000\n  min_edge: 150\n")
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if c.Strategy.MinDepth != 1000 || c.Strategy.MinEdge != 150 || c.Strategy.KalshiFeeMult != 0.07 {
			t.Errorf("%s: strategy = %+v; want min_depth 1000, min_edge 150, fee default 0.07", key, c.Strategy)
		}
	}
	if _, err := load(t, "strategy: {min_edge: 1}\nbacktest: {min_edge: 2}\n"); err == nil {
		t.Error("both strategy: and backtest: must be refused")
	}
}

// An old config with the unused risk: block and no live: block still loads,
// with every live value at its default — what the VM has until config ships.
func TestOldConfigLoadsWithLiveDefaults(t *testing.T) {
	c, err := load(t, "backtest: {min_depth: 1000}\nrisk: {max_order_qty: 5000, max_unhedged: 20000}\n")
	if err != nil {
		t.Fatal(err)
	}
	if c.Live != DefaultLive() {
		t.Errorf("live = %+v; want defaults %+v", c.Live, DefaultLive())
	}
}

func TestLiveOverridesAndDurations(t *testing.T) {
	c, err := load(t, "live:\n  hedge_grace: 20s\n  slow_ack: 1500ms\n  sports_entry_ttl: 6h\n  hedge_slip: 300\n")
	if err != nil {
		t.Fatal(err)
	}
	if c.Live.HedgeGrace != 20*time.Second || c.Live.SlowAck != 1500*time.Millisecond ||
		c.Live.SportsEntryTTL != 6*time.Hour || c.Live.HedgeSlip != 300 {
		t.Errorf("overrides not applied: %+v", c.Live)
	}
	if c.Live.MaxUnhedgedFor != DefaultLive().MaxUnhedgedFor {
		t.Errorf("unset key should keep its default, got %s", c.Live.MaxUnhedgedFor)
	}
}

// The hedge must get its whole grace window before the unhedged kill fires,
// or the bot kills itself mid-recovery. That rule lived only in a comment.
func TestLiveValidation(t *testing.T) {
	cases := map[string]string{
		"live: {hedge_grace: 60s}":                        "hedge_grace",
		"live: {hedge_grace: 90s, max_unhedged_for: 60s}": "hedge_grace",
		"live: {hedge_retry: 45s}":                        "hedge_retry",
		"live: {fill_wait: -1s}":                          "fill_wait",
		"live: {hedge_slip: -100}":                        "hedge_slip",
	}
	for body, want := range cases {
		if _, err := load(t, body+"\n"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v; want it to name %s", body, err, want)
		}
	}
	if err := DefaultLive().Validate(); err != nil {
		t.Errorf("defaults must validate: %v", err)
	}
}
