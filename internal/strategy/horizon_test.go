package strategy

import (
	"testing"
	"time"

	"predbot/internal/core"
)

func TestHorizonEdge(t *testing.T) {
	const base = core.Price(100) // 1c
	cases := []struct {
		until, step time.Duration
		want        core.Price
	}{
		{3 * time.Hour, 24 * time.Hour, 100},
		{24 * time.Hour, 24 * time.Hour, 100}, // exactly 24h out is still the first step
		{24*time.Hour + time.Minute, 24 * time.Hour, 200},
		{47 * time.Hour, 24 * time.Hour, 200},
		{70 * time.Hour, 24 * time.Hour, 300},
		{-2 * time.Hour, 24 * time.Hour, 100}, // under way
		{70 * time.Hour, 0, 100},              // off
	}
	for _, c := range cases {
		if got := HorizonEdge(base, c.until, c.step); got != c.want {
			t.Errorf("HorizonEdge(%v, step %v) = %d; want %d", c.until, c.step, got, c.want)
		}
	}
}
