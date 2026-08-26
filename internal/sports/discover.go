package sports

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
}

// venueGame is one game as seen on a single venue: canonical team -> the venue's
// market id for that team's win.
type venueGame struct {
	league   string
	date     string
	outcomes map[string]string
}

// Client discovers games from the venues' public APIs.
type Client struct {
	HTTP       *http.Client
	KalshiBase string // https://api.elections.kalshi.com/trade-api/v2
	PolyBase   string // https://gateway.polymarket.us
}

func New() *Client {
	return &Client{
		HTTP:       &http.Client{Timeout: 15 * time.Second},
		KalshiBase: "https://api.elections.kalshi.com/trade-api/v2",
		PolyBase:   "https://gateway.polymarket.us",
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

var leagueSeries = map[string]string{"mlb": "KXMLBGAME", "nfl": "KXNFLGAME", "nba": "KXNBAGAME"}

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
		g := venueGame{league: league, date: date, outcomes: map[string]string{}}
		for _, m := range e.Markets {
			team := canonTeam(league, m.YesSubTitle)
			if team == "" {
				team = canonTeam(league, kalshiMarketSuffix(m.Ticker))
			}
			if team != "" {
				g.outcomes[team] = m.Ticker
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
			team := canonTeam(league, m.YesSubTitle)
			if team == "" {
				team = canonTeam(league, kalshiMarketSuffix(m.Ticker))
			}
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
				Slug             string `json:"slug"`
				SportsMarketType string `json:"sportsMarketType"`
			} `json:"markets"`
		} `json:"events"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	var games []venueGame
	for _, e := range resp.Events {
		date, away := parsePolyEventSlug(league, e.Slug)
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
			})
			break
		}
	}
	return games, nil
}

// parsePolyEventSlug parses "mlb-tor-nyy-2026-08-23" -> (date, canonical away team).
// parts[1] is the away team, parts[2] the home team, last three the date.
func parsePolyEventSlug(league, slug string) (date, away string) {
	parts := strings.Split(slug, "-")
	if len(parts) < 6 || parts[0] != league {
		return "", ""
	}
	date = strings.Join(parts[len(parts)-3:], "-")
	if _, err := time.Parse("2006-01-02", date); err != nil {
		return "", ""
	}
	return date, canonTeam(league, parts[1])
}

// ReconstructFromIDs rebuilds cross-listed game matches from venue ids that
// appear in recorded data (Kalshi tickers, Poly slugs), instead of a live API.
// This lets the backtester map the sports snapshots it replays — sports markets
// are discovered at runtime and never live in markets.yaml, so without this the
// backtest would drop them.
func ReconstructFromIDs(kalshiTickers, polySlugs []string) []Match {
	type gameKey struct{ league, date string }
	kBy := map[gameKey]map[string]string{}
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
			team := canonTeam(league, t[i+1:])
			if date == "" || team == "" {
				continue
			}
			k := gameKey{league, date}
			if kBy[k] == nil {
				kBy[k] = map[string]string{}
			}
			kBy[k][team] = t
		}
	}
	pBy := map[gameKey]map[string]string{}
	for _, s := range polySlugs {
		rest, ok := strings.CutPrefix(s, "aec-") // aec-mlb-tor-nyy-2026-08-23
		if !ok {
			continue
		}
		league, _, found := strings.Cut(rest, "-")
		if !found {
			continue
		}
		date, away := parsePolyEventSlug(league, rest)
		if away == "" {
			continue
		}
		k := gameKey{league, date}
		if pBy[k] == nil {
			pBy[k] = map[string]string{}
		}
		pBy[k][away] = s
	}
	var kg, pg []venueGame
	for k, o := range kBy {
		kg = append(kg, venueGame{league: k.league, date: k.date, outcomes: o})
	}
	for k, o := range pBy {
		pg = append(pg, venueGame{league: k.league, date: k.date, outcomes: o})
	}
	return MatchGames(kg, pg)
}

// MatchGames pairs Kalshi and Poly games by (league, date, canonical team) and
// returns one Match per shared outcome.
func MatchGames(kalshi, poly []venueGame) []Match {
	type key struct{ league, date, team string }
	pIdx := map[key]string{}
	for _, g := range poly {
		for team, slug := range g.outcomes {
			pIdx[key{g.league, g.date, team}] = slug
		}
	}
	var out []Match
	for _, g := range kalshi {
		for team, ticker := range g.outcomes {
			if slug, ok := pIdx[key{g.league, g.date, team}]; ok {
				out = append(out, Match{
					Canonical:    fmt.Sprintf("%s-%s-%s", strings.ToUpper(g.league), strings.ReplaceAll(g.date, "-", ""), team),
					League:       g.league,
					Date:         g.date,
					Team:         team,
					KalshiTicker: ticker,
					PolySlug:     slug,
				})
			}
		}
	}
	return out
}
