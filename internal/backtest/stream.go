package backtest

import (
	"bufio"
	"compress/gzip"
	"container/heap"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
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
//
// Decoding runs in parallel. Gunzip + JSON parse is essentially all of the
// replay's CPU time (a 14 GB window is ~85 min per pass single-threaded), and
// each hour is self-contained: readers hold per-file Kalshi delta state, so no
// hour depends on another. So hours are decoded on GOMAXPROCS goroutines while
// the consumer hands snapshots to fn from ONE goroutine, in the original order.
// fn therefore needs no locking and sees byte-identical output to the serial
// walk. Override the worker count with PREDBOT_REPLAY_WORKERS (1 = serial).
func StreamReplay(dir string, hours int, fn func(core.BookSnapshot) error) error {
	byHour, err := hourlyFiles(dir, hours)
	if err != nil {
		return err
	}
	return streamHours(byHour, sortedHours(byHour), replayWorkers(), fn)
}

// errCanceled unwinds a decode goroutine whose output is no longer wanted. It
// never escapes streamHours.
var errCanceled = errors.New("replay canceled")

const (
	// batchSize is how many snapshots cross a channel at once. Per-snapshot
	// channel sends cost more than the decode they overlap with.
	batchSize = 1024
	// batchesAhead is the per-hour send buffer, and maxWorkers caps the total.
	// In-flight memory is bounded by workers · (reorderWindow + batchesAhead ·
	// batchSize) snapshots — the point of streaming is that it stays O(1) in
	// archive size, and that must survive going parallel.
	batchesAhead = 2
	maxWorkers   = 16
)

// replayWorkers is the decode concurrency: GOMAXPROCS, capped, env-overridable.
func replayWorkers() int {
	n := runtime.GOMAXPROCS(0)
	if v, err := strconv.Atoi(os.Getenv("PREDBOT_REPLAY_WORKERS")); err == nil && v > 0 {
		n = v
	}
	if n > maxWorkers {
		n = maxWorkers
	}
	if n < 1 {
		n = 1
	}
	return n
}

// hourBatch is one hour's decoded snapshots, or the error that stopped it.
type hourBatch struct {
	snaps []core.BookSnapshot
	err   error
}

// streamHours walks hours in order, decoding up to workers of them ahead of the
// consumer. With workers == 1 it is the plain serial walk.
func streamHours(byHour map[string][]string, hours []string, workers int, fn func(core.BookSnapshot) error) error {
	if workers > len(hours) {
		workers = len(hours)
	}
	if workers <= 1 {
		for _, hour := range hours {
			if err := streamHour(byHour[hour], fn); err != nil {
				return err
			}
		}
		return nil
	}

	// done stops the launcher and every in-flight decoder on early return, so
	// an fn error (or a decode error) leaks neither goroutines nor open files.
	done := make(chan struct{})
	defer close(done)

	// queue carries one channel per hour, in order. Its capacity is what keeps
	// the launcher — and therefore the number of live decoders — bounded.
	queue := make(chan chan hourBatch, workers)
	go func() {
		defer close(queue)
		for _, hour := range hours {
			out := make(chan hourBatch, batchesAhead)
			select {
			case queue <- out:
			case <-done:
				return
			}
			go decodeHour(byHour[hour], out, done)
		}
	}()

	for out := range queue {
		for b := range out {
			if b.err != nil {
				return b.err
			}
			for _, s := range b.snaps {
				if err := fn(s); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// decodeHour merges one hour and ships it in batches. It owns out and closes it.
func decodeHour(files []string, out chan<- hourBatch, done <-chan struct{}) {
	defer close(out)
	batch := make([]core.BookSnapshot, 0, batchSize)
	// A fresh slice per send: the consumer owns what it receives.
	flush := func() bool {
		if len(batch) == 0 {
			return true
		}
		select {
		case out <- hourBatch{snaps: batch}:
			batch = make([]core.BookSnapshot, 0, batchSize)
			return true
		case <-done:
			return false
		}
	}
	err := streamHour(files, func(s core.BookSnapshot) error {
		batch = append(batch, s)
		if len(batch) >= batchSize && !flush() {
			return errCanceled
		}
		return nil
	})
	switch {
	case errors.Is(err, errCanceled):
		return
	case err != nil:
		select {
		case out <- hourBatch{err: err}:
		case <-done:
		}
	default:
		flush()
	}
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

	// Parallel parse of a normalized file (see startParse). nil for raw files
	// and for serial parsing.
	parsed []chan []core.BookSnapshot // per worker, results in batch order
	next   int                        // worker holding the next batch
	batch  []core.BookSnapshot
	pos    int
	stop   chan struct{}
	wg     sync.WaitGroup
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
	// Raw files carry stateful Kalshi deltas, so only normalized lines, which
	// parse independently, go to the worker pool.
	if w := parseWorkers(); !r.raw && w > 1 {
		r.startParse(w)
	}
	r.advance()
	return r, nil
}

func (r *snapReader) close() {
	if r.stop != nil {
		close(r.stop)
		r.wg.Wait() // the pipeline reads the file; let it exit before closing
	}
	r.gz.Close()
	r.f.Close()
}

// parseBatch is how many lines a parse worker takes at once.
const parseBatch = 256

// parseWorkers is how many goroutines parse each normalized file: GOMAXPROCS,
// or PREDBOT_PARSE_WORKERS (1 = parse inline, the old serial path).
func parseWorkers() int {
	n := runtime.GOMAXPROCS(0)
	if v, err := strconv.Atoi(os.Getenv("PREDBOT_PARSE_WORKERS")); err == nil && v > 0 {
		n = v
	}
	return n
}

// startParse moves JSON decoding, which is most of a replay's CPU, off the
// consumer. One goroutine scans lines (gunzip + split) and deals batches to
// workers round-robin; advance collects results from the workers in that same
// order, so snapshots come out exactly as the serial parse yields them. Each
// worker holds at most a batch in, one in progress and one out, so memory stays
// O(workers) per open file.
//
// The earlier hour-level parallelism in streamHours can't do this on its own:
// a look-ahead hour may only run batchesAhead batches in front of the consumer,
// so in practice one hour decodes at a time.
func (r *snapReader) startParse(workers int) {
	r.stop = make(chan struct{})
	lines := make([]chan [][]byte, workers)
	r.parsed = make([]chan []core.BookSnapshot, workers)
	for i := range lines {
		lines[i] = make(chan [][]byte, 1)
		r.parsed[i] = make(chan []core.BookSnapshot, 1)
	}
	r.wg.Add(1 + workers)
	go func() {
		defer r.wg.Done()
		defer func() {
			for _, in := range lines {
				close(in)
			}
		}()
		w := 0
		batch := make([][]byte, 0, parseBatch)
		send := func() bool {
			select {
			case lines[w] <- batch:
			case <-r.stop:
				return false
			}
			w = (w + 1) % workers
			batch = make([][]byte, 0, parseBatch)
			return true
		}
		for r.sc.Scan() {
			line := r.sc.Bytes()
			if len(line) == 0 {
				continue
			}
			// The scanner reuses its buffer; the worker needs its own copy.
			batch = append(batch, append([]byte(nil), line...))
			if len(batch) == parseBatch && !send() {
				return
			}
		}
		if len(batch) > 0 {
			send()
		}
	}()
	for i := range lines {
		go func(in <-chan [][]byte, out chan<- []core.BookSnapshot) {
			defer r.wg.Done()
			defer close(out)
			for batch := range in {
				snaps := make([]core.BookSnapshot, 0, len(batch))
				for _, line := range batch {
					var s core.BookSnapshot
					if json.Unmarshal(line, &s) == nil {
						snaps = append(snaps, s)
					}
				}
				select {
				case out <- snaps:
				case <-r.stop:
					return
				}
			}
		}(lines[i], r.parsed[i])
	}
}

// advance loads the next snapshot, setting ok=false at end of file.
func (r *snapReader) advance() {
	if r.parsed != nil {
		for r.pos >= len(r.batch) {
			// Batches were dealt round-robin, so if the worker owed the next one
			// has closed, there are no more batches anywhere.
			b, more := <-r.parsed[r.next]
			if !more {
				r.ok = false
				return
			}
			r.next = (r.next + 1) % len(r.parsed)
			r.batch, r.pos = b, 0
		}
		r.cur, r.ok = r.batch[r.pos], true
		r.pos++
		return
	}
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
