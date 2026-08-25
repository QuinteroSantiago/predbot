package backtest

import (
	"predbot/internal/core"
)

// RestingFill decides whether a resting (maker) order would have been filled by
// the market, replaying the book forward.
//
// This is the crux of the maker strategy and the easiest place to fool yourself.
// A taker fill is observable: the liquidity was there, you crossed, you got it.
// A MAKER fill depends on someone else choosing to cross to YOU, which the
// recording only tells us indirectly. Two rules, both deliberately pessimistic:
//
//  1. FILL ONLY ON A CROSS. A resting buy at P fills only when the market's best
//     ASK trades down to <= P (someone was willing to sell at your price). Merely
//     having the best bid is not a fill — that is the mistake that makes maker
//     backtests look free.
//
//  2. QUEUE POSITION IS UNKNOWN. Polymarket does not expose it (Kalshi does, via
//     the queue-position endpoints — a TODO worth using). We assume we are at the
//     BACK of the queue: the cross must exceed the size resting ahead of us at
//     that price before any of our quantity fills.
//
// What this still does NOT model, and you should not forget it: ADVERSE
// SELECTION. Your rest fills exactly when someone wants the other side badly —
// disproportionately when the price is about to move against you. A backtest that
// says maker-resting is profitable can still lose money live for this reason.
type RestingFill struct {
	// Intent is the resting order.
	Intent core.Intent
	// QueueAhead is the size resting at our price when we joined (we assume we
	// are behind all of it).
	QueueAhead core.Qty
	// Filled is how much has filled so far.
	Filled core.Qty
	// crossed accumulates size that traded through our price.
	crossed core.Qty
}

// NewRestingFill starts tracking a resting order against the book it joined.
func NewRestingFill(i core.Intent, joined core.BookSnapshot) *RestingFill {
	r := &RestingFill{Intent: i}
	// Size already resting at our price is ahead of us in the queue.
	levels := joined.YesBids
	if i.Side == core.SellYes {
		levels = joined.YesAsks
	}
	for _, lv := range levels {
		if lv.Px == i.Px {
			r.QueueAhead = lv.Qty
			break
		}
	}
	return r
}

// Observe advances the resting order against a new book snapshot and reports the
// quantity newly filled. It returns 0 unless the market actually crossed our
// price and consumed the queue ahead of us.
func (r *RestingFill) Observe(b core.BookSnapshot) core.Qty {
	if r.Filled >= r.Intent.Qty {
		return 0
	}
	var through core.Qty
	if r.Intent.Side == core.BuyYes {
		// We rest a BID at Px. Someone selling into us shows up as an ASK at or
		// below our price.
		for _, lv := range b.YesAsks {
			if lv.Px <= r.Intent.Px {
				through += lv.Qty
			}
		}
	} else {
		// We rest an ASK at Px; a BID at or above our price is someone buying.
		for _, lv := range b.YesBids {
			if lv.Px >= r.Intent.Px {
				through += lv.Qty
			}
		}
	}
	if through <= r.crossed {
		return 0 // nothing new crossed
	}
	newCross := through - r.crossed
	r.crossed = through

	// The queue ahead of us absorbs the cross first.
	if r.QueueAhead > 0 {
		if newCross <= r.QueueAhead {
			r.QueueAhead -= newCross
			return 0
		}
		newCross -= r.QueueAhead
		r.QueueAhead = 0
	}
	remaining := r.Intent.Qty - r.Filled
	if newCross > remaining {
		newCross = remaining
	}
	r.Filled += newCross
	return newCross
}

// Complete reports whether the resting order is fully filled.
func (r *RestingFill) Complete() bool { return r.Filled >= r.Intent.Qty }
