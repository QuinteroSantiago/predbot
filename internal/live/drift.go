package live

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"predbot/internal/risk"
)

// DriftMonitor treats the VENUES' reported positions as the only truth and
// continuously compares them against what the bot believes it holds.
//
// This exists because fill streams can silently fail. On 2026-08-24 the
// Polymarket private stream connected but delivered no executions: the bot sold
// 5 contracts, never saw a fill, believed it held nothing, and sat on an
// unhedged short for hours. Startup-only reconciliation could not catch that.
//
// Any position the bot did not knowingly open is an emergency: it means our
// accounting is wrong, so the risk limits are meaningless. The monitor Kill()s
// rather than trying to reason about it.
type DriftMonitor struct {
	accounts []AccountClient
	engine   *risk.Engine
	expected func() map[string]float64 // venueID -> contracts the bot believes it holds
	log      *slog.Logger

	Interval  time.Duration // default 30s
	Tolerance float64       // contracts of allowed mismatch (fills in flight); default 0.01
}

func NewDriftMonitor(accounts []AccountClient, engine *risk.Engine, expected func() map[string]float64) *DriftMonitor {
	return &DriftMonitor{
		accounts: accounts, engine: engine, expected: expected,
		log:      slog.With("component", "drift-monitor"),
		Interval: 30 * time.Second, Tolerance: 0.01,
	}
}

// Run polls both venues until ctx is done, or until the engine is killed.
//
// Standing down after a kill is deliberate. The engine's kill is latched and
// blocks all trading, so no NEW drift can appear once it fires — but the drift
// itself does not go away (a human still has to close the position). Without
// this guard the monitor re-detected the same position every Interval and
// re-killed forever: one real event on 2026-08-25 produced 61 identical ERROR
// lines and was still going. That noise is not harmless — a flooded journal is
// what hid the original naked-short incident in the first place.
func (d *DriftMonitor) Run(ctx context.Context) {
	t := time.NewTicker(d.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if d.engine.Killed() {
				d.log.Warn("drift monitor standing down — engine is killed; positions still need a human",
					"reason", "kill latched")
				return
			}
			d.check(ctx)
		}
	}
}

func (d *DriftMonitor) check(ctx context.Context) {
	want := map[string]float64{}
	if d.expected != nil {
		want = d.expected()
	}
	for _, a := range d.accounts {
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		actual, err := a.Positions(cctx)
		cancel()
		if err != nil {
			d.log.Warn("position read failed; cannot verify drift", "venue", a.Venue(), "err", err)
			continue
		}
		// Reconciliation log EVERY poll — venue truth vs what the bot believes,
		// per held position. This makes the hedge state (and any naked/half-filled
		// leg) continuously visible in the journal, not only at the moment it
		// trips the kill. INFO when everything matches, WARN when it does not.
		var held, mism int
		var detail []string
		for venueID, qty := range actual {
			if qty == 0 {
				continue
			}
			held++
			exp := want[venueID]
			tag := "ok"
			if diff := qty - exp; diff > d.Tolerance || diff < -d.Tolerance {
				tag, mism = "MISMATCH", mism+1
			}
			detail = append(detail, fmt.Sprintf("%s venue=%.2f bot=%.2f %s", venueID, qty, exp, tag))
		}
		sort.Strings(detail)
		rec := d.log.Info
		if mism > 0 {
			rec = d.log.Warn
		}
		rec("position reconciliation", "venue", a.Venue(), "held", held,
			"mismatches", mism, "detail", strings.Join(detail, " | "))

		for venueID, qty := range actual {
			exp := want[venueID]
			if diff := qty - exp; diff > d.Tolerance || diff < -d.Tolerance {
				reason := fmt.Sprintf("POSITION DRIFT on %s %s: venue says %.2f, bot believes %.2f",
					a.Venue(), venueID, qty, exp)
				d.log.Error("KILL: "+reason,
					"venue", a.Venue(), "market", venueID, "venue_qty", qty, "bot_qty", exp)
				d.engine.Kill(reason)
				return
			}
		}
	}
}
