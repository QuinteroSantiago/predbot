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

// Supervisor performs startup reconciliation and runs the kill triggers
// (design §8). It is the safety layer around the Trader: nothing trades until
// Reconcile succeeds, and it Kill()s the engine on any condition that means the
// bot has lost track of reality.
type Supervisor struct {
	accounts []AccountClient
	engine   *risk.Engine
	trader   *Trader
	log      *slog.Logger

	// Kill thresholds.
	MaxUnhedgedFor time.Duration // unhedged longer than this -> kill
	MaxErrors      int           // venue errors within ErrorWindow -> kill
	ErrorWindow    time.Duration
	PnLFloorUSD    float64 // realized+unrealized below this -> kill (negative number)

	// AdoptExisting allows startup to proceed with positions already on the
	// venues, seeding them into the ledger instead of refusing. Opt-in only:
	// the default is to refuse, because at startup the ledger is empty by
	// definition, so every venue position is one the bot cannot account for.
	AdoptExisting bool

	preExisting  map[string]float64 // venueID -> contracts, found by Reconcile
	errTimes     []time.Time
	unhedgedFrom time.Time
}

// PreExisting returns the positions Reconcile found on the venues at startup.
// Only meaningful with AdoptExisting; callers seed the Trader with it so the
// drift monitor does not immediately kill over positions it was told to keep.
func (s *Supervisor) PreExisting() map[string]float64 {
	out := make(map[string]float64, len(s.preExisting))
	for k, v := range s.preExisting {
		out[k] = v
	}
	return out
}

func NewSupervisor(accounts []AccountClient, engine *risk.Engine, trader *Trader) *Supervisor {
	return &Supervisor{
		accounts: accounts, engine: engine, trader: trader,
		log:            slog.With("component", "live-supervisor"),
		MaxUnhedgedFor: 60 * time.Second,
		MaxErrors:      10,
		ErrorWindow:    60 * time.Second,
		PnLFloorUSD:    -25,
	}
}

// Reconcile pulls authoritative state from every venue before any intent is
// accepted (design §8). It returns an error if a venue can't be read — the
// caller MUST NOT trade in that case. Any pre-existing open order or position is
// reported loudly: the bot did not create it in this run, so a human should look.
func (s *Supervisor) Reconcile(ctx context.Context) error {
	s.preExisting = map[string]float64{}
	var found, openOrders []string
	for _, a := range s.accounts {
		pos, err := a.Positions(ctx)
		if err != nil {
			return err
		}
		for id, q := range pos {
			s.preExisting[id] = q
			found = append(found, fmt.Sprintf("%s %s=%v", a.Venue(), id, q))
		}
		orders, err := a.OpenOrders(ctx)
		if err != nil {
			s.log.Warn("open-orders read failed (continuing, but positions were readable)",
				"venue", a.Venue(), "err", err)
		}
		if len(pos) > 0 {
			s.log.Warn("PRE-EXISTING POSITIONS at startup — verify before trading",
				"venue", a.Venue(), "positions", pos)
		}
		if len(orders) > 0 {
			s.log.Warn("PRE-EXISTING OPEN ORDERS at startup — verify before trading",
				"venue", a.Venue(), "count", len(orders))
			for _, id := range orders {
				openOrders = append(openOrders, a.Venue()+" "+id)
			}
		}
		if bal, err := a.BalanceUSD(ctx); err == nil {
			s.log.Info("venue balance", "venue", a.Venue(), "usd", bal)
		}
	}

	// Fail CLOSED. This used to warn and return nil, so preflight printed
	// "reconcile OK" and the bot armed — and then the drift monitor, reading the
	// exact same number 30s later, killed. Two components, identical data,
	// opposite verdicts, with a 30-second window in between where the bot was
	// live and free to trade. Whatever the answer is, it has to be the same one.
	// An open order the bot didn't place this run is never adoptable: it can
	// fill at any moment with no pair behind it. That's exactly how an orphaned
	// rest (its cancel had failed) filled unhedged on 2026-09-24.
	if len(openOrders) > 0 {
		sort.Strings(openOrders)
		return fmt.Errorf("live: %d open order(s) left on the venues: %s — cancel them first (scripts/trading.sh cancel-open)",
			len(openOrders), strings.Join(openOrders, ", "))
	}
	sort.Strings(found)
	if len(found) > 0 && !s.AdoptExisting {
		return fmt.Errorf("live: %d pre-existing position(s) the bot cannot account for: %s — flatten them, or pass -adopt-positions if this is a hedge you have reviewed",
			len(found), strings.Join(found, ", "))
	}
	if len(found) > 0 {
		s.log.Warn("ADOPTING pre-existing positions — the bot will treat these as its own",
			"count", len(found), "positions", strings.Join(found, ", "))
	}
	return nil
}

// NoteError records a venue error; N errors within the window trips the kill
// switch (design §8).
func (s *Supervisor) NoteError() {
	now := time.Now()
	cutoff := now.Add(-s.ErrorWindow)
	kept := s.errTimes[:0]
	for _, t := range s.errTimes {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	s.errTimes = append(kept, now)
	if len(s.errTimes) >= s.MaxErrors {
		s.engine.Kill(fmt.Sprintf("error storm: %d venue errors within %s", len(s.errTimes), s.ErrorWindow))
	}
}

// Run checks the kill conditions on a loop until ctx is done.
func (s *Supervisor) Run(ctx context.Context, pnl func() float64, unhedged func() bool) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if s.engine.Killed() {
				continue
			}
			// P&L floor.
			if v := pnl(); v < s.PnLFloorUSD {
				s.engine.Kill("P&L floor breached")
				s.log.Error("KILL: P&L floor breached", "pnl_usd", v, "floor", s.PnLFloorUSD)
				continue
			}
			// Unhedged for too long.
			if unhedged() {
				if s.unhedgedFrom.IsZero() {
					s.unhedgedFrom = time.Now()
				} else if time.Since(s.unhedgedFrom) > s.MaxUnhedgedFor {
					s.engine.Kill("unhedged position persisted beyond limit")
					s.log.Error("KILL: unhedged too long", "for", time.Since(s.unhedgedFrom))
				}
			} else {
				s.unhedgedFrom = time.Time{}
			}
		}
	}
}
