package sports

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

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
		"KC": "KXNFLGAME-...-KC", "PHI": "KXNFLGAME-...-PHI"}, opp: map[string]string{"KC": "PHI", "PHI": "KC"}}}
	poly := []venueGame{{league: "nfl", date: "2026-09-11", outcomes: map[string]string{
		"KC": "aec-nfl-kc-phi-2026-09-11"}, opp: map[string]string{"KC": "PHI"}}}
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
		"CIN": "KXMLBGAME-26AUG231335CINSF-CIN", "SF": "KXMLBGAME-26AUG231335CINSF-SF"}, opp: map[string]string{"CIN": "SF", "SF": "CIN"}}}
	poly := []venueGame{{league: "mlb", date: "2026-08-23", outcomes: map[string]string{
		"CIN": "aec-mlb-cin-sf-2026-08-23"}, opp: map[string]string{"CIN": "SF"}}}

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
	kalshi := []venueGame{{league: "mlb", date: "2026-08-23", outcomes: map[string]string{"CIN": "k"}, opp: map[string]string{"CIN": "SF"}}}
	poly := []venueGame{{league: "mlb", date: "2026-08-24", outcomes: map[string]string{"CIN": "p"}, opp: map[string]string{"CIN": "SF"}}}
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

// WNBA ids as both venues listed the 2026-09-24 playoff slate. Kalshi and Poly
// disagree on Golden State (GS vs gsv); both must land on the same canonical,
// and Poly's YES (the away team, first in the slug) must pair with that team's
// Kalshi market, never the opponent's.
func TestReconstructWNBA(t *testing.T) {
	kalshi := []string{
		"KXWNBAGAME-26SEP24TORCONN-CONN", "KXWNBAGAME-26SEP24TORCONN-TOR",
		"KXWNBAGAME-26SEP24GSLA-LA", "KXWNBAGAME-26SEP24GSLA-GS",
	}
	poly := []string{"aec-wnba-tor-conn-2026-09-24", "aec-wnba-gsv-la-2026-09-24"}
	got := map[string]Match{}
	for _, m := range ReconstructFromIDs(kalshi, poly) {
		got[m.Canonical] = m
	}
	want := map[string][2]string{
		"WNBA-20260924-TOR": {"KXWNBAGAME-26SEP24TORCONN-TOR", "aec-wnba-tor-conn-2026-09-24"},
		"WNBA-20260924-GS":  {"KXWNBAGAME-26SEP24GSLA-GS", "aec-wnba-gsv-la-2026-09-24"},
	}
	if len(got) != len(want) {
		t.Fatalf("matches = %+v; want %d", got, len(want))
	}
	for c, ids := range want {
		if m := got[c]; m.KalshiTicker != ids[0] || m.PolySlug != ids[1] {
			t.Errorf("%s = %s / %s; want %s / %s", c, m.KalshiTicker, m.PolySlug, ids[0], ids[1])
		}
	}
}

func TestCanonTeamWNBA(t *testing.T) {
	cases := map[string]string{
		"CONN": "CONN", "Connecticut": "CONN", "GS": "GS", "gsv": "GS", "Golden State": "GS",
		"LA": "LA", "Los Angeles": "LA", "LV": "LV", "Las Vegas": "LV",
		"New York": "NY", "PDX": "PDX", "Portland": "PDX",
		"Team Coop": "", "Team Spoon": "", // All-Star game sides
	}
	for in, want := range cases {
		if got := canonTeam("wnba", in); got != want {
			t.Errorf("canonTeam(wnba, %q) = %q; want %q", in, got, want)
		}
	}
}

// College football codes collide across venues: Poly "sdst" is San Diego State
// (Kalshi SDSU) while Kalshi "SDST" is South Dakota State (Poly "sdkst"). Each
// venue's code must resolve through its own side, and the two games must not
// cross.
func TestReconstructCFBCollidingCodes(t *testing.T) {
	kalshi := []string{
		"KXNCAAFGAME-26SEP26SDSUTOL-SDSU", "KXNCAAFGAME-26SEP26SDSUTOL-TOL",
		"KXNCAAFGAME-26SEP26EIUSDST-EIU", "KXNCAAFGAME-26SEP26EIUSDST-SDST",
	}
	poly := []string{"aec-cfb-sdst-toledo-2026-09-26", "aec-cfb-eill-sdkst-2026-09-26"}
	got := map[string]Match{}
	for _, m := range ReconstructFromIDs(kalshi, poly) {
		got[m.Canonical] = m
	}
	if m := got["CFB-20260926-SDSU"]; m.KalshiTicker != "KXNCAAFGAME-26SEP26SDSUTOL-SDSU" || m.PolySlug != "aec-cfb-sdst-toledo-2026-09-26" {
		t.Errorf("San Diego State = %+v", m)
	}
	if _, ok := got["CFB-20260926-SDST"]; ok {
		t.Errorf("South Dakota State (home, not Poly's YES) matched: %+v", got)
	}
	if polyTeam("cfb", "sdkst") != "SDST" || polyTeam("cfb", "sdst") != "SDSU" {
		t.Error("cfb Poly codes must resolve per venue")
	}
	if polyTeam("cfb", "nosuchschool") != "" {
		t.Error("unknown cfb code must not match")
	}
}

// NHL: Poly's own codes (veg, nas, mon, was) must land on Kalshi's.
func TestCanonTeamNHL(t *testing.T) {
	cases := map[string]string{
		"veg": "VGK", "VGK": "VGK", "Vegas": "VGK", "nas": "NSH", "Nashville": "NSH",
		"mon": "MTL", "Montreal": "MTL", "was": "WSH", "WSH": "WSH",
		"New York I": "NYI", "New York R": "NYR", "nj": "NJ", "St. Louis": "STL",
		"Hartford Whalers": "",
	}
	for in, want := range cases {
		if got := canonTeam("nhl", in); got != want {
			t.Errorf("canonTeam(nhl, %q) = %q; want %q", in, got, want)
		}
	}
	m := ReconstructFromIDs(
		[]string{"KXNHLGAME-26SEP26SJVGK-VGK", "KXNHLGAME-26SEP26SJVGK-SJ"},
		[]string{"aec-nhl-sj-veg-2026-09-26"})
	if len(m) != 1 || m[0].Canonical != "NHL-20260926-SJ" || m[0].KalshiTicker != "KXNHLGAME-26SEP26SJVGK-SJ" {
		t.Fatalf("nhl match = %+v; want NHL-20260926-SJ on the SJ ticker", m)
	}
}

func TestKalshiOpponent(t *testing.T) {
	cases := []struct{ league, series, ticker, want string }{
		{"mlb", "KXMLBGAME", "KXMLBGAME-26AUG231335STLPHI-STL", "PHI"},
		{"mlb", "KXMLBGAME", "KXMLBGAME-26AUG231335STLPHI-PHI", "STL"},
		{"mlb", "KXMLBGAME", "KXMLBGAME-26AUG231335CWSATH-ATH", "CHW"}, // canonicalized
		{"wnba", "KXWNBAGAME", "KXWNBAGAME-26SEP24GSLA-LA", "GS"},
		{"cfb", "KXNCAAFGAME", "KXNCAAFGAME-26SEP24LIBCCAR-LIB", "CCAR"},
		{"cfb", "KXNCAAFGAME", "KXNCAAFGAME-26SEP26SDSUSD-SDSU", "SD"},
		{"cfb", "KXNCAAFGAME", "KXNCAAFGAME-26SEP26SDSUSD-SD", ""}, // SD is both ends: ambiguous
		{"mlb", "KXMLBGAME", "KXMLBGAME-26AUG231335STLPHI-NYY", ""},
		{"mlb", "KXMLBGAME", "garbage", ""},
	}
	for _, c := range cases {
		if got := kalshiOpponent(c.league, c.series, c.ticker); got != c.want {
			t.Errorf("kalshiOpponent(%s) = %q; want %q", c.ticker, got, c.want)
		}
	}
}

// NHL split squads: Montreal plays Ottawa and Boston on the same date. Poly
// lists only MTL-at-OTT; Kalshi only MTL-at-BOS. Keyed on (date, team) alone
// those paired across two different games.
func TestMatchGamesSplitSquad(t *testing.T) {
	m := ReconstructFromIDs(
		[]string{"KXNHLGAME-26SEP26MTLBOS-MTL", "KXNHLGAME-26SEP26MTLBOS-BOS"},
		[]string{"aec-nhl-mon-ott-2026-09-26"})
	if len(m) != 0 {
		t.Fatalf("split-squad games paired across different opponents: %+v", m)
	}
}

// A doubleheader against the same opponent is two games with the same key on
// Kalshi (the start time tells them apart there, but not on Poly), so it's
// ambiguous and must be skipped rather than paired twice.
func TestMatchGamesDoubleheaderSkipped(t *testing.T) {
	m := ReconstructFromIDs(
		[]string{"KXMLBGAME-26AUG231305CINSF-CIN", "KXMLBGAME-26AUG231835CINSF-CIN"},
		[]string{"aec-mlb-cin-sf-2026-08-23"})
	if len(m) != 0 {
		t.Fatalf("doubleheader paired: %+v", m)
	}
}

// The scheduled start rides on Poly's side of the match (Kalshi's listing has
// no reliable start), so the live loop can price time-to-start.
func TestMatchGamesCarriesStart(t *testing.T) {
	st := time.Date(2026, 9, 24, 23, 0, 0, 0, time.UTC)
	kalshi := []venueGame{{league: "wnba", date: "2026-09-24", outcomes: map[string]string{"TOR": "k"}, opp: map[string]string{"TOR": "CONN"}}}
	poly := []venueGame{{league: "wnba", date: "2026-09-24", outcomes: map[string]string{"TOR": "p"}, opp: map[string]string{"TOR": "CONN"}, start: st}}
	m := MatchGames(kalshi, poly)
	if len(m) != 1 || !m[0].Start.Equal(st) {
		t.Fatalf("match = %+v; want start %v", m, st)
	}
}

func TestLiveCoversEveryLeague(t *testing.T) {
	for league := range leagueSeries {
		if c := strings.ToUpper(league) + "-20260924-XYZ"; !Live(c) {
			t.Errorf("Live(%q) = false; a traded league's entries would never age out", c)
		}
	}
	if Live("FED-DEC-CUT") || Live("MLB") {
		t.Error("non-sports canonical reported live")
	}
}

func TestPastRegulationLabels(t *testing.T) {
	for _, p := range []string{"OT", "2OT", "OT2", "SO", "F/OT", "F/SO", "FT", "Shootout", "ot"} {
		if !pastRegulation(p) {
			t.Errorf("%q should read as past regulation", p)
		}
	}
	for _, p := range []string{"", "NS", "P1", "P3", "Q4", "HT", "BOT 5", "TOP 9", "B9", "2nd", "SOON"} {
		if pastRegulation(p) {
			t.Errorf("%q must not read as past regulation", p)
		}
	}
}

// The live cutoff keys on the same canonical MatchGames produces, and an
// OVERTIME period score (as Poly recorded for CGY@SEA, 2026-09-24) marks the
// game past regulation even while the label still reads a regulation period.
func TestPolyGameStatesOvertime(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"events":[
		 {"slug":"nhl-cgy-sea-2026-09-24","live":true,"period":"P3","eventState":{"periodScores":[
		   {"type":"PERIOD_SCORE_TYPE_REGULATION"},{"type":"PERIOD_SCORE_TYPE_OVERTIME"}]}},
		 {"slug":"nhl-bos-phi-2026-09-24","live":true,"period":"P2","eventState":{"periodScores":[
		   {"type":"PERIOD_SCORE_TYPE_REGULATION"}]}}]}`)
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), PolyBase: srv.URL}
	st, err := c.PolyGameStates(context.Background(), "nhl")
	if err != nil {
		t.Fatal(err)
	}
	if !st["NHL-20260924-CGY"].PastRegulation {
		t.Error("CGY@SEA with an overtime period score must be past regulation")
	}
	if g, ok := st["NHL-20260924-BOS"]; !ok || g.PastRegulation || !g.Live {
		t.Errorf("BOS@PHI in P2 = %+v; want live, in regulation", g)
	}
}
