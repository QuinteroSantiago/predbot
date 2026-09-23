package backtest

import (
	"sort"
	"time"
)

// RunReport is one backtest run's results as JSON: what the backtest runs page
// (dashboard/backtests) reads. Money is in USD; rates are fractions (0.05 = 5%).
// Bump RunReportSchema on any breaking change to the shape.
type RunReport struct {
	Schema        int             `json:"schema"`
	Run           RunMeta         `json:"run"`
	Report        ReportSummary   `json:"report"`
	Maker         *MakerSummary   `json:"maker,omitempty"`
	Adverse       *AdverseResult  `json:"adverse,omitempty"` // -adverse runs
	Opportunities int             `json:"opportunities"`     // gap-open episodes
	Pairs         []PairOutcome   `json:"pairs"`             // entry order: the equity curve
	Markets       []MarketOutcome `json:"markets"`           // net P&L, best first
}

// RunReportSchema is the RunReport shape version.
const RunReportSchema = 1

// AnalysisReport is a read-only analysis run (-leadlag, -longshot, -moves) as JSON.
// Run.Kind names the analysis and which result field is set.
type AnalysisReport struct {
	Schema     int               `json:"schema"`
	Run        RunMeta           `json:"run"`
	LeadLag    *LeadLagResult    `json:"leadlag,omitempty"`
	Longshot   *LongshotResult   `json:"longshot,omitempty"`
	Moves      *MoveResult       `json:"moves,omitempty"`
	SpreadMR   *SpreadMRResult   `json:"spreadmr,omitempty"`
	Autocorr   *AutocorrResult   `json:"autocorr,omitempty"`
	OutcomeReg *OutcomeRegResult `json:"outcomereg,omitempty"`
	FairValue  *FairValueResult  `json:"fairvalue,omitempty"`
}

// RunMeta describes how a run was made.
type RunMeta struct {
	ID           string     `json:"id"`    // unique, sortable: <started UTC>-<mode>
	Kind         string     `json:"kind"`  // "trading", "leadlag", "longshot", "moves", "spreadmr", "autocorr", "outcomereg" or "fairvalue"
	Label        string     `json:"label"` // free text, e.g. "full archive"
	Mode         string     `json:"mode"`  // trading: "maker" or "taker"; analyses: same as Kind
	Adverse      bool       `json:"adverse"`
	WindowHours  int        `json:"window_hours"`   // 0 = whole archive
	From         *time.Time `json:"from,omitempty"` // requested range (-from/-to), if any
	To           *time.Time `json:"to,omitempty"`
	Variant      string     `json:"variant,omitempty"` // sweep variant within Label
	Params       RunParams  `json:"params"`
	DataFrom     time.Time  `json:"data_from"` // first and last replayed snapshot
	DataTo       time.Time  `json:"data_to"`
	Snapshots    int        `json:"snapshots"`
	Markets      int        `json:"markets"`
	StartedAt    time.Time  `json:"started_at"`
	FinishedAt   time.Time  `json:"finished_at"`
	DurationS    float64    `json:"duration_s"`
	Host         string     `json:"host"`
	Revision     string     `json:"revision,omitempty"` // git commit the binary was built from
	Dirty        bool       `json:"dirty,omitempty"`    // built with uncommitted changes
	Shards       int        `json:"shards"`
	ParseWorkers int        `json:"parse_workers"`
	Args         []string   `json:"args"`
}

// RunParams are the effective strategy and sim settings (config plus any
// command-line overrides), in human units.
type RunParams struct {
	LatencyMS         int     `json:"latency_ms"`
	MinEdgeCents      float64 `json:"min_edge_cents"`
	MinDepthContracts float64 `json:"min_depth_contracts"` // 0 = no liquidity floor
	MaxClipContracts  float64 `json:"max_clip_contracts"`
	KalshiFeeMult     float64 `json:"kalshi_fee_mult"`
	MaxSkewMS         int64   `json:"max_skew_ms"`
	MaxBookAgeMS      int64   `json:"max_book_age_ms"`
}

// ReportSummary is Report in JSON units.
type ReportSummary struct {
	GrossUSD         float64            `json:"gross_usd"`
	FeesUSD          float64            `json:"fees_usd"` // total; negative = net rebate
	FeesByVenueUSD   map[string]float64 `json:"fees_by_venue_usd"`
	SlippageUSD      float64            `json:"slippage_usd"`
	LeggingCostUSD   float64            `json:"legging_cost_usd"`
	NetPnLUSD        float64            `json:"net_pnl_usd"`
	CapitalLockedUSD float64            `json:"capital_locked_usd"`
	PairsAttempted   int                `json:"pairs_attempted"`
	PairsCompleted   int                `json:"pairs_completed"`
	PairsLegged      int                `json:"pairs_legged"`
	PairsFaded       int                `json:"pairs_faded"`
	IntraCrossings   int                `json:"intra_crossings"`
	Annualized       float64            `json:"annualized"`
	ExcessAnnualized float64            `json:"excess_annualized"`
	IdleYield        float64            `json:"idle_yield"`
	AvgHoldDays      float64            `json:"avg_hold_days"`
	SharpePerTrade   float64            `json:"sharpe_per_trade"`
	WinRate          float64            `json:"win_rate"`
	MaxDrawdownUSD   float64            `json:"max_drawdown_usd"`
	SettledPairs     int                `json:"settled_pairs"` // pairs behind the derived stats
}

// MakerSummary is the maker-mode rest accounting.
type MakerSummary struct {
	RestsPlaced int `json:"rests_placed"`
	RestsFilled int `json:"rests_filled"`
}

// PairOutcome is one settled pair.
type PairOutcome struct {
	Canonical  string  `json:"canonical"`
	EntryMs    int64   `json:"entry_ms"`
	NetUSD     float64 `json:"net_usd"`
	CapitalUSD float64 `json:"capital_usd"`
}

// MarketOutcome totals one market.
type MarketOutcome struct {
	Canonical  string  `json:"canonical"`
	Category   string  `json:"category"`
	Attempts   int     `json:"attempts"` // submissions, as "opportunities by market" in the text report
	Pairs      int     `json:"pairs"`
	NetUSD     float64 `json:"net_usd"`
	CapitalUSD float64 `json:"capital_usd"`
}

func usd(v int64) float64 { return float64(v) / float64(1e4) }

// NewRunReport builds the JSON report from a finalized TradingResult.
func NewRunReport(meta RunMeta, res *TradingResult, maker bool) RunReport {
	r := res.Report
	sum := ReportSummary{
		GrossUSD:         usd(r.GrossCaptured),
		FeesByVenueUSD:   map[string]float64{},
		SlippageUSD:      usd(r.Slippage),
		LeggingCostUSD:   usd(r.LeggingCost),
		NetPnLUSD:        usd(r.NetPnL),
		CapitalLockedUSD: usd(r.CapitalLocked),
		PairsAttempted:   r.PairsAttempted,
		PairsCompleted:   r.PairsCompleted,
		PairsLegged:      r.PairsLegged,
		PairsFaded:       r.PairsFaded,
		IntraCrossings:   r.IntraCrossings,
		Annualized:       r.Annualized,
		ExcessAnnualized: r.ExcessAnnualized,
		IdleYield:        r.IdleYield,
		AvgHoldDays:      r.AvgHoldDays,
		SharpePerTrade:   r.SharpePerTrade,
		WinRate:          r.WinRate,
		MaxDrawdownUSD:   usd(r.MaxDrawdown),
		SettledPairs:     len(r.pairs),
	}
	var fees int64
	for v, f := range r.FeesByVenue {
		sum.FeesByVenueUSD[v] = usd(f)
		fees += f
	}
	sum.FeesUSD = usd(fees)

	out := RunReport{
		Schema:        RunReportSchema,
		Run:           meta,
		Report:        sum,
		Opportunities: len(res.Opps),
		Pairs:         make([]PairOutcome, 0, len(r.pairs)),
	}
	if maker {
		out.Maker = &MakerSummary{RestsPlaced: res.RestsPlaced, RestsFilled: res.RestsFilled}
	}

	byMarket := map[string]*MarketOutcome{}
	market := func(c string) *MarketOutcome {
		m, ok := byMarket[c]
		if !ok {
			m = &MarketOutcome{Canonical: c, Category: Category(c)}
			byMarket[c] = m
		}
		return m
	}
	for c, n := range r.byCanonical {
		market(c).Attempts = n
	}
	// Finalize has already put pairs in entry order.
	for _, p := range r.pairs {
		out.Pairs = append(out.Pairs, PairOutcome{
			Canonical: p.canonical, EntryMs: p.entryTs / 1e6,
			NetUSD: usd(p.net), CapitalUSD: usd(p.capital),
		})
		m := market(p.canonical)
		m.Pairs++
		m.NetUSD += usd(p.net)
		m.CapitalUSD += usd(p.capital)
	}
	out.Markets = make([]MarketOutcome, 0, len(byMarket))
	for _, m := range byMarket {
		out.Markets = append(out.Markets, *m)
	}
	sort.Slice(out.Markets, func(i, j int) bool {
		a, b := out.Markets[i], out.Markets[j]
		if a.NetUSD != b.NetUSD {
			return a.NetUSD > b.NetUSD
		}
		return a.Canonical < b.Canonical
	})
	return out
}
