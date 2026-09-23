package backtest

import (
	"path/filepath"
	"strings"
	"time"
)

// Window bounds a replay: the most recent Hours (0 = no limit) and/or an
// absolute [From, To) range in receive time (a zero bound = open). Both apply
// when both are set. Hourly files outside the range are skipped unread;
// snapshots in the boundary hours are filtered one by one.
type Window struct {
	Hours    int
	From, To time.Time
}

// Ranged reports whether w has an absolute bound.
func (w Window) Ranged() bool { return !w.From.IsZero() || !w.To.IsZero() }

// contains reports whether a receive time (unix ns) falls inside the range.
func (w Window) contains(ts int64) bool {
	if !w.From.IsZero() && ts < w.From.UnixNano() {
		return false
	}
	if !w.To.IsZero() && ts >= w.To.UnixNano() {
		return false
	}
	return true
}

// overlapsHour reports whether the hour starting at t overlaps the range.
func (w Window) overlapsHour(t time.Time) bool {
	if !w.From.IsZero() && !t.Add(time.Hour).After(w.From) {
		return false
	}
	if !w.To.IsZero() && !t.Before(w.To) {
		return false
	}
	return true
}

// withinWindow keeps the "<prefix>-<YYYY-MM-DDTHH>.jsonl.gz" files that w
// covers. A name whose stamp doesn't parse is kept only for unranged windows,
// matching withinHours.
func withinWindow(files []string, w Window) []string {
	files = withinHours(files, w.Hours)
	if !w.Ranged() {
		return files
	}
	var out []string
	for _, f := range files {
		// The stamp is always the last 13 characters: book files are
		// "<venue>-<stamp>", trade files "<venue>-trades-<stamp>".
		base := strings.TrimSuffix(filepath.Base(f), ".jsonl.gz")
		if len(base) < 13 {
			continue
		}
		t, err := time.Parse("2006-01-02T15", base[len(base)-13:])
		if err != nil || !w.overlapsHour(t) {
			continue
		}
		out = append(out, f)
	}
	return out
}
