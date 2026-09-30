// Package journal records the feed to disk (design §7.3). Two streams per venue:
//
//   - raw:        the ground truth. One gzipped JSONL file per venue per hour,
//     line = {ts_recv_ns, venue, frame}. Everything else can be regenerated
//     from these frames.
//   - normalized: convenience for the backtester. line = core.BookSnapshot.
//
// Files rotate hourly (tradekit/journal); a rotate fsyncs and closes the old
// file. Close() (wired to SIGHUP/shutdown in cmd/record) closes everything cleanly.
package journal

import (
	"encoding/json"
	"errors"
	"time"

	"predbot/internal/core"
	"tradekit/journal"
)

// rawLine is one line of a raw file.
type rawLine struct {
	TsRecvNs int64           `json:"ts_recv_ns"`
	Venue    string          `json:"venue"`
	Frame    json.RawMessage `json:"frame"`
}

// Recorder writes the raw, normalized and trades streams, one file per venue
// per hour. Safe for concurrent use: both ingesters share one Recorder.
type Recorder struct {
	raw, norm, trades *journal.Stream
}

// New returns a Recorder writing under dir/{raw,normalized,trades}. It creates
// the directories eagerly so a permissions problem fails at startup, not mid-feed.
func New(dir string) (*Recorder, error) {
	raw, err1 := journal.NewStream(dir, "raw")
	norm, err2 := journal.NewStream(dir, "normalized")
	trades, err3 := journal.NewStream(dir, "trades")
	if err := errors.Join(err1, err2, err3); err != nil {
		return nil, err
	}
	return &Recorder{raw: raw, norm: norm, trades: trades}, nil
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
	return r.raw.Write(venue, line, tsRecv)
}

// WriteNormalized persists one normalized snapshot.
func (r *Recorder) WriteNormalized(s core.BookSnapshot) error {
	line, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return r.norm.Write(s.Venue, line, time.Unix(0, s.TsRecv))
}

// WriteTrade persists one normalized trade print to the trades/ stream.
// Trades are recorded separately from books because they answer a different
// question: a book shows what is OFFERED, a trade shows what actually
// TRANSACTED — which is the only way to measure maker fills (and therefore
// adverse selection) or true market capacity.
func (r *Recorder) WriteTrade(venue string, line []byte, ts time.Time) error {
	return r.trades.Write(venue, line, ts)
}

// Close flushes and closes every open file. Idempotent.
func (r *Recorder) Close() error {
	return errors.Join(r.raw.Close(), r.norm.Close(), r.trades.Close())
}
