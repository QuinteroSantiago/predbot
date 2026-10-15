package sports

import (
	"testing"
	"time"
)

func TestPreGame(t *testing.T) {
	start := time.Date(2026, 9, 29, 23, 0, 0, 0, time.UTC)
	s := &Supervisor{
		starts: map[string]time.Time{"NHL-20260929-MTL": start, "NHL-20260929-BOS": start},
		states: map[string]GameState{"NHL-20260929-BOS": {Live: true, Period: "P1"}},
	}
	before, after := start.Add(-time.Minute), start.Add(time.Minute)
	for _, c := range []struct {
		canonical string
		now       time.Time
		want      bool
	}{
		{"NHL-20260929-MTL", before, true},
		{"NHL-20260929-MTL", start, false},
		{"NHL-20260929-MTL", after, false},
		{"NHL-20260929-BOS", before, false}, // started early per Poly's feed
		{"NHL-20260929-TOR", before, false}, // unknown start
	} {
		if got := s.PreGame(c.canonical, c.now); got != c.want {
			t.Errorf("PreGame(%s, %s) = %v; want %v", c.canonical, c.now.Format("15:04"), got, c.want)
		}
	}
}
