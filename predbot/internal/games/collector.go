package games

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"predbot/internal/sports"
)

// Writer receives records; journal.Stream in production.
type Writer interface {
	Write(prefix string, line []byte, ts time.Time) error
}

// Collector polls MLB and writes four record types, one JSON object per line:
//
//	game     schedule and status, whenever a game's status or score changes
//	state    in-game state (inning, outs, count, runs, bases) when it changes
//	play     each completed plate appearance, once, with MLB's own timestamps
//	winprob  MLB's home win probability after each plate appearance, once
//
// Every record carries ts_recv (when we saw it, unix ns) and game_pk; game
// records also carry the canonical market ids of both teams, so studies can
// join events to prices. Join on the plays' end_time, not ts_recv: it's MLB's
// own clock, and polling adds up to a poll interval of lag.
type Collector struct {
	MLB *MLB
	Out Writer
	Log *slog.Logger
	Now func() time.Time

	// Prefix names the output files ("<prefix>-<hour>.jsonl.gz"). A backfill
	// uses its own, so it can run beside the live service without two
	// processes appending to one gzip file.
	Prefix string

	ScheduleEvery time.Duration // how often to re-read the schedule (status changes)
	PlaysEvery    time.Duration // refetch plays at least this often while live

	games        map[int64]*tracked
	lastSchedule time.Time
}

type tracked struct {
	g         Game
	statusKey string
	stateKey  string
	playsKey  string
	lastPlays time.Time
	seenPlay  map[int]bool
	seenWP    map[int]bool
	done      bool // final (or called off) and fully recorded
}

// NewCollector polls with the default cadence: schedule every 2 minutes, plays
// on any at-bat-level change or at least every minute while a game is live.
func NewCollector(mlb *MLB, out Writer, log *slog.Logger) *Collector {
	return &Collector{MLB: mlb, Out: out, Log: log, Now: time.Now, Prefix: "mlb",
		ScheduleEvery: 2 * time.Minute, PlaysEvery: time.Minute, games: map[int64]*tracked{}}
}

var eastern = mustLoc("America/New_York")

func mustLoc(name string) *time.Location {
	l, err := time.LoadLocation(name)
	if err != nil {
		return time.FixedZone("ET", -5*3600)
	}
	return l
}

// Tick does one polling round. Errors on one game are logged and don't stop
// the others; the next tick retries.
func (c *Collector) Tick(ctx context.Context) {
	now := c.Now()
	if now.Sub(c.lastSchedule) >= c.ScheduleEvery {
		// Late games run past midnight: keep yesterday's slate until it's final.
		et := now.In(eastern)
		for _, d := range []string{et.AddDate(0, 0, -1).Format("2006-01-02"), et.Format("2006-01-02")} {
			if err := c.readSchedule(ctx, d); err != nil {
				c.Log.Warn("schedule read failed", "date", d, "err", err)
				continue
			}
		}
		c.lastSchedule = now
	}
	for _, t := range c.games {
		if t.done || ctx.Err() != nil {
			continue
		}
		switch t.g.Status.Abstract {
		case "Live":
			c.pollLive(ctx, t)
		case "Final":
			// One last sweep catches the final plays and win probabilities;
			// a game already over when first seen is backfilled the same way.
			if err := c.syncPlays(ctx, t); err != nil {
				c.Log.Warn("final sync failed", "game_pk", t.g.Pk, "err", err)
				continue
			}
			t.done = true
		default:
			if calledOff(t.g.Status.Detailed) {
				t.done = true
			}
		}
	}
}

func calledOff(detailed string) bool {
	d := strings.ToLower(detailed)
	return strings.Contains(d, "postponed") || strings.Contains(d, "cancelled") || strings.Contains(d, "canceled")
}

func (c *Collector) readSchedule(ctx context.Context, date string) error {
	gs, err := c.MLB.Schedule(ctx, date)
	if err != nil {
		return err
	}
	for _, g := range gs {
		t := c.games[g.Pk]
		if t == nil {
			t = &tracked{seenPlay: map[int]bool{}, seenWP: map[int]bool{}}
			c.games[g.Pk] = t
		}
		t.g = g
		key := fmt.Sprintf("%s|%s|%d|%d", g.Status.Abstract, g.Status.Detailed, g.Teams.Away.Score, g.Teams.Home.Score)
		if key != t.statusKey {
			t.statusKey = key
			c.write(gameRecord(g))
		}
	}
	return nil
}

func (c *Collector) pollLive(ctx context.Context, t *tracked) {
	l, err := c.MLB.Linescore(ctx, t.g.Pk)
	if err != nil {
		c.Log.Warn("linescore failed", "game_pk", t.g.Pk, "err", err)
		return
	}
	state := fmt.Sprintf("%d|%s|%d|%d|%d|%d|%d|%s", l.CurrentInning, l.InningHalf, l.Outs, l.Balls, l.Strikes,
		l.Teams.Away.Runs, l.Teams.Home.Runs, l.Bases())
	if state != t.stateKey {
		t.stateKey = state
		c.write(map[string]any{
			"type": "state", "league": "mlb", "game_pk": t.g.Pk,
			"inning": l.CurrentInning, "half": strings.ToLower(l.InningHalf), "inning_state": l.InningState,
			"outs": l.Outs, "balls": l.Balls, "strikes": l.Strikes,
			"away_runs": l.Teams.Away.Runs, "home_runs": l.Teams.Home.Runs, "bases": l.Bases(),
		})
	}
	// Plays only change at the plate-appearance level; a new pitch alone doesn't
	// need a refetch.
	plays := fmt.Sprintf("%d|%s|%d|%d|%d|%s", l.CurrentInning, l.InningHalf, l.Outs, l.Teams.Away.Runs, l.Teams.Home.Runs, l.Bases())
	if plays != t.playsKey || c.Now().Sub(t.lastPlays) >= c.PlaysEvery {
		t.playsKey = plays
		if err := c.syncPlays(ctx, t); err != nil {
			c.Log.Warn("plays sync failed", "game_pk", t.g.Pk, "err", err)
		}
	}
}

// syncPlays writes plate appearances and win probabilities not yet recorded.
func (c *Collector) syncPlays(ctx context.Context, t *tracked) error {
	t.lastPlays = c.Now()
	ps, err := c.MLB.Plays(ctx, t.g.Pk)
	if err != nil {
		return err
	}
	fresh := 0
	for _, p := range ps {
		if !p.About.IsComplete || t.seenPlay[p.AtBatIndex] {
			continue
		}
		t.seenPlay[p.AtBatIndex] = true
		fresh++
		c.write(map[string]any{
			"type": "play", "league": "mlb", "game_pk": t.g.Pk, "at_bat_index": p.AtBatIndex,
			"inning": p.About.Inning, "half": p.About.HalfInning,
			"event": p.Result.Event, "event_type": p.Result.EventType, "description": p.Result.Description,
			"rbi": p.Result.RBI, "away_score": p.Result.AwayScore, "home_score": p.Result.HomeScore,
			"scoring": p.About.IsScoringPlay, "outs": p.Count.Outs,
			"start_time": p.About.StartTime, "end_time": p.About.EndTime,
		})
	}
	if fresh == 0 {
		return nil
	}
	wps, err := c.MLB.WinProbability(ctx, t.g.Pk)
	if err != nil {
		return fmt.Errorf("win probability: %w", err)
	}
	for _, w := range wps {
		if t.seenWP[w.AtBatIndex] || !t.seenPlay[w.AtBatIndex] {
			continue // only for plays already recorded complete
		}
		t.seenWP[w.AtBatIndex] = true
		c.write(map[string]any{
			"type": "winprob", "league": "mlb", "game_pk": t.g.Pk, "at_bat_index": w.AtBatIndex,
			"home_win_prob": w.HomeWin, "home_wp_added": w.HomeAdded, "end_time": w.About.EndTime,
		})
	}
	return nil
}

// Resume reads records already written under dir (the games/ directory) so a
// restart, or a repeated backfill, doesn't write any play, win probability or
// game status twice. Unreadable tails (a file cut off by a crash) are skipped.
func (c *Collector) Resume(dir string) (int, error) {
	files, err := filepath.Glob(filepath.Join(dir, "mlb-*.jsonl.gz"))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, f := range files {
		n += c.resumeFile(f)
	}
	return n, nil
}

func (c *Collector) resumeFile(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	gz, err := gzip.NewReader(f) // multistream: appended restarts read as one
	if err != nil {
		return 0
	}
	defer gz.Close()
	sc := bufio.NewScanner(gz)
	sc.Buffer(make([]byte, 0, 1<<16), 1<<20)
	n := 0
	for sc.Scan() {
		if c.resumeLine(sc.Bytes()) {
			n++
		}
	}
	return n
}

// resumeLine marks one already-written record as seen; false if it isn't one.
func (c *Collector) resumeLine(line []byte) bool {
	var r struct {
		Type       string `json:"type"`
		GamePk     int64  `json:"game_pk"`
		AtBatIndex int    `json:"at_bat_index"`
		State      string `json:"state"`
		Detailed   string `json:"detailed"`
		AwayScore  int    `json:"away_score"`
		HomeScore  int    `json:"home_score"`
	}
	if json.Unmarshal(line, &r) != nil || r.GamePk == 0 {
		return false
	}
	t := c.games[r.GamePk]
	if t == nil {
		t = &tracked{seenPlay: map[int]bool{}, seenWP: map[int]bool{}}
		c.games[r.GamePk] = t
	}
	switch r.Type {
	case "play":
		t.seenPlay[r.AtBatIndex] = true
	case "winprob":
		t.seenWP[r.AtBatIndex] = true
	case "game":
		t.statusKey = fmt.Sprintf("%s|%s|%d|%d", r.State, r.Detailed, r.AwayScore, r.HomeScore)
	}
	return true
}

// Backfill records the finished games of past dates (YYYY-MM-DD, inclusive):
// status, every play and every win probability. ts_recv is the backfill time,
// so join on the plays' end_time.
func (c *Collector) Backfill(ctx context.Context, from, to string) error {
	start, err := time.Parse("2006-01-02", from)
	if err != nil {
		return err
	}
	end, err := time.Parse("2006-01-02", to)
	if err != nil {
		return err
	}
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		if err := ctx.Err(); err != nil {
			return err
		}
		date := d.Format("2006-01-02")
		if err := c.readSchedule(ctx, date); err != nil {
			return fmt.Errorf("%s: %w", date, err)
		}
		n := 0
		for _, t := range c.games {
			if t.done || t.g.OfficialDate != date || t.g.Status.Abstract != "Final" {
				continue
			}
			if err := c.syncPlays(ctx, t); err != nil {
				c.Log.Warn("backfill game failed", "game_pk", t.g.Pk, "err", err)
				continue
			}
			t.done = true
			n++
		}
		c.Log.Info("backfilled", "date", date, "games", n)
	}
	return nil
}

func (c *Collector) write(rec map[string]any) {
	now := c.Now()
	rec["ts_recv"] = now.UnixNano()
	b, err := json.Marshal(rec)
	if err != nil {
		c.Log.Error("record encode failed", "err", err)
		return
	}
	if err := c.Out.Write(c.Prefix, b, now); err != nil {
		c.Log.Error("record write failed", "err", err)
	}
}

// Canonical is the market id for a team's win on a date, as the sports
// discovery builds it: MLB-<YYYYMMDD>-<TEAM>. "" if the team code is unknown.
func Canonical(officialDate, abbr string) string {
	team := sports.CanonTeam("mlb", abbr)
	if team == "" {
		return ""
	}
	return "MLB-" + strings.ReplaceAll(officialDate, "-", "") + "-" + team
}

func gameRecord(g Game) map[string]any {
	return map[string]any{
		"type": "game", "league": "mlb", "game_pk": g.Pk,
		"official_date": g.OfficialDate, "start": g.GameDate,
		"state": g.Status.Abstract, "detailed": g.Status.Detailed,
		"game_number": g.GameNumber, "double_header": g.DoubleHeader,
		"away": g.Teams.Away.Team.Abbreviation, "home": g.Teams.Home.Team.Abbreviation,
		"away_name": g.Teams.Away.Team.Name, "home_name": g.Teams.Home.Team.Name,
		"away_score": g.Teams.Away.Score, "home_score": g.Teams.Home.Score,
		"canonical_away": Canonical(g.OfficialDate, g.Teams.Away.Team.Abbreviation),
		"canonical_home": Canonical(g.OfficialDate, g.Teams.Home.Team.Abbreviation),
	}
}
