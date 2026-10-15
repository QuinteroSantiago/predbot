// Package journal records data to disk as hourly-rotated gzip JSONL: one file
// per prefix per hour, "<dir>/<name>/<prefix>-<YYYY-MM-DDTHH>.jsonl.gz". A
// rotate fsyncs and closes the old file, so a crash costs at most the writes
// not yet flushed. Keep markers protect chosen hours from retention pruning.
package journal

import (
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Stream is one hourly-rotated gzip JSONL stream under dir/<name>/, one file
// per prefix per hour. Safe for concurrent use.
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

// rotFile is one gzipped JSONL file for one hour.
type rotFile struct {
	hour time.Time
	f    *os.File
	gz   *gzip.Writer
}

func openRot(path string, hour time.Time) (*rotFile, error) {
	// Append so a restart within the hour continues the same file rather than
	// truncating already-recorded lines.
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

// close flushes the gzip stream and fsyncs before closing, so a crash costs
// at most the last unflushed writes.
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
