package main

import (
	"testing"
	"time"

	"etfbot/internal/market"
)

func TestLastOfMonth(t *testing.T) {
	d := market.Date
	for _, c := range []struct {
		today time.Time
		days  []time.Time
		want  bool
	}{
		{d(2026, 9, 30), []time.Time{d(2026, 9, 30), d(2026, 10, 1)}, true},
		{d(2026, 9, 29), []time.Time{d(2026, 9, 29), d(2026, 9, 30)}, false},
		{d(2026, 10, 30), []time.Time{d(2026, 10, 30), d(2026, 11, 2)}, true}, // Friday before a weekend month end
		{d(2026, 10, 31), []time.Time{d(2026, 11, 2)}, false},                 // not a trading day
	} {
		if got := lastOfMonth(c.today, c.days); got != c.want {
			t.Errorf("lastOfMonth(%s) = %v; want %v", c.today.Format(time.DateOnly), got, c.want)
		}
	}
}
