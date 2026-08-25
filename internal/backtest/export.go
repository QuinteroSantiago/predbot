package backtest

import (
	"encoding/json"
	"os"
	"strings"
	"time"

	"predbot/internal/core"
	"predbot/internal/strategy"
)

// Opportunity is one detected cross-venue arb, in human-friendly units, for the
// visualization. Prices in cents (0–100), money in USD, qty in contracts.
type Opportunity struct {
	TsMs          int64   `json:"ts_ms"`
	ISO           string  `json:"iso"`
	Canonical     string  `json:"canonical"`
	Category      string  `json:"category"`
	BuyVenue      string  `json:"buy_venue"`
	SellVenue     string  `json:"sell_venue"`
	BuyCents      float64 `json:"buy_cents"`
	SellCents     float64 `json:"sell_cents"`
	EdgeCents     float64 `json:"edge_cents"` // net edge per contract
	Qty           float64 `json:"qty"`        // contracts
	GrossUSD      float64 `json:"gross_usd"`
	FeesUSD       float64 `json:"fees_usd"`
	NetUSD        float64 `json:"net_usd"`
	CapitalUSD    float64 `json:"capital_usd"`
	AnnualizedPct float64 `json:"annualized_pct"`
	HoldDays      float64 `json:"hold_days"`
}

// BuildOpportunity computes the economics of a detected pair for the export.
func BuildOpportunity(buy, sell core.Intent, fees strategy.FeeModel, tsNs int64, resolve time.Time) Opportunity {
	contracts := float64(buy.Qty) / 100
	gross := float64(int64(sell.Px)-int64(buy.Px)) / 1e4 * contracts
	feeBuy := float64(fees.Fee(buy.Venue, buy.Px, buy.Qty, false)) / 1e4
	feeSell := float64(fees.Fee(sell.Venue, sell.Px, buy.Qty, false)) / 1e4
	fee := feeBuy + feeSell
	net := gross - fee
	capital := float64(buy.Px)/1e4*contracts + float64(int64(core.OneDollar)-int64(sell.Px))/1e4*contracts

	var ann, days float64
	if !resolve.IsZero() {
		days = resolve.Sub(time.Unix(0, tsNs)).Hours() / 24
		if days < 1 {
			days = 1
		}
		if capital > 0 {
			ann = net / capital * 365 / days * 100
		}
	}
	edgeCents := 0.0
	if contracts > 0 {
		edgeCents = net / contracts * 100
	}
	return Opportunity{
		TsMs:          tsNs / 1e6,
		ISO:           time.Unix(0, tsNs).UTC().Format(time.RFC3339),
		Canonical:     buy.VenueID + "", // overwritten by caller with canonical
		Category:      "",
		BuyVenue:      buy.Venue,
		SellVenue:     sell.Venue,
		BuyCents:      float64(buy.Px) / 100,
		SellCents:     float64(sell.Px) / 100,
		EdgeCents:     edgeCents,
		Qty:           contracts,
		GrossUSD:      gross,
		FeesUSD:       fee,
		NetUSD:        net,
		CapitalUSD:    capital,
		AnnualizedPct: ann,
		HoldDays:      days,
	}
}

// Category maps a canonical id to a human category for grouping/coloring.
func Category(canonical string) string {
	switch {
	case strings.HasPrefix(canonical, "BTC-"):
		return "Crypto"
	case strings.HasPrefix(canonical, "EMMY-"):
		return "Culture"
	case strings.HasPrefix(canonical, "FED-"):
		return "Macro"
	case strings.HasPrefix(canonical, "TECH-"):
		return "Tech"
	case strings.HasPrefix(canonical, "HOUSE-"), strings.HasPrefix(canonical, "SENATE-"),
		strings.HasPrefix(canonical, "GOV-"):
		return "Elections"
	case strings.HasPrefix(canonical, "MLB-"), strings.HasPrefix(canonical, "NFL-"),
		strings.HasPrefix(canonical, "NBA-"):
		return "Sports"
	default:
		return "Other"
	}
}

// SpreadPoint is a sampled cross-venue gross edge (best direction) for one
// market at one time — the "near-miss surface" showing how close a market came
// to an arb versus the fee hurdle. Gross (pre-fee) in cents; can be negative.
type SpreadPoint struct {
	TsMs           int64   `json:"ts_ms"`
	Canonical      string  `json:"canonical"`
	Category       string  `json:"category"`
	GrossEdgeCents float64 `json:"gross_edge_cents"`
}

// CrossGrossCents returns the best cross-venue gross edge (max over directions
// of best-bid − best-ask, in cents); ok=false if a side is missing.
func CrossGrossCents(books map[string]core.BookSnapshot) (float64, bool) {
	venues := make([]string, 0, len(books))
	for v := range books {
		venues = append(venues, v)
	}
	var best int64
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
				e := int64(bid.Px) - int64(ask.Px)
				if !found || e > best {
					best, found = e, true
				}
			}
		}
	}
	return float64(best) / 100, found
}

// WriteSpread dumps sampled spread points as JSON.
func WriteSpread(path string, pts []SpreadPoint) error {
	b, err := json.Marshal(pts)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// WriteOpportunities dumps the detected opportunities as JSON.
func WriteOpportunities(path string, opps []Opportunity) error {
	b, err := json.MarshalIndent(opps, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
