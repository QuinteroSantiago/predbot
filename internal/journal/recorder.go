// Package journal records the feed to disk (design §7.3). Two streams per venue:
//
//   - raw:        the ground truth. One gzipped JSONL file per venue per hour,
//     line = {ts_recv_ns, venue, frame}. Everything else can be regenerated
//     from these frames.
//   - normalized: convenience for the backtester. line = core.BookSnapshot.
//
// Files rotate hourly; a rotate fsyncs and closes the old file. Close() (wired
// to SIGHUP/shutdown in cmd/record) closes everything cleanly.
package journal

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"predbot/internal/core"
)

// rawLine is one line of a raw file.
type rawLine struct {
	TsRecvNs int64           `json:"ts_recv_ns"`
	Venue    string          `json:"venue"`
	Frame    json.RawMessage `json:"frame"`
}

// Recorder writes raw and normalized streams with hourly rotation. Safe for
// concurrent use: both ingesters share one Recorder.
type Recorder struct {
	dir string

	mu     sync.Mutex
	raw    map[string]*rotFile // keyed by venue
	norm   map[string]*rotFile
	trades map[string]*rotFile
}

// New returns a Recorder writing under dir/{raw,normalized}. It creates the
// directories eagerly so a permissions problem fails at startup, not mid-feed.
func New(dir string) (*Recorder, error) {
	for _, sub := range []string{"raw", "normalized", "trades"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o750); err != nil {
			return nil, err
		}
	}
	return &Recorder{
		dir:  dir,
		raw:    map[string]*rotFile{},
		norm:   map[string]*rotFile{},
		trades: map[string]*rotFile{},
	}, nil
}

// WriteRaw persists one raw venue frame verbatim. frame must be valid JSON.
func (r *Recorder) WriteRaw(venue string, frame []byte, tsRecv time.Time) error {
	line, err := json.Marshal(rawLine{
		TsRecvNs: tsRecv.UnixNano(),
		Venue:    venue,
		Frame:    json.RawMessage(frame),
	})
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	f, err := r.fileLocked(r.raw, "raw", venue, tsRecv)
	if err != nil {
		return err
	}
	return f.writeLine(line)
}

// WriteNormalized persists one normalized snapshot.
func (r *Recorder) WriteNormalized(s core.BookSnapshot) error {
	line, err := json.Marshal(s)
	if err != nil {
		return err
	}
	ts := time.Unix(0, s.TsRecv)
	r.mu.Lock()
	defer r.mu.Unlock()
	f, err := r.fileLocked(r.norm, "normalized", s.Venue, ts)
	if err != nil {
		return err
	}
	return f.writeLine(line)
}

// fileLocked returns the current hour's file for (stream, venue), rotating if
// the hour changed. Caller holds r.mu.
func (r *Recorder) fileLocked(m map[string]*rotFile, stream, venue string, ts time.Time) (*rotFile, error) {
	hour := ts.UTC().Truncate(time.Hour)
	f := m[venue]
	if f != nil && f.hour.Equal(hour) {
		return f, nil
	}
	if f != nil {
		if err := f.close(); err != nil {
			return nil, err
		}
	}
	name := fmt.Sprintf("%s-%s.jsonl.gz", venue, hour.Format("2006-01-02T15"))
	path := filepath.Join(r.dir, stream, name)
	nf, err := openRot(path, hour)
	if err != nil {
		return nil, err
	}
	m[venue] = nf
	return nf, nil
}

// Close flushes and closes every open file. Idempotent.
func (r *Recorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var firstErr error
	closeAll := func(m map[string]*rotFile) {
		for k, f := range m {
			if err := f.close(); err != nil && firstErr == nil {
				firstErr = err
			}
			delete(m, k)
		}
	}
	closeAll(r.raw)
	closeAll(r.norm)
	closeAll(r.trades)
	return firstErr
}

// rotFile is one gzipped JSONL file for one hour.
type rotFile struct {
	hour time.Time
	f    *os.File
	gz   *gzip.Writer
}

func openRot(path string, hour time.Time) (*rotFile, error) {
	// Append so a restart within the hour continues the same file rather than
	// truncating already-recorded frames.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return nil, err
	}
	return &rotFile{hour: hour, f: f, gz: gzip.NewWriter(f)}, nil
}

func (rf *rotFile) writeLine(line []byte) error {
	if _, err := rf.gz.Write(line); err != nil {
		return err
	}
	_, err := rf.gz.Write([]byte{'\n'})
	return err
}

// close flushes the gzip stream and fsyncs before closing (design §7.3: fsync
// on rotate so a crash costs at most the last unflushed writes).
func (rf *rotFile) close() error {
	if err := rf.gz.Close(); err != nil {
		_ = rf.f.Close()
		return err
	}
	if err := rf.f.Sync(); err != nil {
		_ = rf.f.Close()
		return err
	}
	return rf.f.Close()
}

// WriteTrade persists one normalized trade print to the trades/ stream.
// Trades are recorded separately from books because they answer a different
// question: a book shows what is OFFERED, a trade shows what actually
// TRANSACTED — which is the only way to measure maker fills (and therefore
// adverse selection) or true market capacity.
func (r *Recorder) WriteTrade(venue string, line []byte, ts time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, err := r.fileLocked(r.trades, "trades", venue, ts)
	if err != nil {
		return err
	}
	return f.writeLine(line)
}

// Stream is one extra hourly-rotated gzip JSONL stream under dir/<name>/, one
// file per prefix per hour ("<prefix>-<YYYY-MM-DDTHH>.jsonl.gz"), for feeds
// recorded beside the venues' (e.g. live game events). Safe for concurrent use.
type Stream struct {
	dir   string
	mu    sync.Mutex
	files map[string]*rotFile
}

// NewStream creates dir/name eagerly, so a permissions problem fails at startup.
func NewStream(dir, name string) (*Stream, error) {
	d := filepath.Join(dir, name)
	if err := os.MkdirAll(d, 0o750); err != nil {
		return nil, err
	}
	return &Stream{dir: d, files: map[string]*rotFile{}}, nil
}

// Write appends one JSON line to prefix's file for ts's hour.
func (s *Stream) Write(prefix string, line []byte, ts time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	hour := ts.UTC().Truncate(time.Hour)
	f := s.files[prefix]
	if f == nil || !f.hour.Equal(hour) {
		if f != nil {
			if err := f.close(); err != nil {
				return err
			}
		}
		nf, err := openRot(filepath.Join(s.dir, fmt.Sprintf("%s-%s.jsonl.gz", prefix, hour.Format("2006-01-02T15"))), hour)
		if err != nil {
			return err
		}
		s.files[prefix], f = nf, nf
	}
	return f.writeLine(line)
}

// Close flushes and closes every open file. Idempotent.
func (s *Stream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var firstErr error
	for k, f := range s.files {
		if err := f.close(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(s.files, k)
	}
	return firstErr
}
