package backtest

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"

	"predbot/internal/core"
	"predbot/internal/venue/kalshi"
	"predbot/internal/venue/polymarketus"
)

// rawLine mirrors journal's raw record.
type rawLine struct {
	TsRecvNs int64           `json:"ts_recv_ns"`
	Venue    string          `json:"venue"`
	Frame    json.RawMessage `json:"frame"`
}

// LoadReplayFromRaw reconstructs normalized snapshots directly from the RAW
// stream, which is the ground truth (design §7.3: "raw frames are the ground
// truth; everything else can be regenerated"). This lets the recorder stop
// writing the normalized stream (saving ~35% disk) without losing any ability
// to backtest.
//
// Kalshi frames are stateful (snapshot + sequenced deltas), so each Kalshi file
// is replayed in order through a fresh kalshi.State — exactly as the live
// ingester does. Polymarket frames are self-contained snapshots.
func LoadReplayFromRaw(dir string) ([]core.BookSnapshot, error) {
	return LoadReplayFromRawDays(dir, 0)
}

// LoadReplayFromRawDays reconstructs from raw, bounded to the most recent days.
func LoadReplayFromRawDays(dir string, days int) ([]core.BookSnapshot, error) {
	return LoadReplayFromRawHours(dir, days*24)
}

// LoadReplayFromRawHours reconstructs from raw, bounded to recent hours.
func LoadReplayFromRawHours(dir string, hours int) ([]core.BookSnapshot, error) {
	files, err := filepath.Glob(filepath.Join(dir, "raw", "*.jsonl.gz"))
	if err != nil {
		return nil, err
	}
	files = withinHours(files, hours)
	sort.Strings(files) // chronological within a venue, so Kalshi seq applies in order

	var out []core.BookSnapshot
	for _, f := range files {
		snaps, err := replayRawFile(f)
		out = append(out, snaps...)
		if err != nil {
			// A truncated final file (the live current hour) is expected.
			continue
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TsRecv < out[j].TsRecv })
	return out, nil
}

func replayRawFile(path string) ([]core.BookSnapshot, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gz.Close()

	kState := kalshi.NewState() // per-file Kalshi book state
	var out []core.BookSnapshot
	sc := bufio.NewScanner(gz)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rl rawLine
		if json.Unmarshal(line, &rl) != nil {
			continue
		}
		now := time.Unix(0, rl.TsRecvNs)
		switch rl.Venue {
		case "kalshi":
			snap, emit, err := kState.ProcessFrame(rl.Frame, now)
			if err != nil || !emit {
				continue // gaps/non-book frames: skip, same as live
			}
			out = append(out, snap)
		case "polymarketus":
			snap, ok, err := polymarketus.ParseMarketData(rl.Frame, now)
			if err != nil || !ok {
				continue
			}
			out = append(out, snap)
		}
	}
	return out, sc.Err()
}
