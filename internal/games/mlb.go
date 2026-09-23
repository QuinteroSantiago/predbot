// Package games records live game events — schedule and status, in-game
// state, every completed play, and MLB's own per-at-bat win probability — so
// price studies can tell a market reacting to news from a market overreacting.
// Records land beside the venue data as data/games/<league>-<hour>.jsonl.gz.
package games

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// MLB is a client for MLB's public Stats API (statsapi.mlb.com). It needs no
// key. Responses are trimmed with the API's fields parameter: a late-game
// play-by-play is ~440 KB whole and ~20 KB trimmed, which matters on WiFi.
type MLB struct {
	HTTP *http.Client
	Base string // default https://statsapi.mlb.com
}

// NewMLB returns a client with a 15s timeout.
func NewMLB() *MLB {
	return &MLB{HTTP: &http.Client{Timeout: 15 * time.Second}, Base: "https://statsapi.mlb.com"}
}

func (c *MLB) get(ctx context.Context, path string, q url.Values, v any) error {
	u := c.Base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "predbot-games (homelab research recorder)")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return fmt.Errorf("GET %s: HTTP %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// Game is one scheduled game.
type Game struct {
	Pk           int64     `json:"gamePk"`
	GameDate     time.Time `json:"gameDate"`     // scheduled first pitch, UTC
	OfficialDate string    `json:"officialDate"` // the game's local date, as in market tickers
	GameNumber   int       `json:"gameNumber"`   // 2 for the second game of a doubleheader
	DoubleHeader string    `json:"doubleHeader"` // "N", "Y" (traditional) or "S" (split)
	Status       struct {
		Abstract string `json:"abstractGameState"` // Preview, Live, Final
		Detailed string `json:"detailedState"`     // Scheduled, In Progress, Delayed, Final, Postponed…
	} `json:"status"`
	Teams struct {
		Away GameTeam `json:"away"`
		Home GameTeam `json:"home"`
	} `json:"teams"`
}

// GameTeam is one side of a scheduled game.
type GameTeam struct {
	Score int `json:"score"`
	Team  struct {
		ID           int    `json:"id"`
		Name         string `json:"name"`
		Abbreviation string `json:"abbreviation"`
	} `json:"team"`
}

// Schedule lists the games on a date (YYYY-MM-DD, the league's local date).
func (c *MLB) Schedule(ctx context.Context, date string) ([]Game, error) {
	var resp struct {
		Dates []struct {
			Games []Game `json:"games"`
		} `json:"dates"`
	}
	q := url.Values{"sportId": {"1"}, "date": {date}, "hydrate": {"team"}}
	if err := c.get(ctx, "/api/v1/schedule", q, &resp); err != nil {
		return nil, err
	}
	var out []Game
	for _, d := range resp.Dates {
		out = append(out, d.Games...)
	}
	return out, nil
}

// Linescore is a live game's current state.
type Linescore struct {
	CurrentInning int    `json:"currentInning"`
	InningHalf    string `json:"inningHalf"` // Top, Bottom
	InningState   string `json:"inningState"`
	Outs          int    `json:"outs"`
	Balls         int    `json:"balls"`
	Strikes       int    `json:"strikes"`
	Teams         struct {
		Away struct {
			Runs int `json:"runs"`
		} `json:"away"`
		Home struct {
			Runs int `json:"runs"`
		} `json:"home"`
	} `json:"teams"`
	Offense map[string]json.RawMessage `json:"offense"` // "first"/"second"/"third" present when occupied
}

// Bases returns the occupied bases as a compact string: "" (empty), "1", "13", "123"…
func (l Linescore) Bases() string {
	s := ""
	for i, b := range []string{"first", "second", "third"} {
		if _, ok := l.Offense[b]; ok {
			s += fmt.Sprint(i + 1)
		}
	}
	return s
}

// Linescore fetches a game's current state (~3 KB).
func (c *MLB) Linescore(ctx context.Context, pk int64) (Linescore, error) {
	var l Linescore
	err := c.get(ctx, fmt.Sprintf("/api/v1/game/%d/linescore", pk), nil, &l)
	return l, err
}

// Play is one plate appearance.
type Play struct {
	AtBatIndex int `json:"atBatIndex"`
	Result     struct {
		Event       string `json:"event"`
		EventType   string `json:"eventType"`
		Description string `json:"description"`
		RBI         int    `json:"rbi"`
		AwayScore   int    `json:"awayScore"`
		HomeScore   int    `json:"homeScore"`
	} `json:"result"`
	About struct {
		HalfInning    string    `json:"halfInning"`
		Inning        int       `json:"inning"`
		StartTime     time.Time `json:"startTime"`
		EndTime       time.Time `json:"endTime"`
		IsComplete    bool      `json:"isComplete"`
		IsScoringPlay bool      `json:"isScoringPlay"`
	} `json:"about"`
	Count struct {
		Outs int `json:"outs"`
	} `json:"count"`
}

const playFields = "allPlays,result,event,eventType,description,rbi,awayScore,homeScore," +
	"about,atBatIndex,halfInning,inning,startTime,endTime,isComplete,isScoringPlay,count,outs"

// Plays fetches a game's plate appearances so far, trimmed to what's recorded.
func (c *MLB) Plays(ctx context.Context, pk int64) ([]Play, error) {
	var resp struct {
		AllPlays []Play `json:"allPlays"`
	}
	err := c.get(ctx, fmt.Sprintf("/api/v1/game/%d/playByPlay", pk), url.Values{"fields": {playFields}}, &resp)
	return resp.AllPlays, err
}

// WinProb is MLB's modeled win probability after one plate appearance.
type WinProb struct {
	AtBatIndex int     `json:"atBatIndex"`
	HomeWin    float64 `json:"homeTeamWinProbability"` // percent
	HomeAdded  float64 `json:"homeTeamWinProbabilityAdded"`
	About      struct {
		EndTime time.Time `json:"endTime"`
	} `json:"about"`
}

// WinProbability fetches MLB's per-plate-appearance win probability (~11 KB).
func (c *MLB) WinProbability(ctx context.Context, pk int64) ([]WinProb, error) {
	var out []WinProb
	err := c.get(ctx, fmt.Sprintf("/api/v1/game/%d/winProbability", pk),
		url.Values{"fields": {"atBatIndex,homeTeamWinProbability,homeTeamWinProbabilityAdded,about,endTime"}}, &out)
	return out, err
}
