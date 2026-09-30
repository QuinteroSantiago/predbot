package journal

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Keeper marks hours in which something interesting happened (for predbot, a
// cross-venue edge at or above a threshold) so the retention pruner can delete
// routine data while preserving every marked hour forever.
//
// A marker is an empty file `keep/<YYYY-MM-DDTHH>.keep` containing the reason.
// Pruning consults these markers; anything marked is never deleted.
type Keeper struct {
	dir string

	mu     sync.Mutex
	marked map[string]bool // hour key -> already written this run
}

func NewKeeper(dataDir string) (*Keeper, error) {
	d := filepath.Join(dataDir, "keep")
	if err := os.MkdirAll(d, 0o750); err != nil {
		return nil, err
	}
	return &Keeper{dir: d, marked: map[string]bool{}}, nil
}

// HourKey is the marker name for a timestamp (UTC hour).
func HourKey(t time.Time) string { return t.UTC().Format("2006-01-02T15") }

// Mark records that the given hour is worth keeping. Idempotent and cheap: it
// writes at most one marker per hour per process.
func (k *Keeper) Mark(t time.Time, reason string) {
	key := HourKey(t)
	k.mu.Lock()
	if k.marked[key] {
		k.mu.Unlock()
		return
	}
	k.marked[key] = true
	k.mu.Unlock()

	path := filepath.Join(k.dir, key+".keep")
	_ = os.WriteFile(path, []byte(reason+"\n"), 0o640)
}

// MarkWindow marks the hour of t plus the surrounding hours, so the context
// leading into and out of an opportunity survives pruning too.
func (k *Keeper) MarkWindow(t time.Time, reason string) {
	for _, d := range []time.Duration{-time.Hour, 0, time.Hour} {
		k.Mark(t.Add(d), fmt.Sprintf("%s (window around %s)", reason, HourKey(t)))
	}
}
