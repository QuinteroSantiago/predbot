package core

// Quote derivation. The strategy never thinks about NO books: a NO quote is
// derived from the opposite YES quote (design §5, §6.3):
//
//	NoBid = OneDollar - YesAsk   (best NO bid comes from the best YES ask)
//	NoAsk = OneDollar - YesBid
//
// The quantity carries across unchanged: the depth available to sell YES at a
// price is exactly the depth available to buy NO at (1 - that price).

// BestYesBid returns the highest YES bid, or ok=false if the bid side is empty.
func (b *BookSnapshot) BestYesBid() (Level, bool) {
	if len(b.YesBids) == 0 {
		return Level{}, false
	}
	return b.YesBids[0], true
}

// BestYesAsk returns the lowest YES ask, or ok=false if the ask side is empty.
func (b *BookSnapshot) BestYesAsk() (Level, bool) {
	if len(b.YesAsks) == 0 {
		return Level{}, false
	}
	return b.YesAsks[0], true
}

// BestNoBid = OneDollar - BestYesAsk, same quantity.
func (b *BookSnapshot) BestNoBid() (Level, bool) {
	ask, ok := b.BestYesAsk()
	if !ok {
		return Level{}, false
	}
	return Level{Px: OneDollar - ask.Px, Qty: ask.Qty}, true
}

// BestNoAsk = OneDollar - BestYesBid, same quantity.
func (b *BookSnapshot) BestNoAsk() (Level, bool) {
	bid, ok := b.BestYesBid()
	if !ok {
		return Level{}, false
	}
	return Level{Px: OneDollar - bid.Px, Qty: bid.Qty}, true
}

// Crossed reports whether the YES book is crossed (best bid >= best ask). On a
// matched CLOB this essentially never persists, but it can appear transiently
// in Kalshi's two-sided representation during delta application and is the
// resolution-risk-free intra-venue signal (design §9.2). Phase 1 uses it only
// as a feed-consistency check.
func (b *BookSnapshot) Crossed() bool {
	bid, okB := b.BestYesBid()
	ask, okA := b.BestYesAsk()
	return okB && okA && bid.Px >= ask.Px
}
