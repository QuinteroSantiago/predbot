// Package state is what the bot remembers between runs: the holdings it left
// behind and the equity high-water mark. Holdings let the next run notice
// anything that changed the account behind the bot's back.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"time"
)

type State struct {
	Holdings   map[string]float64 `json:"holdings"` // shares, as left by the last run
	PeakEquity float64            `json:"peak_equity"`
	LastRun    time.Time          `json:"last_run"`
}

// Load reads path; a missing file is a first run.
func Load(path string) (State, bool, error) {
	var s State
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return State{Holdings: map[string]float64{}}, false, nil
	}
	if err != nil {
		return s, false, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, false, fmt.Errorf("state %s: %w", path, err)
	}
	if s.Holdings == nil {
		s.Holdings = map[string]float64{}
	}
	return s, true, nil
}

// Save writes atomically: a crash leaves the old state or the new, never half.
func Save(path string, s State) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Diff lists every symbol whose broker holding differs from the expected one
// by more than a millionth of a share.
func Diff(expected, actual map[string]float64) []string {
	syms := map[string]bool{}
	for s := range expected {
		syms[s] = true
	}
	for s := range actual {
		syms[s] = true
	}
	var out []string
	for s := range syms {
		if math.Abs(expected[s]-actual[s]) > 1e-6 {
			out = append(out, fmt.Sprintf("%s expected %g, broker has %g", s, expected[s], actual[s]))
		}
	}
	sort.Strings(out)
	return out
}
