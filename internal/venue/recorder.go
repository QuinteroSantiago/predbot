package venue

import "time"

// RawRecorder persists raw venue frames. journal.Recorder satisfies it. Raw
// frames are the ground truth (design §7.3): the ingester records every frame
// before processing it, so a parsing bug can be fixed and the stream replayed.
type RawRecorder interface {
	WriteRaw(venue string, frame []byte, ts time.Time) error
}

// nopRecorder discards frames; used when recording is not wired (tests).
type nopRecorder struct{}

func (nopRecorder) WriteRaw(string, []byte, time.Time) error { return nil }

// NopRecorder returns a RawRecorder that discards everything.
func NopRecorder() RawRecorder { return nopRecorder{} }
