package games

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// MLB's Stats API spells a few teams differently from the market tickers.
func TestCanonical(t *testing.T) {
	for abbr, want := range map[string]string{
		"AZ": "MLB-20260922-ARI", "ATH": "MLB-20260922-OAK", "CWS": "MLB-20260922-CHW",
		"WSH": "MLB-20260922-WSH", "NYY": "MLB-20260922-NYY", "XYZ": "",
	} {
		if got := Canonical("2026-09-22", abbr); got != want {
			t.Errorf("Canonical(%q) = %q, want %q", abbr, got, want)
		}
	}
}

type memWriter struct {
	mu    sync.Mutex
	lines []map[string]any
}

func (w *memWriter) Write(_ string, line []byte, _ time.Time) error {
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		return err
	}
	w.mu.Lock()
	w.lines = append(w.lines, m)
	w.mu.Unlock()
	return nil
}

func (w *memWriter) count(typ string) int {
	n := 0
	for _, l := range w.lines {
		if l["type"] == typ {
			n++
		}
	}
	return n
}

// fakeMLB serves one final game with two complete plays and one in progress.
func fakeMLB(t *testing.T) *MLB {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/schedule"):
			io.WriteString(w, `{"dates":[{"games":[{"gamePk":7,"gameDate":"2026-08-22T23:05:00Z","officialDate":"2026-08-22",
				"gameNumber":1,"doubleHeader":"N","status":{"abstractGameState":"Final","detailedState":"Final"},
				"teams":{"away":{"score":2,"team":{"id":1,"name":"Arizona Diamondbacks","abbreviation":"AZ"}},
				         "home":{"score":3,"team":{"id":2,"name":"New York Yankees","abbreviation":"NYY"}}}}]}]}`)
		case strings.HasSuffix(r.URL.Path, "/playByPlay"):
			io.WriteString(w, `{"allPlays":[
				{"atBatIndex":0,"result":{"event":"Single"},"about":{"isComplete":true,"inning":1,"halfInning":"top","endTime":"2026-08-22T23:07:00Z"}},
				{"atBatIndex":1,"result":{"event":"Home Run","rbi":2,"awayScore":2},"about":{"isComplete":true,"isScoringPlay":true,"inning":1,"halfInning":"top","endTime":"2026-08-22T23:09:00Z"}},
				{"atBatIndex":2,"about":{"isComplete":false}}]}`)
		case strings.HasSuffix(r.URL.Path, "/winProbability"):
			io.WriteString(w, `[{"atBatIndex":0,"homeTeamWinProbability":52.1},{"atBatIndex":1,"homeTeamWinProbability":38.4},{"atBatIndex":2,"homeTeamWinProbability":39}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &MLB{HTTP: srv.Client(), Base: srv.URL}
}

func TestBackfillWritesOnceAndResumes(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	w := &memWriter{}
	c := NewCollector(fakeMLB(t), w, log)
	if err := c.Backfill(context.Background(), "2026-08-22", "2026-08-22"); err != nil {
		t.Fatal(err)
	}
	// Only complete plays, and win probabilities only for those.
	if g, p, wp := w.count("game"), w.count("play"), w.count("winprob"); g != 1 || p != 2 || wp != 2 {
		t.Fatalf("game/play/winprob = %d/%d/%d, want 1/2/2", g, p, wp)
	}
	var game map[string]any
	for _, l := range w.lines {
		if l["type"] == "game" {
			game = l
		}
	}
	if game["canonical_away"] != "MLB-20260822-ARI" || game["canonical_home"] != "MLB-20260822-NYY" {
		t.Errorf("canonicals = %v / %v", game["canonical_away"], game["canonical_home"])
	}

	// A second collector that resumes from the first one's records writes nothing new.
	w2 := &memWriter{}
	c2 := NewCollector(fakeMLB(t), w2, log)
	for _, l := range w.lines {
		b, _ := json.Marshal(l)
		c2.resumeLine(b)
	}
	if err := c2.Backfill(context.Background(), "2026-08-22", "2026-08-22"); err != nil {
		t.Fatal(err)
	}
	if len(w2.lines) != 0 {
		t.Fatalf("resumed backfill wrote %d records, want 0", len(w2.lines))
	}
}
