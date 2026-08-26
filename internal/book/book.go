// Package book is the Book Manager (design §3, §4): it stores the latest
// normalized snapshot per venue+market and joins them under a canonical market
// id from markets.yaml. It refuses to key a snapshot to a canonical id that
// isn't in the map (design §12).
package book

import (
	"sync"

	"predbot/internal/config"
	"predbot/internal/core"
)

func key(venue, venueID string) string { return venue + "|" + venueID }

// Manager owns the canonical mapping and the latest-snapshot store. Safe for
// concurrent use: the live loop drives Update while a discovery loop may
// AddMarkets and callers read via Pair/CrossEdge.
type Manager struct {
	mu            sync.RWMutex
	canonByKey    map[string]string            // "venue|venueID" -> canonical
	venueIDByPair map[string]map[string]string // canonical -> venue -> venueID
	latest        map[string]core.BookSnapshot // "venue|venueID" -> snapshot
}

// NewManager builds the mapping from markets.yaml.
func NewManager(markets []config.Market) *Manager {
	m := &Manager{
		canonByKey:    map[string]string{},
		venueIDByPair: map[string]map[string]string{},
		latest:        map[string]core.BookSnapshot{},
	}
	m.addLocked(markets)
	return m
}

// AddMarkets registers additional markets at runtime (e.g. sports games as they
// are discovered). Idempotent: markets already present are skipped.
func (m *Manager) AddMarkets(markets []config.Market) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addLocked(markets)
}

func (m *Manager) addLocked(markets []config.Market) {
	for _, mk := range markets {
		if _, exists := m.venueIDByPair[mk.Canonical]; exists {
			continue
		}
		m.venueIDByPair[mk.Canonical] = map[string]string{}
		if t := mk.Venues.Kalshi.Ticker; t != "" {
			m.canonByKey[key("kalshi", t)] = mk.Canonical
			m.venueIDByPair[mk.Canonical]["kalshi"] = t
		}
		if s := mk.Venues.PolymarketUS.Slug; s != "" {
			m.canonByKey[key("polymarketus", s)] = mk.Canonical
			m.venueIDByPair[mk.Canonical]["polymarketus"] = s
		}
	}
}

// Update stores a snapshot and returns the canonical id it maps to. ok=false
// means the snapshot's venue+id is not in markets.yaml — the caller drops it
// (design §12: the Book Manager refuses unknown ids).
func (m *Manager) Update(s core.BookSnapshot) (canonical string, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.canonByKey[key(s.Venue, s.VenueID)]
	if !ok {
		return "", false
	}
	m.latest[key(s.Venue, s.VenueID)] = s
	return c, true
}

// Pair returns the latest snapshot from every venue mapped to canonical. ok is
// true only when every mapped venue has produced at least one snapshot, i.e. the
// canonical market is ready for a cross-venue comparison.
func (m *Manager) Pair(canonical string) (map[string]core.BookSnapshot, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	venues, exists := m.venueIDByPair[canonical]
	if !exists || len(venues) == 0 {
		return nil, false
	}
	out := make(map[string]core.BookSnapshot, len(venues))
	for venue, venueID := range venues {
		s, seen := m.latest[key(venue, venueID)]
		if !seen {
			return nil, false
		}
		out[venue] = s
	}
	return out, true
}

// CrossEdge returns the best cross-venue gross edge for a canonical market in
// cents (max over both directions of best-bid − best-ask), the fillable size in
// contracts behind that edge (the min of the two tops' quantities), and the
// oldest of the venues' receive times so the caller can gate on freshness. ok is
// false unless every mapped venue has a book with both a bid and an ask.
//
// The size matters as much as the edge: a wide edge against a one-lot book is a
// phantom, not an arb. Callers (and the live-arb surface) gate on size the same
// way the trading strategy does (min-depth), so dust edges never read as
// opportunities.
func (m *Manager) CrossEdge(canonical string) (cents, sizeContracts float64, oldestRecv int64, ok bool) {
	books, ready := m.Pair(canonical)
	if !ready {
		return 0, 0, 0, false
	}
	venues := make([]string, 0, len(books))
	oldest := int64(1)<<62 - 1
	for v, b := range books {
		venues = append(venues, v)
		if b.TsRecv < oldest {
			oldest = b.TsRecv
		}
	}
	var best int64
	var bestQty core.Qty
	found := false
	for i := range venues {
		for j := range venues {
			if i == j {
				continue
			}
			a, b := books[venues[i]], books[venues[j]]
			ask, okA := a.BestYesAsk()
			bid, okB := b.BestYesBid()
			if okA && okB {
				if e := int64(bid.Px) - int64(ask.Px); !found || e > best {
					best, found = e, true
					bestQty = ask.Qty
					if bid.Qty < bestQty {
						bestQty = bid.Qty // fillable size is the shallower of the two tops
					}
				}
			}
		}
	}
	if !found {
		return 0, 0, 0, false
	}
	return float64(best) / 100, float64(bestQty) / 100, oldest, true
}

// Canonicals returns all canonical ids known to the manager.
func (m *Manager) Canonicals() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.venueIDByPair))
	for c := range m.venueIDByPair {
		out = append(out, c)
	}
	return out
}

// VenueID returns the venue-specific market id for a canonical market, e.g. to
// build an explicit subscription list for a venue's private stream.
func (m *Manager) VenueID(canonical, venue string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	id, ok := m.venueIDByPair[canonical][venue]
	return id, ok
}

// Canonical maps a venue's own market id back to the canonical market, without
// touching the stored books (unlike Update, which also records a snapshot).
// Used to attribute trade prints to a market.
func (m *Manager) Canonical(venue, venueID string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.canonByKey[key(venue, venueID)]
	return c, ok
}
