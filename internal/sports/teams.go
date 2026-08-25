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
	"ari": "ARI", "arizona": "ARI", "diamondbacks": "ARI", "dbacks": "ARI",
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

var canonByLeague = map[string]map[string]string{
	"mlb": mlbCanon,
}

// canonTeam normalizes a raw team code or name to its canonical abbreviation for
// the league. Returns "" if the league is unknown or the team can't be matched.
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
