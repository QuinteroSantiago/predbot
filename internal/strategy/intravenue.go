package strategy

import "predbot/internal/core"

// IntraVenue detects a crossed YES book on a single venue (design §9.2):
// BestYesBid > BestYesAsk means YesBid + NoBid > $1, a resolution-risk-free
// opportunity. On a matched CLOB this essentially never persists; it appears
// transiently in Kalshi's two-sided representation and is mostly a
// feed-consistency check. Detect-only: it returns a marker Intent per venue with
// a crossed book, for the report to count.
func IntraVenue(canonical string, books map[string]core.BookSnapshot, fees FeeModel, cfg Config) []core.Intent {
	var out []core.Intent
	for venue, b := range books {
		bid, okB := b.BestYesBid()
		ask, okA := b.BestYesAsk()
		if !okB || !okA || bid.Px <= ask.Px {
			continue
		}
		qty := minQty(bid.Qty, ask.Qty, cfg.MaxClip)
		out = append(out, core.Intent{
			PairID: canonical + ":intra:" + venue, Leg: 0, Venue: venue, VenueID: b.VenueID,
			Side: core.BuyYes, Px: ask.Px, Qty: qty, TIF: core.IOC,
			Reason: "intravenue crossed book",
		})
	}
	return out
}
