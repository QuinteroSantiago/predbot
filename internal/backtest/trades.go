package backtest

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"predbot/internal/venue"
)

// LoadTrades reads recorded trade prints (the data/trades stream) inside w,
// ordered by receive time.
//
// Trades are small compared with books — a print is a few fields, not a whole
// order book — so they fit comfortably in memory even over long windows.
func LoadTrades(dir string, w Window) ([]venue.Trade, error) {
	files, err := filepath.Glob(filepath.Join(dir, "trades", "*.jsonl.gz"))
	if err != nil {
		return nil, err
	}
	files = withinWindow(files, w)

	var out []venue.Trade
	for _, f := range files {
		ts, err := readTradeFile(f)
		for _, t := range ts {
			if !w.Ranged() || w.contains(t.TsRecv) {
				out = append(out, t)
			}
		}
		if err != nil {
			continue // a torn current-hour file is expected
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TsRecv < out[j].TsRecv })
	return out, nil
}

func readTradeFile(path string) ([]venue.Trade, error) {
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

	var out []venue.Trade
	sc := bufio.NewScanner(gz)
	sc.Buffer(make([]byte, 0, 1<<16), 1<<20)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var t venue.Trade
		if json.Unmarshal(sc.Bytes(), &t) != nil {
			continue
		}
		out = append(out, t)
	}
	return out, sc.Err()
}

// TradeStream yields recorded trade prints in receive-time order one hour at a
// time, so a pass over the whole archive holds a single hour of prints, not
// all of them (LoadTrades on a month of prints was OOM-killed). Prints are
// sorted within each hour; files rotate on receive time, so hours arrive in
// order too.
type TradeStream struct {
	w      Window
	hours  []string
	byHour map[string][]string
	buf    []venue.Trade
	i      int
}

// OpenTradeStream lists the trade files in w; it reads nothing yet.
func OpenTradeStream(dir string, w Window) (*TradeStream, error) {
	files, err := filepath.Glob(filepath.Join(dir, "trades", "*.jsonl.gz"))
	if err != nil {
		return nil, err
	}
	ts := &TradeStream{w: w, byHour: map[string][]string{}}
	for _, f := range withinWindow(files, w) {
		base := strings.TrimSuffix(filepath.Base(f), ".jsonl.gz")
		if len(base) < 13 {
			continue
		}
		hour := base[len(base)-13:]
		if ts.byHour[hour] == nil {
			ts.hours = append(ts.hours, hour)
		}
		ts.byHour[hour] = append(ts.byHour[hour], f)
	}
	sort.Strings(ts.hours)
	return ts, nil
}

// Peek returns the next print without consuming it; ok is false at the end.
func (s *TradeStream) Peek() (venue.Trade, bool) {
	for s.i >= len(s.buf) {
		if len(s.hours) == 0 {
			return venue.Trade{}, false
		}
		hour := s.hours[0]
		s.hours = s.hours[1:]
		s.buf, s.i = s.buf[:0], 0
		for _, f := range s.byHour[hour] {
			prints, _ := readTradeFile(f) // a torn current-hour file keeps what it has
			for _, t := range prints {
				if !s.w.Ranged() || s.w.contains(t.TsRecv) {
					s.buf = append(s.buf, t)
				}
			}
		}
		delete(s.byHour, hour)
		sort.SliceStable(s.buf, func(a, b int) bool { return s.buf[a].TsRecv < s.buf[b].TsRecv })
	}
	return s.buf[s.i], true
}

// Next consumes the print Peek returned.
func (s *TradeStream) Next() { s.i++ }

// Empty reports whether the stream has no prints at all. It reads ahead.
func (s *TradeStream) Empty() bool {
	_, ok := s.Peek()
	return !ok
}
