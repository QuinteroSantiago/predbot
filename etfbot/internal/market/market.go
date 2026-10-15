// Package market holds the types every broker, data source and strategy share,
// and the interfaces the rest of the bot is written against. Adapters (alpaca,
// yahoo) implement them; nothing here knows about any one venue.
package market

import (
	"context"
	"time"
)

// Bar is one trading day's close for a symbol. Close is adjusted for splits
// and dividends, so a ratio of two closes is a total return.
type Bar struct {
	Date  time.Time // the trading date, midnight UTC
	Close float64
}

// Date is a trading date: midnight UTC of the given calendar day.
func Date(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

// DayOf is the trading date t falls on in New York.
func DayOf(t time.Time) time.Time {
	y, m, d := t.In(NewYork).Date()
	return Date(y, m, d)
}

// NewYork is the exchange time zone.
var NewYork = mustZone("America/New_York")

func mustZone(name string) *time.Location {
	l, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return l
}

// Account is the broker account's cash and total value, in dollars.
type Account struct {
	Cash, Equity float64
}

// Position is a holding, in shares (fractional allowed).
type Position struct {
	Symbol string
	Qty    float64
}

// Side is buy or sell.
type Side int

const (
	Buy Side = iota
	Sell
)

func (s Side) String() string {
	if s == Sell {
		return "sell"
	}
	return "buy"
}

// Order is a market order. A buy is sized in dollars (Notional), a sell in
// shares (Qty), so a full exit sells exactly what is held.
type Order struct {
	Symbol   string
	Side     Side
	Qty      float64 // sells
	Notional float64 // buys, dollars
}

// OrderState is an order's progress at the broker.
type OrderState struct {
	ID        string
	Status    string // broker's status, e.g. "filled", "canceled"
	Done      bool   // no further fills will come
	FilledQty float64
	AvgPrice  float64
}

// Broker places orders and reports holdings.
type Broker interface {
	Account(ctx context.Context) (Account, error)
	Positions(ctx context.Context) ([]Position, error)
	Place(ctx context.Context, o Order) (id string, err error)
	Order(ctx context.Context, id string) (OrderState, error)
}

// Bars serves daily history, oldest first.
type Bars interface {
	Daily(ctx context.Context, symbols []string, from, to time.Time) (map[string][]Bar, error)
}

// Upto returns the bars dated on or before day. bars must be oldest first.
func Upto(bars []Bar, day time.Time) []Bar {
	lo, hi := 0, len(bars)
	for lo < hi {
		mid := (lo + hi) / 2
		if bars[mid].Date.After(day) {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return bars[:lo]
}
