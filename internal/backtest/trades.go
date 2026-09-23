package backtest

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"

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
