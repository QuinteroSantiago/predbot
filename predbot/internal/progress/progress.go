// Package progress draws a progress line for long CLI runs. On a terminal it
// redraws one line in place; when the output is redirected (nohup, tee) it
// prints a plain status line every Interval instead, so logs stay readable.
//
// A nil *Bar is valid and does nothing, so callers can disable progress by
// simply not starting one.
package progress

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Interval is how often a redirected (non-terminal) bar prints a status line.
var Interval = 15 * time.Second

// Bar tracks done out of total. Units are whatever the caller counts
// (snapshots, nanoseconds of replayed time); Detail turns them into text.
type Bar struct {
	w     io.Writer
	label string
	total int64
	done  atomic.Int64
	start time.Time
	tty   bool

	// Detail renders the part after the percentage, e.g. "1.2M/3.4M snapshots".
	// It runs on the render goroutine, so it may only read atomics.
	detail func(done, total int64, elapsed time.Duration) string

	quit chan struct{}
	wg   sync.WaitGroup
	once sync.Once
}

// Start begins drawing a bar on stderr. total <= 0 means unknown: the bar shows
// the detail text and elapsed time but no percentage or ETA.
func Start(label string, total int64, detail func(done, total int64, elapsed time.Duration) string) *Bar {
	return start(os.Stderr, isTerminal(os.Stderr), label, total, detail)
}

func start(w io.Writer, tty bool, label string, total int64, detail func(done, total int64, elapsed time.Duration) string) *Bar {
	b := &Bar{w: w, label: label, total: total, start: time.Now(), tty: tty, detail: detail, quit: make(chan struct{})}
	every := Interval
	if tty {
		every = 200 * time.Millisecond
	}
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				b.draw(false)
			case <-b.quit:
				return
			}
		}
	}()
	return b
}

// Add counts n more units done.
func (b *Bar) Add(n int64) {
	if b != nil {
		b.done.Add(n)
	}
}

// Set records progress as an absolute value. It never moves the bar backwards.
func (b *Bar) Set(v int64) {
	if b == nil {
		return
	}
	for {
		cur := b.done.Load()
		if v <= cur || b.done.CompareAndSwap(cur, v) {
			return
		}
	}
}

// Counter exposes the done count for code that increments it directly; nil
// when b is nil.
func (b *Bar) Counter() *atomic.Int64 {
	if b == nil {
		return nil
	}
	return &b.done
}

// Finish stops the bar and prints its final line.
func (b *Bar) Finish() {
	if b == nil {
		return
	}
	b.once.Do(func() {
		close(b.quit)
		b.wg.Wait()
		if b.total > 0 {
			b.done.Store(b.total)
		}
		b.draw(true)
	})
}

func (b *Bar) draw(final bool) {
	done, elapsed := b.done.Load(), time.Since(b.start)
	var line strings.Builder
	line.WriteString(b.label)
	if b.total > 0 {
		frac := min(float64(done)/float64(b.total), 1)
		if b.tty {
			const width = 28
			fill := int(frac * width)
			line.WriteString(" [" + strings.Repeat("█", fill) + strings.Repeat("░", width-fill) + "]")
		}
		fmt.Fprintf(&line, " %5.1f%%", frac*100)
		if d := b.detail; d != nil {
			line.WriteString("  " + d(done, b.total, elapsed))
		}
		switch {
		case final:
			fmt.Fprintf(&line, "  done in %s", round(elapsed))
		case frac > 0.001:
			eta := time.Duration(float64(elapsed) * (1 - frac) / frac)
			fmt.Fprintf(&line, "  %s elapsed, ETA %s", round(elapsed), round(eta))
		default:
			fmt.Fprintf(&line, "  %s elapsed", round(elapsed))
		}
	} else {
		if d := b.detail; d != nil {
			line.WriteString("  " + d(done, b.total, elapsed))
		}
		fmt.Fprintf(&line, "  %s", round(elapsed))
	}
	if b.tty {
		// \r + clear-to-end-of-line redraws in place.
		end := ""
		if final {
			end = "\n"
		}
		fmt.Fprint(b.w, "\r\033[K"+line.String()+end)
		return
	}
	fmt.Fprintln(b.w, line.String())
}

func round(d time.Duration) time.Duration {
	if d < time.Minute {
		return d.Round(100 * time.Millisecond)
	}
	return d.Round(time.Second)
}

// Count formats n compactly: 950, 12.3k, 4.56M.
func Count(n int64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.2fG", float64(n)/1e9)
	case n >= 1_000_000:
		return fmt.Sprintf("%.2fM", float64(n)/1e6)
	case n >= 10_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	default:
		return fmt.Sprint(n)
	}
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
