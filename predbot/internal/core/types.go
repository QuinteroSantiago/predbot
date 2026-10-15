// Package core holds the venue-agnostic domain model for the prediction-market
// bot. It has NO I/O and NO venue knowledge (design §3 invariant 3): nothing in
// here may import another internal package. Everything a strategy sees is
// expressed on the YES instrument (design §6.3).
package core

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

// Price is a fixed-point price in units of $0.0001. $0.5500 == 5500.
// Valid range for a live quote is the open interval (0, OneDollar).
// We never use float64 for prices or quantities: both venues send decimal
// strings and Kalshi quotes to sub-penny precision (design §5).
type Price int64

// Qty is a fixed-point quantity in units of 0.01 contract. 300 contracts == 30000.
type Qty int64

// OneDollar is the price of a resolved-YES contract, and the axis around which
// NO quotes are derived: NoBid = OneDollar - YesAsk, NoAsk = OneDollar - YesBid.
const OneDollar Price = 10000

// Venue names: the Venue field of every quote, order and fill, and the key of
// every per-venue map. Compare and key against these, never a string literal —
// a typo in a literal compiles fine and silently misses the map.
const (
	VenueKalshi = "kalshi"
	VenuePoly   = "polymarketus"
)

const (
	pxScaleDigits  = 4 // Price granularity: $0.0001
	qtyScaleDigits = 2 // Qty granularity:   0.01 contract
	pxScale        = 10000
	qtyScale       = 100
)

var (
	// ErrPrecision is returned when a decimal string carries more fractional
	// digits than the venue/model supports (design §5: reject >4 dp prices).
	ErrPrecision = errors.New("core: value has more precision than supported")
	ErrEmpty     = errors.New("core: empty numeric string")
	ErrFormat    = errors.New("core: malformed numeric string")
)

// ParsePrice parses a decimal dollar string into fixed-point $0.0001 units,
// exactly and without float64. "0.0550" -> 550; "0.960" -> 9600; "1" -> 10000.
// It rejects inputs finer than 4 decimal places rather than silently rounding.
func ParsePrice(s string) (Price, error) {
	v, err := parseFixed(s, pxScaleDigits, pxScale)
	return Price(v), err
}

// ParseQty parses a decimal contract-count string into 0.01-contract units.
// "333.00" -> 33300. Rejects finer than 2 dp.
func ParseQty(s string) (Qty, error) {
	v, err := parseFixed(s, qtyScaleDigits, qtyScale)
	return Qty(v), err
}

// parseFixed converts a decimal string to an int64 scaled by 10^digits,
// exactly. It accepts an optional leading sign, an integer part, and up to
// `digits` fractional digits. More fractional digits than `digits` is an error
// (ErrPrecision) — we never round venue data.
func parseFixed(s string, digits int, scale int64) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, ErrEmpty
	}
	neg := false
	switch s[0] {
	case '+':
		s = s[1:]
	case '-':
		neg = true
		s = s[1:]
	}
	if s == "" {
		return 0, ErrFormat
	}

	intPart, fracPart := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, fracPart = s[:i], s[i+1:]
	}
	// Tolerate trailing-zero over-precision (venues format qty as "819.0000")
	// but reject genuine sub-unit precision (a nonzero digit past `digits`).
	if len(fracPart) > digits {
		for i := digits; i < len(fracPart); i++ {
			if fracPart[i] != '0' {
				return 0, fmt.Errorf("%w: %q", ErrPrecision, s)
			}
		}
		fracPart = fracPart[:digits]
	}
	// Empty int part (".5") is allowed; empty overall ("." or "") is not.
	if intPart == "" && fracPart == "" {
		return 0, ErrFormat
	}

	var whole int64
	for i := 0; i < len(intPart); i++ {
		c := intPart[i]
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("%w: %q", ErrFormat, s)
		}
		whole = whole*10 + int64(c-'0')
	}

	var frac int64
	for i := 0; i < digits; i++ {
		frac *= 10
		if i < len(fracPart) {
			c := fracPart[i]
			if c < '0' || c > '9' {
				return 0, fmt.Errorf("%w: %q", ErrFormat, s)
			}
			frac += int64(c - '0')
		}
	}

	v := whole*scale + frac
	if neg {
		v = -v
	}
	return v, nil
}

// String renders a Price as a decimal dollar string (for logs/journal).
func (p Price) String() string { return fmtFixed(int64(p), pxScaleDigits, pxScale) }

// String renders a Qty as a decimal contract string.
func (q Qty) String() string { return fmtFixed(int64(q), qtyScaleDigits, qtyScale) }

// Unit conversions. Price, fees and cash amounts are fixed-point $0.0001
// units; Qty is 0.01-contract units. Convert through these, never a bare
// /100 or *1e4: the two scales differ, and /100 means "to contracts" on a Qty
// but "to cents" on a Price.

// Contracts is q in whole contracts.
func (q Qty) Contracts() float64 { return float64(q) / qtyScale }

// QtyOf converts a contract count to Qty, rounded to the nearest 0.01.
func QtyOf(contracts float64) Qty { return Qty(math.Round(contracts * qtyScale)) }

// Dollars is p in dollars.
func (p Price) Dollars() float64 { return float64(p) / pxScale }

// Cents is p in cents.
func (p Price) Cents() float64 { return float64(p) / (pxScale / 100) }

// USD converts an amount in $0.0001 units (a fee, notional or P&L) to dollars.
func USD(v int64) float64 { return float64(v) / pxScale }

// FromUSD converts dollars to $0.0001 units, rounded to the nearest unit.
func FromUSD(dollars float64) int64 { return int64(math.Round(dollars * pxScale)) }

// FromCents converts cents to $0.0001 units, rounded to the nearest unit.
func FromCents(cents float64) int64 { return int64(math.Round(cents * (pxScale / 100))) }

// Notional is qty contracts at px, in $0.0001 units.
func Notional(px Price, qty Qty) int64 { return int64(px) * int64(qty) / qtyScale }

func fmtFixed(v int64, digits int, scale int64) string {
	sign := ""
	if v < 0 {
		sign, v = "-", -v
	}
	return fmt.Sprintf("%s%d.%0*d", sign, v/scale, digits, v%scale)
}

// MarketState is the venue-reported trading state of a market.
type MarketState uint8

const (
	StateUnknown MarketState = iota
	StateOpen
	StatePaused
	StateHalted
	StateClosed
)

func (s MarketState) String() string {
	switch s {
	case StateOpen:
		return "open"
	case StatePaused:
		return "paused"
	case StateHalted:
		return "halted"
	case StateClosed:
		return "closed"
	default:
		return "unknown"
	}
}

// Level is one price level of the book.
type Level struct {
	Px  Price
	Qty Qty
}

// BookSnapshot is the venue-agnostic view of one market on one venue, expressed
// entirely on the YES instrument. Both venues are single-instrument underneath
// (design §6.3) so this representation is exact, not an approximation.
//
// Snapshots handed out by the ingesters are immutable copies: the ingester owns
// the authoritative raw book and never shares a mutable pointer (design §3
// invariant 1).
type BookSnapshot struct {
	Venue    string      `json:"venue"`
	VenueID  string      `json:"venue_id"`  // Kalshi market_ticker / Polymarket US market slug
	YesBids  []Level     `json:"yes_bids"`  // descending px
	YesAsks  []Level     `json:"yes_asks"`  // ascending px
	TsServer int64       `json:"ts_server"` // venue timestamp, unix nanos, 0 if absent
	TsRecv   int64       `json:"ts_recv"`   // our receive time, unix nanos
	Seq      uint64      `json:"seq"`       // Kalshi seq; 0 for Polymarket
	State    MarketState `json:"state"`
}

// Side is the internal order side. There is deliberately no "Buy NO": buying NO
// is expressed as SellYes everywhere, which removes a whole class of sign bugs
// and matches how both venues actually work (design §5).
type Side uint8

const (
	BuyYes Side = iota
	SellYes
)

func (s Side) String() string {
	if s == SellYes {
		return "sell_yes"
	}
	return "buy_yes"
}

// TIF is time-in-force for an order. IOC for taking legs; GTC/GTD for resting.
type TIF uint8

const (
	IOC TIF = iota
	GTC
	GTD
	FOK
)

// Intent is a proposed order emitted by a strategy. Strategy proposes; it never
// places (design §3 invariant 2). Phase 1 does not emit intents; the type lives
// here so the whole model compiles as one unit.
type Intent struct {
	PairID  string // groups the two legs of an arb; "" for single-leg
	Leg     int    // 0 or 1
	Venue   string
	VenueID string
	Side    Side
	Px      Price // limit price on the YES instrument
	Qty     Qty
	TIF     TIF
	Reason  string
}

// Order is an Intent that has been assigned an idempotency key. Retries
// reconcile against ClientID rather than resubmitting (design §5, §11).
type Order struct {
	Intent
	ClientID string // Kalshi: client_order_id. Polymarket US: [VERIFY field name]
}

// Fill is a (partial) execution reported by a venue's private stream.
type Fill struct {
	OrderClientID string
	Venue         string
	Px            Price
	Qty           Qty
	Fee           int64 // signed, $0.0001 units; negative == maker rebate
	IsMaker       bool
	TsFilled      int64
}
