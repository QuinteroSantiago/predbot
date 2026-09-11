package live

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// Outage is a per-venue circuit breaker for exchange maintenance.
//
// Polymarket US goes down for a weekly window (Thursday 2–6 AM ET) and for
// unannounced emergency maintenance; during it every account read answers 503
// (gRPC code 14 UNAVAILABLE). On 2026-09-11 an unscheduled 1h40m window made the
// live bot's two pollers (drift monitor every 30s, positions gauge every 15s)
// log 582 identical warnings while learning nothing new.
//
// After Threshold consecutive 5xx responses the venue is treated as DOWN: the
// periodic pollers stop calling it and a single probe is let through every Probe
// (15 min). The first successful call — a probe, or any caller that bypasses
// the breaker such as the silent-fill verifier — brings it back immediately.
// Only 5xx counts: a 4xx is our bug, and a timeout may be our own network, so
// neither should silence the venue.
//
// The breaker is also a trading gate: cmd/live opens no new pair on a venue
// that is down, because nothing it placed there could be reconciled.
type Outage struct {
	venue     string
	Threshold int           // consecutive 5xx before the venue is DOWN; default 3
	Probe     time.Duration // while DOWN, one call per Probe; default 15m

	mu        sync.Mutex
	fails     int
	down      bool
	since     time.Time
	nextProbe time.Time
	now       func() time.Time
	log       *slog.Logger
}

// ErrVenueUnavailable is returned instead of calling a venue that is DOWN
// between probes. The breaker has already logged the outage, so callers should
// skip quietly rather than warn on every tick.
var ErrVenueUnavailable = errors.New("venue unavailable (maintenance/outage); call skipped until the next probe")

func NewOutage(venue string) *Outage {
	return &Outage{venue: venue, Threshold: 3, Probe: 15 * time.Minute,
		now: time.Now, log: slog.With("component", "outage", "venue", venue)}
}

// Down reports whether the venue is currently treated as unavailable.
func (o *Outage) Down() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.down
}

// Allow reports whether a periodic caller may hit the venue now. While DOWN it
// admits exactly one caller per Probe window — concurrent pollers sharing the
// breaker do not each probe.
func (o *Outage) Allow() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.down {
		return true
	}
	if now := o.now(); !now.Before(o.nextProbe) {
		o.nextProbe = now.Add(o.Probe)
		return true
	}
	return false
}

// Observe feeds the result of a real venue call into the breaker.
func (o *Outage) Observe(err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	now := o.now()
	switch {
	case err == nil:
		if o.down {
			o.log.Info("venue back up — resuming normal polling and trading",
				"down_for", now.Sub(o.since).Round(time.Second).String())
		}
		o.down, o.fails = false, 0
	case isUnavailable(err):
		o.fails++
		if o.down {
			o.log.Info("venue still unavailable", "next_probe_in", o.Probe.String(), "err", err)
			return
		}
		if o.fails >= o.Threshold {
			o.down, o.since, o.nextProbe = true, now, now.Add(o.Probe)
			o.log.Warn("venue unavailable (maintenance?) — pausing polls to one probe per interval and blocking new pairs on it",
				"consecutive_5xx", o.fails, "probe_every", o.Probe.String(), "err", err)
		}
	}
}

func isUnavailable(err error) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.Status >= 500
}

// GuardedAccount routes an AccountClient's calls through an Outage breaker: a
// DOWN venue answers ErrVenueUnavailable without being called, except for the
// one probe per window. Use it for PERIODIC readers (drift monitor, positions
// gauge, balance poller). A caller that must never be skipped — the silent-fill
// verifier, startup reconciliation — uses Unguarded and reports via Observe.
type GuardedAccount struct {
	AccountClient
	outage *Outage
}

func Guard(a AccountClient, o *Outage) *GuardedAccount {
	return &GuardedAccount{AccountClient: a, outage: o}
}

func (g *GuardedAccount) Outage() *Outage { return g.outage }

// Unguarded returns the raw client, bypassing the breaker.
func (g *GuardedAccount) Unguarded() AccountClient { return g.AccountClient }

func (g *GuardedAccount) Positions(ctx context.Context) (map[string]float64, error) {
	if !g.outage.Allow() {
		return nil, ErrVenueUnavailable
	}
	pos, err := g.AccountClient.Positions(ctx)
	g.outage.Observe(err)
	return pos, err
}

func (g *GuardedAccount) OpenOrders(ctx context.Context) ([]string, error) {
	if !g.outage.Allow() {
		return nil, ErrVenueUnavailable
	}
	ids, err := g.AccountClient.OpenOrders(ctx)
	g.outage.Observe(err)
	return ids, err
}

func (g *GuardedAccount) BalanceUSD(ctx context.Context) (float64, error) {
	if !g.outage.Allow() {
		return 0, ErrVenueUnavailable
	}
	usd, err := g.AccountClient.BalanceUSD(ctx)
	g.outage.Observe(err)
	return usd, err
}
