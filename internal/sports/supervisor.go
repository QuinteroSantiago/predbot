package sports

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"predbot/internal/book"
	"predbot/internal/config"
	"predbot/internal/core"
	"predbot/internal/obs"
	"predbot/internal/venue"
	"predbot/internal/venue/kalshi"
	"predbot/internal/venue/polymarketus"
)

// Supervisor keeps the recorder/trader subscribed to today's cross-listed daily
// games. Every refresh it rediscovers the slate, registers new games with the
// Book Manager, and — when the set changes — restarts the sports ingesters over
// the same WS dialers. Games that finish simply stop being listed and drop out.
//
// Shared by cmd/record and cmd/live so both see the same market set.
type Supervisor struct {
	Leagues []string
	KDial   venue.Dialer
	PDial   venue.Dialer
	Rec     venue.RawRecorder // venue.NopRecorder() when recording isn't wanted
	Met     *obs.Metrics
	BM      *book.Manager
	Out     chan<- core.BookSnapshot
	Refresh time.Duration // default 5m

	client *Client
	log    *slog.Logger

	mu     sync.Mutex
	starts map[string]time.Time // canonical -> scheduled start
}

// StartTime is a discovered game's scheduled start, if known.
func (s *Supervisor) StartTime(canonical string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.starts[canonical]
	return t, ok
}

// Run blocks until ctx is done, keeping the sports subscription current.
func (s *Supervisor) Run(ctx context.Context) {
	if s.Refresh == 0 {
		s.Refresh = 5 * time.Minute
	}
	s.client = New()
	s.log = slog.With("component", "sports-supervisor")

	var cancel context.CancelFunc
	var lastSig string
	defer func() {
		if cancel != nil {
			cancel()
		}
	}()

	discover := func() {
		var matches []Match
		for _, lg := range s.Leagues {
			k, ek := s.client.DiscoverKalshi(ctx, lg)
			p, ep := s.client.DiscoverPoly(ctx, lg)
			if ek != nil || ep != nil {
				s.log.Warn("discovery error, keeping current set", "league", lg, "kalshi_err", ek, "poly_err", ep)
				return
			}
			matches = append(matches, MatchGames(k, p)...)
		}
		starts := make(map[string]time.Time, len(matches))
		for _, m := range matches {
			if !m.Start.IsZero() {
				starts[m.Canonical] = m.Start
			}
		}
		// Updated every refresh, not only when the set changes: a postponed game
		// keeps its ids but moves its start.
		s.mu.Lock()
		s.starts = starts
		s.mu.Unlock()

		var mkts []config.Market
		var kt, ps []string
		for _, m := range matches {
			var mk config.Market
			mk.Canonical = m.Canonical
			mk.Venues.Kalshi.Ticker = m.KalshiTicker
			mk.Venues.PolymarketUS.Slug = m.PolySlug
			mkts = append(mkts, mk)
			kt = append(kt, m.KalshiTicker)
			ps = append(ps, m.PolySlug)
		}
		sort.Strings(kt)
		sort.Strings(ps)
		sig := strings.Join(kt, ",") + "|" + strings.Join(ps, ",")
		if sig == lastSig {
			return
		}
		lastSig = sig
		s.BM.AddMarkets(mkts)

		if cancel != nil {
			cancel()
		}
		var sctx context.Context
		sctx, cancel = context.WithCancel(ctx)
		if len(kt) > 0 {
			go func() { _ = kalshi.New(s.KDial, s.Rec, s.Met).Stream(sctx, kt, s.Out) }()
		}
		if len(ps) > 0 {
			go func() { _ = polymarketus.New(s.PDial, s.Rec, s.Met).Stream(sctx, ps, s.Out) }()
		}
		s.log.Info("sports subscription updated", "games", len(matches),
			"kalshi_markets", len(kt), "poly_markets", len(ps))
	}

	discover()
	t := time.NewTicker(s.Refresh)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			discover()
		}
	}
}

// Live reports whether a canonical id is a sports market (they expire, unlike
// the static basket, so callers must age them out of any "already entered" set).
func Live(canonical string) bool {
	league, _, ok := strings.Cut(canonical, "-")
	_, known := leagueSeries[strings.ToLower(league)]
	return ok && known
}
