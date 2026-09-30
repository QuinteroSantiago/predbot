// Package killswitch is a trading kill switch that survives restarts, and the
// one-shot permission markers a human leaves for a single start.
//
// Engaging the switch writes a marker file; a process that finds the marker at
// startup comes up killed and stays killed until a human removes the file.
// Stopping trading is automatic; resuming it is always a human decision.
package killswitch

import (
	"errors"
	"os"
	"sync"
)

// Switch is safe for concurrent use.
type Switch struct {
	mu     sync.Mutex
	file   string
	killed bool
	reason string
}

// New returns a switch persisted at file. If the file already exists the switch
// starts engaged. An empty file keeps the switch in memory only (tests, dry runs).
func New(file string) *Switch {
	s := &Switch{file: file}
	if file != "" {
		if _, err := os.Stat(file); err == nil {
			s.killed = true
			s.reason = "KILLED marker present at startup — remove " + file + " to re-enable"
		}
	}
	return s
}

// Kill engages the switch and persists the marker. Idempotent: the first
// reason is kept.
func (s *Switch) Kill(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.killed {
		return
	}
	s.killed = true
	s.reason = reason
	if s.file != "" {
		_ = os.WriteFile(s.file, []byte(reason+"\n"), 0o640)
	}
}

// Killed reports whether the switch is engaged.
func (s *Switch) Killed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.killed
}

// Reason is why the switch was engaged, or "" if it isn't.
func (s *Switch) Reason() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reason
}

// ConsumeMarker reports whether a one-shot permission marker is present and, if
// so, deletes it, so the permission covers this start and never the next. A
// marker that exists but can't be deleted returns an error: granting a
// permission it can't give back would make it permanent.
func ConsumeMarker(file string) (bool, error) {
	if _, err := os.Stat(file); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if err := os.Remove(file); err != nil {
		return false, err
	}
	return true, nil
}
