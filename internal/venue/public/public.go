// Package public implements credential-free ingesters that poll each venue's
// PUBLIC REST order-book endpoints, instead of the authenticated WebSocket
// feeds. This is the design's sanctioned fallback (§16 open item #5: "can record
// via public gateway REST polling if WS auth is delayed") and lets Phase 1
// record real data with no API keys and no KYC.
//
// It feeds the exact same recorder, metrics, and normalized-snapshot pipeline as
// the WS ingesters, so a later switch to the WS path changes only the source.
// Polling has no sequence numbers and no server-driven backpressure, so the
// seq-gap / conflation / drop metrics do not apply here.
package public

import (
	"context"
	"net/http"
	"time"

	"predbot/internal/core"
	"predbot/internal/obs"
)

// emit publishes a snapshot, counting it. Polling has no backpressure semantics
// to preserve (each poll is already the latest state), so a full channel just
// drops this tick; the next poll supersedes it.
func emit(ctx context.Context, out chan<- core.BookSnapshot, s core.BookSnapshot, met *obs.Metrics, venueName string) {
	select {
	case out <- s:
		met.SnapshotsTotal.Add(ctx, 1, obs.Venue(venueName))
	case <-ctx.Done():
	default:
		met.Dropped.Add(ctx, 1, obs.Venue(venueName))
	}
}

// httpClient is shared; short timeout because these are small book snapshots.
// pollHTTPTimeout bounds one public-REST book poll.
const pollHTTPTimeout = 8 * time.Second

var httpClient = &http.Client{Timeout: pollHTTPTimeout}

func getJSON(ctx context.Context, url string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	buf := make([]byte, 0, 8192)
	tmp := make([]byte, 8192)
	for {
		n, rerr := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if rerr != nil {
			break
		}
		if len(buf) > 4<<20 { // 4MB safety cap
			break
		}
	}
	return buf, resp.StatusCode, nil
}
