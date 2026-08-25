package backtest

import (
	"bufio"
	"compress/gzip"
	"container/heap"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"predbot/internal/core"
	"predbot/internal/venue/kalshi"
	"predbot/internal/venue/polymarketus"
)

// StreamReplay walks every recorded snapshot in receive-time order WITHOUT
// holding the archive in memory, calling fn for each one.
//
// Why this exists: LoadReplay* build one giant slice. A 1.3 GB gzipped archive
// expands to many GB of parsed structs and was OOM-killed on the Pi, which caps
// backtests to a few hours of data — useless for the multi-month histories that
// directional/statistical strategies need.
//
// How it stays bounded: recordings are hourly files, and each file is already in
// receive-time order. So we group files by hour, walk hours chronologically, and
// inside each hour do a k-way merge across that hour's venue files holding just
// ONE snapshot per file. Peak memory is O(open files), not O(archive).
//
// hours <= 0 walks the entire archive — which is now safe.
func StreamReplay(dir string, hours int, fn func(core.BookSnapshot) error) error {
	byHour, err := hourlyFiles(dir, hours)
	if err != nil {
		return err
	}
	for _, hour := range sortedHours(byHour) {
		if err := streamHour(byHour[hour], fn); err != nil {
			return err
		}
	}
	return nil
}

// hourlyFiles returns files grouped by their hour stamp, preferring the
// normalized stream and falling back to raw (the recorder may run with
// -no-normalized, and raw is the ground truth either way).
func hourlyFiles(dir string, hours int) (map[string][]string, error) {
	// Filter to the window FIRST, then decide whether to fall back to raw.
	// Checking "are there normalized files?" before filtering is wrong: with
	// -no-normalized the normalized files still exist but are all stale, so the
	// window empties them and raw is never consulted.
	files, err := filepath.Glob(filepath.Join(dir, "normalized", "*.jsonl.gz"))
	if err != nil {
		return nil, err
	}
	files = withinHours(files, hours)
	if len(files) == 0 {
		rawFiles, err := filepath.Glob(filepath.Join(dir, "raw", "*.jsonl.gz"))
		if err != nil {
			return nil, err
		}
		files = withinHours(rawFiles, hours)
	}

	out := map[string][]string{}
	for _, f := range files {
		b := filepath.Base(f)
		_, rest, ok := strings.Cut(b, "-")
		if !ok {
			continue
		}
		out[strings.TrimSuffix(rest, ".jsonl.gz")] = append(out[strings.TrimSuffix(rest, ".jsonl.gz")], f)
	}
	return out, nil
}

func sortedHours(m map[string][]string) []string {
	hs := make([]string, 0, len(m))
	for h := range m {
		hs = append(hs, h)
	}
	sort.Strings(hs) // YYYY-MM-DDTHH sorts chronologically as a string
	return hs
}

// snapReader yields snapshots from one file, one at a time.
type snapReader struct {
	f     *os.File
	gz    *gzip.Reader
	sc    *bufio.Scanner
	raw   bool
	kalsh *kalshi.State // per-file Kalshi book state (stateful deltas)
	cur   core.BookSnapshot
	ok    bool
}

func openSnapReader(path string) (*snapReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	sc := bufio.NewScanner(gz)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	r := &snapReader{f: f, gz: gz, sc: sc,
		raw:   strings.Contains(path, string(os.PathSeparator)+"raw"+string(os.PathSeparator)),
		kalsh: kalshi.NewState()}
	r.advance()
	return r, nil
}

func (r *snapReader) close() {
	r.gz.Close()
	r.f.Close()
}

// advance loads the next snapshot, setting ok=false at end of file.
func (r *snapReader) advance() {
	for r.sc.Scan() {
		line := r.sc.Bytes()
		if len(line) == 0 {
			continue
		}
		if !r.raw {
			var s core.BookSnapshot
			if json.Unmarshal(line, &s) != nil {
				continue
			}
			r.cur, r.ok = s, true
			return
		}
		var rl rawLine
		if json.Unmarshal(line, &rl) != nil {
			continue
		}
		now := time.Unix(0, rl.TsRecvNs)
		switch rl.Venue {
		case "kalshi":
			s, emit, err := r.kalsh.ProcessFrame(rl.Frame, now)
			if err != nil || !emit {
				continue
			}
			r.cur, r.ok = s, true
			return
		case "polymarketus":
			s, okp, err := polymarketus.ParseMarketData(rl.Frame, now)
			if err != nil || !okp {
				continue
			}
			r.cur, r.ok = s, true
			return
		}
	}
	r.ok = false
}

// readerHeap orders open readers by their current snapshot's receive time.
type readerHeap []*snapReader

func (h readerHeap) Len() int            { return len(h) }
func (h readerHeap) Less(i, j int) bool  { return h[i].cur.TsRecv < h[j].cur.TsRecv }
func (h readerHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *readerHeap) Push(x any)         { *h = append(*h, x.(*snapReader)) }
func (h *readerHeap) Pop() any {
	old := *h
	n := len(old)
	it := old[n-1]
	*h = old[:n-1]
	return it
}

// reorderWindow is how many snapshots to buffer before emitting. Files are only
// APPROXIMATELY sorted internally: the recorder's two ingesters feed one channel,
// so write order can differ from receive order by microseconds. A small buffer
// makes the merged output exactly ordered while staying O(window) in memory.
const reorderWindow = 8192

// snapHeap is a min-heap of snapshots by receive time (the reorder buffer).
type snapHeap []core.BookSnapshot

func (h snapHeap) Len() int           { return len(h) }
func (h snapHeap) Less(i, j int) bool { return h[i].TsRecv < h[j].TsRecv }
func (h snapHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *snapHeap) Push(x any)        { *h = append(*h, x.(core.BookSnapshot)) }
func (h *snapHeap) Pop() any {
	old := *h
	n := len(old)
	it := old[n-1]
	*h = old[:n-1]
	return it
}

// streamHour merges one hour's files in receive-time order, buffering a single
// snapshot per file plus a small reorder window.
func streamHour(files []string, fn func(core.BookSnapshot) error) error {
	h := &readerHeap{}
	defer func() {
		for _, r := range *h {
			r.close()
		}
	}()
	for _, f := range files {
		r, err := openSnapReader(f)
		if err != nil {
			continue // a torn current-hour file is expected
		}
		if !r.ok {
			r.close()
			continue
		}
		*h = append(*h, r)
	}
	heap.Init(h)
	buf := &snapHeap{}
	for h.Len() > 0 {
		r := (*h)[0]
		heap.Push(buf, r.cur)
		if buf.Len() > reorderWindow {
			if err := fn(heap.Pop(buf).(core.BookSnapshot)); err != nil {
				return err
			}
		}
		r.advance()
		if r.ok {
			heap.Fix(h, 0)
		} else {
			heap.Pop(h)
			r.close()
		}
	}
	for buf.Len() > 0 { // drain
		if err := fn(heap.Pop(buf).(core.BookSnapshot)); err != nil {
			return err
		}
	}
	return nil
}
