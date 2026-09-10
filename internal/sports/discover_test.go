package sports

import "testing"

func TestCanonTeam(t *testing.T) {
	cases := map[string]string{
		"Cincinnati": "CIN", "CIN": "CIN", "cin": "CIN",
		"Chicago WS": "CHW", "CWS": "CHW", "White Sox": "CHW",
		"A's": "OAK", "ATH": "OAK", "Athletics": "OAK",
		"New York Y": "NYY", "New York M": "NYM",
		"San Francisco": "SF", "St. Louis": "STL",
		"Nonexistent FC": "",
	}
	for in, want := range cases {
		if got := canonTeam("mlb", in); got != want {
			t.Errorf("canonTeam(mlb, %q) = %q; want %q", in, got, want)
		}
	}
	if canonTeam("cricket", "CIN") != "" {
		t.Error("unknown league should return empty")
	}
}

func TestCanonTeamNFL(t *testing.T) {
	cases := map[string]string{
		"KC": "KC", "kan": "KC", "Kansas City Chiefs": "KC", "chiefs": "KC",
		"WAS": "WAS", "WSH": "WAS", "Washington Commanders": "WAS",
		"LV": "LV", "LVR": "LV", "Raiders": "LV", "OAK": "LV",
		"JAX": "JAX", "JAC": "JAX", "GB": "GB", "GNB": "GB",
		"Los Angeles Rams": "LAR", "Los Angeles Chargers": "LAC",
		"New York G": "NYG", "New York J": "NYJ",
		"49ers": "SF", "SFO": "SF", "Cricket United": "",
	}
	for in, want := range cases {
		if got := canonTeam("nfl", in); got != want {
			t.Errorf("canonTeam(nfl, %q) = %q; want %q", in, got, want)
		}
	}
}

// An NFL game cross-lists once both venues' team forms canonicalize equally.
func TestMatchGamesNFL(t *testing.T) {
	kalshi := []venueGame{{league: "nfl", date: "2026-09-11", outcomes: map[string]string{
		"KC": "KXNFLGAME-...-KC", "PHI": "KXNFLGAME-...-PHI"}}}
	poly := []venueGame{{league: "nfl", date: "2026-09-11", outcomes: map[string]string{
		"KC": "aec-nfl-kc-phi-2026-09-11"}}}
	m := MatchGames(kalshi, poly)
	if len(m) != 1 || m[0].Canonical != "NFL-20260911-KC" {
		t.Fatalf("nfl match = %+v; want 1 match NFL-20260911-KC", m)
	}
}

func TestKalshiEventDate(t *testing.T) {
	if got := kalshiEventDate("KXMLBGAME", "KXMLBGAME-26AUG231335STLPHI"); got != "2026-08-23" {
		t.Errorf("date = %q; want 2026-08-23", got)
	}
	if got := kalshiEventDate("KXMLBGAME", "garbage"); got != "" {
		t.Errorf("bad ticker should give empty, got %q", got)
	}
}

func TestParsePolyEventSlug(t *testing.T) {
	date, away := parsePolyEventSlug("mlb", "mlb-tor-nyy-2026-08-23")
	if date != "2026-08-23" || away != "TOR" {
		t.Errorf("parse = %q %q; want 2026-08-23 TOR", date, away)
	}
	// A futures event slug (no home/away game shape) must not parse.
	if _, away := parsePolyEventSlug("mlb", "mlb-champ-2026-09-27"); away != "" {
		t.Errorf("futures slug parsed as game: away=%q", away)
	}
}

func TestMatchGames(t *testing.T) {
	kalshi := []venueGame{{league: "mlb", date: "2026-08-23", outcomes: map[string]string{
		"CIN": "KXMLBGAME-26AUG231335CINSF-CIN", "SF": "KXMLBGAME-26AUG231335CINSF-SF"}}}
	poly := []venueGame{{league: "mlb", date: "2026-08-23", outcomes: map[string]string{
		"CIN": "aec-mlb-cin-sf-2026-08-23"}}}

	m := MatchGames(kalshi, poly)
	if len(m) != 1 {
		t.Fatalf("want 1 match, got %d: %+v", len(m), m)
	}
	if m[0].Team != "CIN" || m[0].KalshiTicker == "" || m[0].PolySlug == "" {
		t.Errorf("bad match: %+v", m[0])
	}
	if m[0].Canonical != "MLB-20260823-CIN" {
		t.Errorf("canonical = %q; want MLB-20260823-CIN", m[0].Canonical)
	}
}

func TestMatchGamesNoOverlap(t *testing.T) {
	// Different dates -> no match.
	kalshi := []venueGame{{league: "mlb", date: "2026-08-23", outcomes: map[string]string{"CIN": "k"}}}
	poly := []venueGame{{league: "mlb", date: "2026-08-24", outcomes: map[string]string{"CIN": "p"}}}
	if m := MatchGames(kalshi, poly); len(m) != 0 {
		t.Errorf("expected no match across dates, got %+v", m)
	}
}

func TestCanonicalFromPolySlug(t *testing.T) {
	cases := []struct {
		slug, want string
		ok         bool
	}{
		{"aec-mlb-bos-mia-2026-08-25", "MLB-20260825-BOS", true}, // away=BOS is Poly's YES
		{"aec-mlb-col-wsh-2026-08-25", "MLB-20260825-COL", true},
		{"mlb-tb-det-2026-08-25", "MLB-20260825-TB", true}, // tolerate a missing aec- prefix
		{"aec-mlb-garbage", "", false},
	}
	for _, c := range cases {
		got, ok := CanonicalFromPolySlug("mlb", c.slug)
		if ok != c.ok || got != c.want {
			t.Errorf("CanonicalFromPolySlug(%q) = %q,%v; want %q,%v", c.slug, got, ok, c.want, c.ok)
		}
	}
}
