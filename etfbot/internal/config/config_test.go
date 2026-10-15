package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultAndShippedConfigValidate(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatalf("default config: %v", err)
	}
	c, err := Load("../../config/config.yaml")
	if err != nil {
		t.Fatalf("config/config.yaml: %v", err)
	}
	if c.Strategy.TopN != 3 || c.Strategy.Cash != "BIL" {
		t.Fatalf("shipped config = %+v", c.Strategy)
	}
}

func TestValidateRejectsSlotAboveWeightCap(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(p, []byte("strategy:\n  top_n: 2\n"), 0o640) // 50% slots vs a 40% cap
	if _, err := Load(p); err == nil {
		t.Fatal("top_n 2 with max_weight 0.40 must be refused")
	}
}
