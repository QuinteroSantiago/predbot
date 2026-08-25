package backtest

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"predbot/internal/core"
)

// LoadReplay reads every normalized snapshot under dir/normalized and returns
// them merged and ordered by TsRecv (design §10.1), i.e. exactly the order the
// live pipeline saw them. Deterministic: no wall-clock. Loads into memory (the
// footprint is tens of MB/day compressed); a streaming merge is a TODO if the
// dataset outgrows RAM.
func LoadReplay(dir string) ([]core.BookSnapshot, error) { return LoadReplayDays(dir, 0) }

// LoadReplayDays loads only the most recent `days` of hourly files (0 = all).
// The full archive is far too large to hold in memory — a multi-GB replay both
// exhausts RAM and takes many minutes — so callers that only need a recent
// window (the dashboard, a quick check) should bound it.
func LoadReplayDays(dir string, days int) ([]core.BookSnapshot, error) {
	return LoadReplayHours(dir, days*24)
}

// LoadReplayHours loads only the most recent `hours` of data (0 = all).
// NOTE: the full archive does NOT fit in memory — a 1.3 GB replay was
// OOM-killed on the Pi. Streaming replay remains a TODO (design §10.1); until
// then, bound every load.
func LoadReplayHours(dir string, hours int) ([]core.BookSnapshot, error) {
	files, err := filepath.Glob(filepath.Join(dir, "normalized", "*.jsonl.gz"))
	if err != nil {
		return nil, err
	}
	files = withinHours(files, hours)
	var snaps []core.BookSnapshot
	for _, f := range files {
		// The current-hour file is an open, unfinalized gzip stream, so a
		// truncated read is expected: keep the complete lines and move on.
		s, err := readSnapshotFile(f)
		snaps = append(snaps, s...)
		if err != nil {
			slog.Warn("partial replay file (likely the live current-hour log)", "file", filepath.Base(f), "recovered", len(s), "err", err)
		}
	}
	if len(snaps) == 0 {
		// No normalized stream (the recorder may run with -no-normalized):
		// reconstruct from raw, which is the ground truth (design §7.3).
		slog.Info("no normalized snapshots found — reconstructing from raw")
		return LoadReplayFromRawHours(dir, hours)
	}
	sort.SliceStable(snaps, func(i, j int) bool { return snaps[i].TsRecv < snaps[j].TsRecv })
	return snaps, nil
}

func readSnapshotFile(path string) ([]core.BookSnapshot, error) {
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

	gz.Multistream(true)
	var out []core.BookSnapshot
	sc := bufio.NewScanner(gz)
	sc.Buffer(make([]byte, 0, 1<<20), 8<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var s core.BookSnapshot
		if err := json.Unmarshal(line, &s); err != nil {
			// A torn final line in the live file: stop, keep the rest.
			return out, err
		}
		out = append(out, s)
	}
	return out, sc.Err()
}

// withinDays filters hourly files (…-YYYY-MM-DDTHH.jsonl.gz) to the most recent
// `days`; days<=0 keeps everything.
func withinDays(files []string, days int) []string { return withinHours(files, days*24) }

// withinHours filters hourly files to the most recent `hours`; <=0 keeps all.
func withinHours(files []string, hours int) []string {
	if hours <= 0 {
		return files
	}
	cutoff := time.Now().UTC().Add(-time.Duration(hours) * time.Hour)
	var out []string
	for _, f := range files {
		// Files are "<venue>-<YYYY-MM-DDTHH>.jsonl.gz"; venue names contain no
		// hyphen, so split on the FIRST one (LastIndex would grab the day part).
		b := filepath.Base(f)
		_, rest, found := strings.Cut(b, "-")
		if !found {
			continue
		}
		stamp := strings.TrimSuffix(rest, ".jsonl.gz")
		t, err := time.Parse("2006-01-02T15", stamp)
		if err != nil || t.Before(cutoff) {
			continue
		}
		out = append(out, f)
	}
	return out
}
