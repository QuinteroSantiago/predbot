package live

// Maker execution (design §9.2 "Maker variants").
//
// The taker path (ExecutePair) fires the lead IOC first and alone so a naked leg
// is impossible. The MAKER path deliberately does the opposite: it RESTS one leg
// (TIF GTC) to earn the Polymarket rebate, waits to be filled, and only then
// hedges the other leg. A filled rest is therefore NAKED until the hedge lands —
// the very failure mode the taker sequencing exists to prevent — so the guards
// here matter:
//
//  1. Both legs are risk-gated BEFORE anything is placed, so a rest that fills
//     can always be hedged within buying power.
//  2. The rest is cancelled after restWait if it has not filled — an unfilled
//     rest carries no position, so that is the clean, common outcome.
//  3. The instant the rest fills (fully or partially), the taking leg is hedged
//     through the SAME completeOrExit machinery the taker path uses: re-attempt
//     at the touch for hedgeGrace, then IOC-exit the naked remainder, then let
//     the supervisor's unhedged cap Kill(). Nothing new is trusted.
//  4. A fill can race the cancel; after cancelling we drain briefly and hedge
//     whatever actually filled, measured from the venue fill events (never from
//     the place() return), exactly as the taker path does.
//
// The rest-wait and hedge run in the BACKGROUND so the single-threaded live loop
// is never blocked while an order sits on the book.

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"predbot/internal/core"
	"predbot/internal/risk"
)

// ExecuteMakerPair rests one leg and hedges the other once it fills. It returns
// quickly (after the rest is placed); the wait/cancel/hedge sequence runs in the
// background. A pair with no resting leg falls back to the taker path.
func (t *Trader) ExecuteMakerPair(ctx context.Context, canonical string, legs []core.Intent) error {
	if len(legs) != 2 {
		return fmt.Errorf("live: want 2 legs, got %d", len(legs))
	}
	if t.engine.Killed() {
		return risk.ErrKilled
	}

	restIdx := -1
	for i, l := range legs {
		if l.TIF == core.GTC || l.TIF == core.GTD {
			restIdx = i
			break
		}
	}
	if restIdx == -1 {
		// No resting leg: this is a plain taker/taker pair, run the taker path.
		return t.ExecutePair(ctx, canonical, legs)
	}
	rest := legs[restIdx]
	take := legs[1-restIdx]

	t.bump(func(s *Stats) { s.PairsAttempted++ })

	// Risk-gate BOTH legs before resting anything, so a rest that fills can always
	// be hedged in full. The take reservation is released immediately after — it
	// only proves the hedge COULD clear; completeOrExit re-reserves per attempt at
	// the live touch when the rest actually fills.
	if err := t.engine.CheckAndReserve(canonical, rest); err != nil {
		t.bump(func(s *Stats) { s.Rejected++ })
		return fmt.Errorf("live: rest risk: %w", err)
	}
	if err := t.engine.CheckAndReserve(canonical, take); err != nil {
		t.engine.OnReject(canonical, rest)
		t.bump(func(s *Stats) { s.Rejected++ })
		return fmt.Errorf("live: take risk: %w", err)
	}
	t.engine.OnReject(canonical, take)

	ex, ok := t.ex[rest.Venue]
	if !ok {
		t.engine.OnReject(canonical, rest)
		return fmt.Errorf("live: no executor for venue %q", rest.Venue)
	}

	o := core.Order{Intent: rest, ClientID: uuid.NewString()}
	ch := t.registerSink(o.ClientID)

	placeCtx, cancelPlace := context.WithTimeout(ctx, 5*time.Second)
	id, err := ex.Place(placeCtx, o)
	cancelPlace()
	if err != nil {
		t.forget(o.ClientID)
		t.engine.OnReject(canonical, rest)
		if t.onOrder != nil {
			t.onOrder(rest.Venue, "error")
		}
		return fmt.Errorf("live: rest place: %w", err)
	}
	if id != "" && id != o.ClientID {
		t.alias(id, o.ClientID)
	}
	t.bump(func(s *Stats) { s.OrdersPlaced++ })
	t.log.Info("maker rest placed",
		"canonical", canonical, "venue", rest.Venue, "side", rest.Side.String(),
		"px", rest.Px.String(), "qty", rest.Qty.String(),
		"client_id", o.ClientID, "venue_order_id", id, "rest_wait", t.restWait.String())

	cancelID := id
	if cancelID == "" {
		cancelID = o.ClientID
	}

	t.pending.Add(1)
	go func() {
		defer t.pending.Done()
		defer t.forget(o.ClientID)
		if id != "" && id != o.ClientID {
			defer t.forget(id)
		}
		t.restLifecycle(ctx, canonical, rest, take, ch, ex, cancelID)
	}()
	return nil
}

// restLifecycle waits for the resting leg to fill, cancels it, then hedges
// whatever actually filled. Runs in its own goroutine.
func (t *Trader) restLifecycle(ctx context.Context, canonical string, rest, take core.Intent,
	ch <-chan Fill, ex interface {
		Cancel(context.Context, string) error
	}, cancelID string) {

	filled := drain(ch, rest.Qty, t.restWait)

	// Cancel the rest regardless of outcome. Use a detached context so a shutdown
	// (rootCtx cancelled) still cancels the resting order rather than orphaning it
	// on the book. A full fill makes cancel a harmless no-op.
	cctx, cc := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	if cerr := ex.Cancel(cctx, cancelID); cerr != nil {
		t.log.Warn("maker rest cancel failed (may already be filled/gone)",
			"canonical", canonical, "venue", rest.Venue, "order_id", cancelID, "err", cerr)
	}
	cc()

	// A fill can race the cancel: drain briefly for anything that landed just
	// before the venue acknowledged the cancel.
	if rem := rest.Qty - filled; rem > 0 {
		filled += drain(ch, rem, 500*time.Millisecond)
	}
	if filled > rest.Qty {
		filled = rest.Qty
	}

	// Move the risk engine's reservation to a position for what filled, and
	// release the remainder that never will.
	if filled > 0 {
		t.engine.OnFill(canonical, rest, filled)
		if t.onOrder != nil {
			t.onOrder(rest.Venue, "filled")
		}
	} else if t.onOrder != nil {
		t.onOrder(rest.Venue, "faded")
	}
	if rem := rest.Qty - filled; rem > 0 {
		rel := rest
		rel.Qty = rem
		t.engine.OnReject(canonical, rel)
	}

	if filled == 0 {
		// The rest never filled — no position, the clean and common outcome.
		// "No fill EVENT" is still not proof of no position, so verify the venue.
		if verr := t.verifyNoSilentFill(ctx, canonical, rest); verr != nil {
			return
		}
		t.log.Info("maker rest cancelled unfilled; pair abandoned cleanly",
			"canonical", canonical, "venue", rest.Venue)
		return
	}

	// The rest FILLED: we are naked on the resting venue until the hedge lands.
	// Hand off to the same complete-or-exit path the taker legging uses — it
	// re-attempts the hedge at the touch, then IOC-exits, then escalates to Kill.
	t.engine.SetUnhedged(canonical, filled)
	t.bump(func(s *Stats) { s.PairsLegged++; s.UnhedgedQty += filled })
	t.log.Warn("maker rest FILLED — hedging the taking leg",
		"canonical", canonical, "naked_venue", rest.Venue, "naked_qty", filled.String(),
		"hedge_venue", take.Venue, "grace", t.hedgeGrace.String())
	t.completeOrExit(ctx, canonical, rest, take, filled)
}

// registerSink installs a fill channel for clientID and claims any fills that
// arrived before registration. Mirrors awaitFills' registration but hands back
// the raw channel so the maker path can accumulate across a cancel.
func (t *Trader) registerSink(clientID string) <-chan Fill {
	ch := make(chan Fill, 8)
	t.mu.Lock()
	t.awaiting[clientID] = ch
	t.claimParkedLocked(clientID, ch)
	t.mu.Unlock()
	return ch
}

// drain accumulates fills from ch until `want` contracts have arrived or d
// elapses, returning the quantity drained in THIS call. Partial fills keep the
// timer running until want or the deadline. want <= 0 returns immediately.
func drain(ch <-chan Fill, want core.Qty, d time.Duration) core.Qty {
	if want <= 0 {
		return 0
	}
	var total core.Qty
	deadline := time.After(d)
	for {
		select {
		case f := <-ch:
			total += f.Qty
			if total >= want {
				return total
			}
		case <-deadline:
			return total
		}
	}
}
