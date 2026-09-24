// Package sports discovers cross-listed daily sports games on Kalshi and
// Polymarket US at runtime and matches them into canonical markets, so the
// recorder can subscribe to games that appear and expire each day (dynamic
// subscription) instead of a static markets.yaml.
//
// The hard part is matching the SAME game across venues: identifiers and team
// codes differ (Kalshi CWS/ATH vs standard CHW/OAK), so team names are
// normalized to a canonical abbreviation via the tables here.
package sports

import "strings"

// mlbCanon maps the many forms a team appears as (Kalshi codes, standard
// abbreviations, city names) to a single canonical abbreviation. Keys are
// lower-cased. Add NFL/NBA tables the same way when those seasons matter.
var mlbCanon = map[string]string{
	"ari": "ARI", "az": "ARI", "arizona": "ARI", "diamondbacks": "ARI", "dbacks": "ARI",
	"atl": "ATL", "atlanta": "ATL", "braves": "ATL",
	"bal": "BAL", "baltimore": "BAL", "orioles": "BAL",
	"bos": "BOS", "boston": "BOS", "red sox": "BOS",
	"chc": "CHC", "cubs": "CHC", "chicago c": "CHC",
	"cws": "CHW", "chw": "CHW", "chicago ws": "CHW", "white sox": "CHW",
	"cin": "CIN", "cincinnati": "CIN", "reds": "CIN",
	"cle": "CLE", "cleveland": "CLE", "guardians": "CLE",
	"col": "COL", "colorado": "COL", "rockies": "COL",
	"det": "DET", "detroit": "DET", "tigers": "DET",
	"hou": "HOU", "houston": "HOU", "astros": "HOU",
	"kc": "KC", "kcr": "KC", "kansas city": "KC", "royals": "KC",
	"laa": "LAA", "angels": "LAA", "los angeles a": "LAA",
	"lad": "LAD", "dodgers": "LAD", "los angeles d": "LAD",
	"mia": "MIA", "miami": "MIA", "marlins": "MIA",
	"mil": "MIL", "milwaukee": "MIL", "brewers": "MIL",
	"min": "MIN", "minnesota": "MIN", "twins": "MIN",
	"nym": "NYM", "mets": "NYM", "new york m": "NYM",
	"nyy": "NYY", "yankees": "NYY", "new york y": "NYY",
	"ath": "OAK", "oak": "OAK", "athletics": "OAK", "a's": "OAK", "as": "OAK",
	"phi": "PHI", "philadelphia": "PHI", "phillies": "PHI",
	"pit": "PIT", "pittsburgh": "PIT", "pirates": "PIT",
	"sd": "SD", "sdp": "SD", "san diego": "SD", "padres": "SD",
	"sf": "SF", "sfg": "SF", "san francisco": "SF", "giants": "SF",
	"sea": "SEA", "seattle": "SEA", "mariners": "SEA",
	"stl": "STL", "st. louis": "STL", "st louis": "STL", "cardinals": "STL",
	"tb": "TB", "tbr": "TB", "tampa bay": "TB", "rays": "TB",
	"tex": "TEX", "texas": "TEX", "rangers": "TEX",
	"tor": "TOR", "toronto": "TOR", "blue jays": "TOR",
	"wsh": "WSH", "was": "WSH", "washington": "WSH", "nationals": "WSH",
}

// nflCanon maps NFL team forms (Kalshi codes/subtitles, Poly slug codes, city
// names, nicknames) to a canonical abbreviation. Covers the abbreviation
// variants the two venues are known to disagree on (WAS/WSH, LV/LVR, JAX/JAC,
// GB/GNB, KC/KAN, NE/NWE, NO/NOR, SF/SFO, TB/TAM). Verify against live tickers
// when NFL games first post — Kalshi has used oddball codes before (CWS/ATH in
// MLB), so a first-night check is warranted.
var nflCanon = map[string]string{
	"ari": "ARI", "arz": "ARI", "arizona": "ARI", "cardinals": "ARI",
	"atl": "ATL", "atlanta": "ATL", "falcons": "ATL",
	"bal": "BAL", "baltimore": "BAL", "ravens": "BAL",
	"buf": "BUF", "buffalo": "BUF", "bills": "BUF",
	"car": "CAR", "carolina": "CAR", "panthers": "CAR",
	"chi": "CHI", "chicago": "CHI", "bears": "CHI",
	"cin": "CIN", "cincinnati": "CIN", "bengals": "CIN",
	"cle": "CLE", "cleveland": "CLE", "browns": "CLE",
	"dal": "DAL", "dallas": "DAL", "cowboys": "DAL",
	"den": "DEN", "denver": "DEN", "broncos": "DEN",
	"det": "DET", "detroit": "DET", "lions": "DET",
	"gb": "GB", "gnb": "GB", "green bay": "GB", "packers": "GB",
	"hou": "HOU", "houston": "HOU", "texans": "HOU",
	"ind": "IND", "indianapolis": "IND", "colts": "IND",
	"jax": "JAX", "jac": "JAX", "jacksonville": "JAX", "jaguars": "JAX",
	"kc": "KC", "kan": "KC", "kansas city": "KC", "chiefs": "KC",
	"lv": "LV", "lvr": "LV", "las": "LV", "las vegas": "LV", "raiders": "LV", "oak": "LV", "oakland": "LV",
	"lac": "LAC", "chargers": "LAC", "los angeles c": "LAC", "la chargers": "LAC",
	"lar": "LAR", "rams": "LAR", "los angeles r": "LAR", "la rams": "LAR",
	"mia": "MIA", "miami": "MIA", "dolphins": "MIA",
	"min": "MIN", "minnesota": "MIN", "vikings": "MIN",
	"ne": "NE", "nwe": "NE", "new england": "NE", "patriots": "NE", "pats": "NE",
	"no": "NO", "nor": "NO", "new orleans": "NO", "saints": "NO",
	"nyg": "NYG", "giants": "NYG", "new york g": "NYG", "ny giants": "NYG",
	"nyj": "NYJ", "jets": "NYJ", "new york j": "NYJ", "ny jets": "NYJ",
	"phi": "PHI", "philadelphia": "PHI", "eagles": "PHI",
	"pit": "PIT", "pittsburgh": "PIT", "steelers": "PIT",
	"sf": "SF", "sfo": "SF", "san francisco": "SF", "49ers": "SF", "niners": "SF",
	"sea": "SEA", "seattle": "SEA", "seahawks": "SEA",
	"tb": "TB", "tam": "TB", "tampa bay": "TB", "buccaneers": "TB", "bucs": "TB",
	"ten": "TEN", "tennessee": "TEN", "titans": "TEN",
	"was": "WAS", "wsh": "WAS", "washington": "WAS", "commanders": "WAS",
}

// wnbaCanon maps WNBA team forms to a canonical abbreviation. The 2026
// playoff teams were checked live on 2026-09-24: Kalshi codes CONN/GS/LA and
// Poly slug codes conn/gsv/la. The rest (ATL/DAL/NY/SEA/PDX) use Kalshi's codes
// from its full market history; their Poly codes are guesses, so a mismatch on
// those teams just means that game doesn't cross-list (it can't mispair).
var wnbaCanon = map[string]string{
	"atl": "ATL", "atlanta": "ATL", "dream": "ATL",
	"chi": "CHI", "chicago": "CHI", "sky": "CHI",
	"conn": "CONN", "con": "CONN", "ct": "CONN", "connecticut": "CONN", "sun": "CONN",
	"dal": "DAL", "dallas": "DAL", "wings": "DAL",
	"gs": "GS", "gsv": "GS", "golden state": "GS", "valkyries": "GS",
	"ind": "IND", "indiana": "IND", "fever": "IND",
	"la": "LA", "los angeles": "LA", "sparks": "LA",
	"lv": "LV", "lva": "LV", "las vegas": "LV", "aces": "LV",
	"min": "MIN", "minnesota": "MIN", "lynx": "MIN",
	"ny": "NY", "nyl": "NY", "new york": "NY", "liberty": "NY",
	"pdx": "PDX", "por": "PDX", "portland": "PDX", "fire": "PDX",
	"phx": "PHX", "pho": "PHX", "phoenix": "PHX", "mercury": "PHX",
	"sea": "SEA", "seattle": "SEA", "storm": "SEA",
	"tor": "TOR", "toronto": "TOR", "tempo": "TOR",
	"wsh": "WSH", "was": "WSH", "washington": "WSH", "mystics": "WSH",
}

var canonByLeague = map[string]map[string]string{
	"mlb":  mlbCanon,
	"nfl":  nflCanon,
	"wnba": wnbaCanon,
}

// canonTeam normalizes a raw team code or name to its canonical abbreviation for
// the league. Returns "" if the league is unknown or the team can't be matched.
// CanonTeam maps a team code or name (any source's spelling) to the code used
// in canonical market ids, or "" if unknown. MLB's Stats API writes Arizona as
// "AZ" and the Athletics as "ATH"; both resolve here.
func CanonTeam(league, raw string) string { return canonTeam(league, raw) }

// kalshiTeam is the canonical team for a Kalshi game market, from its
// yes_sub_title and ticker. College football skips the alias tables: codes
// collide across its ~260 schools, so its canonical is Kalshi's own ticker
// suffix and only the Poly side is translated (cfbPolyToKalshi).
func kalshiTeam(league, subtitle, ticker string) string {
	if league == "cfb" {
		return kalshiMarketSuffix(ticker)
	}
	if team := canonTeam(league, subtitle); team != "" {
		return team
	}
	return canonTeam(league, kalshiMarketSuffix(ticker))
}

// polyTeam is the canonical team for a Poly slug team code.
func polyTeam(league, code string) string {
	if league == "cfb" {
		return cfbPolyToKalshi[code]
	}
	return canonTeam(league, code)
}

func canonTeam(league, raw string) string {
	table, ok := canonByLeague[strings.ToLower(league)]
	if !ok {
		return ""
	}
	key := strings.TrimSpace(strings.ToLower(raw))
	if c, ok := table[key]; ok {
		return c
	}
	// Fall back to a suffix/prefix contains-match on the full name (e.g.
	// "New York Yankees" -> matches "yankees").
	for alias, canon := range table {
		if len(alias) >= 4 && strings.Contains(key, alias) {
			return canon
		}
	}
	return ""
}
