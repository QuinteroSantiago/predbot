package config

import "testing"

// Venue APIs report positions by their own market id; the risk engine keys by
// canonical market. Adoption is only possible if that mapping exists.
func TestCanonicalByVenueID(t *testing.T) {
	var m Market
	m.Canonical = "TECH-GROK5-RELEASE-2026"
	m.Venues.Kalshi.Ticker = "KXGROK-GROK5-26OCT01"
	m.Venues.PolymarketUS.Slug = "aimc-xai-grok-5-2026-09-30"

	got := CanonicalByVenueID([]Market{m})
	for _, id := range []string{"KXGROK-GROK5-26OCT01", "aimc-xai-grok-5-2026-09-30"} {
		if got[id] != "TECH-GROK5-RELEASE-2026" {
			t.Errorf("%s -> %q; want the canonical market", id, got[id])
		}
	}
}
