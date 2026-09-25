package sports

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"predbot/internal/config"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Match is one cross-listed game outcome: the same "team X wins" contract on
// both venues, ready to become a canonical market in the recorder.
type Match struct {
	Canonical    string
	League       string
	Date         string // YYYY-MM-DD
	Team         string // canonical abbreviation
	KalshiTicker string
	PolySlug     string
	Start        time.Time // scheduled start, from Poly; zero if unknown
}

// venueGame is one game as seen on a single venue: canonical team -> the venue's
// market id for that team's win, and canonical team -> its opponent.
type venueGame struct {
	league   string
	date     string
	outcomes map[string]string
	opp      map[string]string
	start    time.Time
}

// Client discovers games from the venues' public APIs.
type Client struct {
	HTTP       *http.Client
	KalshiBase string // https://api.elections.kalshi.com/trade-api/v2
	PolyBase   string // https://gateway.polymarket.us
}

// discoverHTTPTimeout bounds one venue listing request during discovery.
const discoverHTTPTimeout = 15 * time.Second

func New() *Client {
	return &Client{
		HTTP:       &http.Client{Timeout: discoverHTTPTimeout},
		KalshiBase: config.KalshiAPI,
		PolyBase:   config.PolyGateway,
	}
}

func (c *Client) get(ctx context.Context, url string) ([]byte, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0") // Poly WAF rejects default UA
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET %s: %d", url, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// leagueSeries maps a league (Poly's slug prefix, e.g. "cfb" for college
// football) to its Kalshi game series.
var leagueSeries = map[string]string{
	"mlb":  "KXMLBGAME",
	"nfl":  "KXNFLGAME",
	"nba":  "KXNBAGAME",
	"wnba": "KXWNBAGAME",
	"cfb":  "KXNCAAFGAME",
	"nhl":  "KXNHLGAME",
}

// DiscoverKalshi returns today's open games for a league. Fully live-verifiable.
func (c *Client) DiscoverKalshi(ctx context.Context, league string) ([]venueGame, error) {
	series, ok := leagueSeries[league]
	if !ok {
		return nil, fmt.Errorf("unknown league %q", league)
	}
	body, err := c.get(ctx, fmt.Sprintf("%s/events?series_ticker=%s&status=open&with_nested_markets=true&limit=200", c.KalshiBase, series))
	if err != nil {
		return nil, err
	}
	var resp struct {
		Events []struct {
			EventTicker string `json:"event_ticker"`
			Markets     []struct {
				Ticker      string `json:"ticker"`
				YesSubTitle string `json:"yes_sub_title"`
			} `json:"markets"`
		} `json:"events"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	var games []venueGame
	for _, e := range resp.Events {
		date := kalshiEventDate(series, e.EventTicker)
		if date == "" {
			continue
		}
		g := venueGame{league: league, date: date, outcomes: map[string]string{}, opp: map[string]string{}}
		for _, m := range e.Markets {
			team := kalshiTeam(league, m.YesSubTitle, m.Ticker)
			if team != "" {
				g.outcomes[team] = m.Ticker
				g.opp[team] = kalshiOpponent(league, series, m.Ticker)
			}
		}
		if len(g.outcomes) > 0 {
			games = append(games, g)
		}
	}
	return games, nil
}

// kalshiEventDate parses the date from an event ticker like
// KXMLBGAME-26AUG231335STLPHI -> 2026-08-23.
func kalshiEventDate(series, ticker string) string {
	rest := strings.TrimPrefix(ticker, series+"-")
	if len(rest) < 7 {
		return ""
	}
	yy, mon, dd := rest[0:2], rest[2:5], rest[5:7]
	months := map[string]string{"JAN": "01", "FEB": "02", "MAR": "03", "APR": "04", "MAY": "05", "JUN": "06", "JUL": "07", "AUG": "08", "SEP": "09", "OCT": "10", "NOV": "11", "DEC": "12"}
	mm, ok := months[mon]
	if !ok {
		return ""
	}
	return "20" + yy + "-" + mm + "-" + dd
}

// kalshiOpponent is the canonical opponent of a Kalshi game market's team, read
// from the event ticker: KXMLBGAME-26AUG231335STLPHI-STL -> PHI. The event part
// is the away code then the home code; if the team's code could be either end
// (SDSU at SD: "SDSUSD" for SD), it's ambiguous and returns "".
func kalshiOpponent(league, series, ticker string) string {
	i := strings.LastIndexByte(ticker, '-')
	rest := strings.TrimPrefix(ticker[:max(i, 0)], series+"-")
	if i < 0 || len(rest) < 7 {
		return ""
	}
	teams := strings.TrimLeft(rest[7:], "0123456789") // drop date, then the start time if any
	code := ticker[i+1:]
	isAway, isHome := strings.HasPrefix(teams, code), strings.HasSuffix(teams, code)
	var opp string
	switch {
	case isAway && isHome:
		return ""
	case isAway:
		opp = teams[len(code):]
	case isHome:
		opp = teams[:len(teams)-len(code)]
	}
	if opp == "" {
		return ""
	}
	return kalshiTeam(league, "", "-"+opp)
}

func kalshiMarketSuffix(ticker string) string {
	if i := strings.LastIndexByte(ticker, '-'); i >= 0 {
		return ticker[i+1:]
	}
	return ""
}

// SettledResult is one finished Kalshi market's outcome, ready to be stored.
// Canonical is built the same way MatchGames builds it, so a recorded outcome
// joins to the book/price history the recorder captured for that game.
type SettledResult struct {
	Canonical string // e.g. MLB-20260825-DET
	Ticker    string // Kalshi market ticker
	Result    string // "yes" | "no"
	Status    string // venue status, e.g. "finalized"
	CloseTime string
}

// SettledKalshi returns finished games' per-team results for a league in ONE
// call. The settled-events endpoint returns nested markets that already carry
// `result` inline, so no per-ticker fetch is needed — the whole point the
// outcome recorder was missing: it discovered only status=open games (which are
// never settled), so it recorded nothing. This asks for status=settled instead.
func (c *Client) SettledKalshi(ctx context.Context, league string) ([]SettledResult, error) {
	series, ok := leagueSeries[league]
	if !ok {
		return nil, fmt.Errorf("unknown league %q", league)
	}
	body, err := c.get(ctx, fmt.Sprintf("%s/events?series_ticker=%s&status=settled&with_nested_markets=true&limit=200", c.KalshiBase, series))
	if err != nil {
		return nil, err
	}
	var resp struct {
		Events []struct {
			EventTicker string `json:"event_ticker"`
			Markets     []struct {
				Ticker      string `json:"ticker"`
				Status      string `json:"status"`
				Result      string `json:"result"`
				YesSubTitle string `json:"yes_sub_title"`
				CloseTime   string `json:"close_time"`
			} `json:"markets"`
		} `json:"events"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	var out []SettledResult
	for _, e := range resp.Events {
		date := kalshiEventDate(series, e.EventTicker)
		if date == "" {
			continue
		}
		for _, m := range e.Markets {
			if m.Result != "yes" && m.Result != "no" {
				continue // not actually resolved
			}
			team := kalshiTeam(league, m.YesSubTitle, m.Ticker)
			if team == "" {
				continue
			}
			out = append(out, SettledResult{
				Canonical: fmt.Sprintf("%s-%s-%s", strings.ToUpper(league), strings.ReplaceAll(date, "-", ""), team),
				Ticker:    m.Ticker, Result: m.Result, Status: m.Status, CloseTime: m.CloseTime,
			})
		}
	}
	return out, nil
}

// CanonicalFromPolySlug maps a recorded Poly moneyline market slug
// (e.g. "aec-mlb-bos-mia-2026-08-25") to its canonical (MLB-20260825-BOS), the
// same id MatchGames/SettledKalshi produce. Used to attach a real Poly slug to a
// settled game's outcome from RECORDED data — where Kalshi and Poly dates already
// agreed at record time — rather than live Poly discovery, whose forward-looking
// listing has rolled a game off by the time Kalshi marks it settled.
func CanonicalFromPolySlug(league, marketSlug string) (string, bool) {
	// Moneyline market slugs are the event slug with an "aec-" prefix.
	ev := strings.TrimPrefix(marketSlug, "aec-")
	date, away := parsePolyEventSlug(league, ev)
	if away == "" {
		return "", false
	}
	return fmt.Sprintf("%s-%s-%s", strings.ToUpper(league), strings.ReplaceAll(date, "-", ""), away), true
}

// DiscoverPoly returns today's open daily games for a league from the v2 Sports
// API (docs.polymarket.us — the v1 markets listing does NOT contain them). Each
// event's moneyline is the market whose sportsMarketType ends in
// "full_game_winner" (slug "aec-<eventslug>"); its single binary YES instrument
// pays the AWAY team (first in the slug), verified live 2026-08-23 (e.g.
// aec-mlb-tor-nyy-2026-08-23, YES=Toronto, 34 bids / 39 offers). We record and
// match on that moneyline market.
func (c *Client) DiscoverPoly(ctx context.Context, league string) ([]venueGame, error) {
	body, err := c.get(ctx, fmt.Sprintf("%s/v2/leagues/%s/events?type=sport&limit=200", c.PolyBase, league))
	if err != nil {
		return nil, err
	}
	var resp struct {
		Events []struct {
			Slug    string `json:"slug"`
			Markets []struct {
				Slug             string    `json:"slug"`
				SportsMarketType string    `json:"sportsMarketType"`
				GameStartTime    time.Time `json:"gameStartTime"`
			} `json:"markets"`
		} `json:"events"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	var games []venueGame
	for _, e := range resp.Events {
		date, away, home := parsePolyGame(league, e.Slug)
		if away == "" {
			continue
		}
		for _, m := range e.Markets {
			if !strings.HasSuffix(m.SportsMarketType, "full_game_winner") {
				continue
			}
			games = append(games, venueGame{
				league: league, date: date,
				outcomes: map[string]string{away: m.Slug}, // YES = away team
				opp:      map[string]string{away: home},
				start:    m.GameStartTime,
			})
			break
		}
	}
	return games, nil
}

// GameState is a game's live status from Poly's sports feed.
type GameState struct {
	Live, Ended bool
	Period      string // Poly's period label, e.g. "NS", "P2", "OT", "FT"
	// PastRegulation is set once any period outside regulation has been
	// played — overtime, a shootout, extra innings — or the game has ended.
	PastRegulation bool
}

// PolyGameStates returns each listed game's live state for a league, keyed by
// the same canonical id MatchGames produces (the away team's).
func (c *Client) PolyGameStates(ctx context.Context, league string) (map[string]GameState, error) {
	body, err := c.get(ctx, fmt.Sprintf("%s/v2/leagues/%s/events?type=sport&limit=200", c.PolyBase, league))
	if err != nil {
		return nil, err
	}
	var resp struct {
		Events []struct {
			Slug       string `json:"slug"`
			Live       bool   `json:"live"`
			Ended      bool   `json:"ended"`
			Period     string `json:"period"`
			EventState struct {
				PeriodScores []struct {
					Type string `json:"type"`
				} `json:"periodScores"`
			} `json:"eventState"`
		} `json:"events"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	out := map[string]GameState{}
	for _, e := range resp.Events {
		date, away := parsePolyEventSlug(league, e.Slug)
		if away == "" {
			continue
		}
		st := GameState{Live: e.Live, Ended: e.Ended, Period: e.Period}
		st.PastRegulation = e.Ended || pastRegulation(e.Period)
		for _, ps := range e.EventState.PeriodScores {
			if ps.Type != "" && ps.Type != "PERIOD_SCORE_TYPE_REGULATION" {
				st.PastRegulation = true
			}
		}
		out[fmt.Sprintf("%s-%s-%s", strings.ToUpper(league), strings.ReplaceAll(date, "-", ""), away)] = st
	}
	return out, nil
}

// otLabel matches overtime period labels: OT, 2OT, OT2, F/OT, F/SO, SO.
var otLabel = regexp.MustCompile(`^(F/)?(\d*OT\d*|SO)$`)

// pastRegulation recognizes Poly period labels for overtime, shootouts and
// finals, in case a state arrives before its periodScores entry does. Labels
// are matched whole: a substring test would read baseball's "BOT 5" as OT.
// Extra innings and the rest are caught by periodScores' type instead.
func pastRegulation(period string) bool {
	p := strings.ToUpper(strings.TrimSpace(period))
	return p == "FT" || p == "SHOOTOUT" || otLabel.MatchString(p)
}

// PolySlugs lists a league's open moneyline market slugs on Poly — the ids the
// trader subscribes to. For tools that need a realistic set of slugs.
func (c *Client) PolySlugs(ctx context.Context, league string) ([]string, error) {
	games, err := c.DiscoverPoly(ctx, league)
	if err != nil {
		return nil, err
	}
	var slugs []string
	for _, g := range games {
		for _, slug := range g.outcomes {
			slugs = append(slugs, slug)
		}
	}
	return slugs, nil
}

// parsePolyEventSlug parses "mlb-tor-nyy-2026-08-23" -> (date, canonical away team).
func parsePolyEventSlug(league, slug string) (date, away string) {
	date, away, _ = parsePolyGame(league, slug)
	return date, away
}

// parsePolyGame parses "mlb-tor-nyy-2026-08-23" -> (date, away, home), teams
// canonical. parts[1] is the away team, parts[2] the home team, last three the
// date. home is "" if unrecognized.
func parsePolyGame(league, slug string) (date, away, home string) {
	parts := strings.Split(slug, "-")
	if len(parts) < 6 || parts[0] != league {
		return "", "", ""
	}
	date = strings.Join(parts[len(parts)-3:], "-")
	if _, err := time.Parse("2006-01-02", date); err != nil {
		return "", "", ""
	}
	return date, polyTeam(league, parts[1]), polyTeam(league, parts[2])
}

// ReconstructFromIDs rebuilds cross-listed game matches from venue ids that
// appear in recorded data (Kalshi tickers, Poly slugs), instead of a live API.
// This lets the backtester map the sports snapshots it replays — sports markets
// are discovered at runtime and never live in markets.yaml, so without this the
// backtest would drop them.
func ReconstructFromIDs(kalshiTickers, polySlugs []string) []Match {
	var kg, pg []venueGame
	for _, t := range kalshiTickers {
		for league, series := range leagueSeries {
			if !strings.HasPrefix(t, series+"-") {
				continue
			}
			i := strings.LastIndexByte(t, '-')
			if i < 0 {
				continue
			}
			date := kalshiEventDate(series, t[:i]) // strip "-<TEAM>" to get the event ticker
			team := kalshiTeam(league, "", t)
			if date == "" || team == "" {
				continue
			}
			kg = append(kg, venueGame{league: league, date: date,
				outcomes: map[string]string{team: t}, opp: map[string]string{team: kalshiOpponent(league, series, t)}})
		}
	}
	for _, s := range polySlugs {
		rest, ok := strings.CutPrefix(s, "aec-") // aec-mlb-tor-nyy-2026-08-23
		if !ok {
			continue
		}
		league, _, found := strings.Cut(rest, "-")
		if !found {
			continue
		}
		date, away, home := parsePolyGame(league, rest)
		if away == "" {
			continue
		}
		pg = append(pg, venueGame{league: league, date: date,
			outcomes: map[string]string{away: s}, opp: map[string]string{away: home}})
	}
	return MatchGames(kg, pg)
}

// MatchGames pairs Kalshi and Poly games by (league, date, canonical team,
// opponent) and returns one Match per shared outcome. The opponent keeps a team
// playing twice in a day (split squads, doubleheaders) from pairing across two
// different games; a key that still repeats on either venue (a doubleheader
// against the same opponent) is ambiguous and skipped, as is an outcome whose
// opponent is unknown.
func MatchGames(kalshi, poly []venueGame) []Match {
	type key struct{ league, date, team, opp string }
	start := map[key]time.Time{}
	index := func(games []venueGame) (map[key]string, map[key]int) {
		ids, n := map[key]string{}, map[key]int{}
		for _, g := range games {
			for team, id := range g.outcomes {
				if g.opp[team] == "" {
					continue
				}
				k := key{g.league, g.date, team, g.opp[team]}
				ids[k] = id
				n[k]++
				if !g.start.IsZero() {
					start[k] = g.start
				}
			}
		}
		return ids, n
	}
	kIDs, kN := index(kalshi)
	pIDs, pN := index(poly)
	var out []Match
	for k, ticker := range kIDs {
		slug, ok := pIDs[k]
		if !ok || kN[k] != 1 || pN[k] != 1 {
			continue
		}
		out = append(out, Match{
			Canonical:    fmt.Sprintf("%s-%s-%s", strings.ToUpper(k.league), strings.ReplaceAll(k.date, "-", ""), k.team),
			League:       k.league,
			Date:         k.date,
			Team:         k.team,
			KalshiTicker: ticker,
			PolySlug:     slug,
			Start:        start[k],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Canonical < out[j].Canonical })
	return out
}
