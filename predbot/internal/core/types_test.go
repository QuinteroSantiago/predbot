package core

import "testing"

func TestParsePrice(t *testing.T) {
	cases := []struct {
		in   string
		want Price
		ok   bool
	}{
		{"0.0550", 550, true},
		{"0.960", 9600, true},
		{"0.9600", 9600, true},
		{"1", 10000, true},
		{"1.0000", 10000, true},
		{".5", 5000, true},
		{"0.0001", 1, true},
		{"0", 0, true},
		{"0.00005", 0, false}, // 5 dp -> reject, never round (design §5)
		{"abc", 0, false},
		{"", 0, false},
		{"1.2.3", 0, false},
	}
	for _, c := range cases {
		got, err := ParsePrice(c.in)
		if c.ok && (err != nil || got != c.want) {
			t.Errorf("ParsePrice(%q) = %d, %v; want %d", c.in, got, err, c.want)
		}
		if !c.ok && err == nil {
			t.Errorf("ParsePrice(%q) = %d; want error", c.in, got)
		}
	}
}

func TestParseQty(t *testing.T) {
	cases := []struct {
		in   string
		want Qty
		ok   bool
	}{
		{"333.00", 33300, true},
		{"819.0000", 81900, true}, // Poly US formats qty to 4dp; trailing zeros ok
		{"1", 100, true},
		{"0.5", 50, true},
		{"0.01", 1, true},
		{"0.001", 0, false},   // genuine sub-0.01 precision -> reject
		{"0.0100", 1, true},   // trailing zeros past scale -> ok
	}
	for _, c := range cases {
		got, err := ParseQty(c.in)
		if c.ok && (err != nil || got != c.want) {
			t.Errorf("ParseQty(%q) = %d, %v; want %d", c.in, got, err, c.want)
		}
		if !c.ok && err == nil {
			t.Errorf("ParseQty(%q) = %d; want error", c.in, got)
		}
	}
}

func TestPriceRoundTrip(t *testing.T) {
	// Parsing the string form of a price must recover the price exactly.
	for _, p := range []Price{0, 1, 550, 5000, 9600, 10000} {
		got, err := ParsePrice(p.String())
		if err != nil || got != p {
			t.Errorf("round trip %d -> %q -> %d, %v", p, p.String(), got, err)
		}
	}
}

func TestQuoteDerivation(t *testing.T) {
	// YES asks at 0.60 (qty 300 contracts) => NO bids at 0.40 (qty 300).
	// YES bids at 0.55 (qty 200)           => NO asks at 0.45 (qty 200).
	b := &BookSnapshot{
		YesBids: []Level{{Px: 5500, Qty: 20000}},
		YesAsks: []Level{{Px: 6000, Qty: 30000}},
	}
	if nb, ok := b.BestNoBid(); !ok || nb.Px != 4000 || nb.Qty != 30000 {
		t.Errorf("BestNoBid = %+v, %v; want {4000 30000}", nb, ok)
	}
	if na, ok := b.BestNoAsk(); !ok || na.Px != 4500 || na.Qty != 20000 {
		t.Errorf("BestNoAsk = %+v, %v; want {4500 20000}", na, ok)
	}
}

func TestQuoteDerivationEmpty(t *testing.T) {
	b := &BookSnapshot{}
	if _, ok := b.BestYesBid(); ok {
		t.Error("empty book reported a YES bid")
	}
	if _, ok := b.BestNoBid(); ok {
		t.Error("empty book reported a NO bid")
	}
}

func TestCrossed(t *testing.T) {
	crossed := &BookSnapshot{
		YesBids: []Level{{Px: 6000, Qty: 100}},
		YesAsks: []Level{{Px: 5900, Qty: 100}},
	}
	if !crossed.Crossed() {
		t.Error("expected crossed book")
	}
	normal := &BookSnapshot{
		YesBids: []Level{{Px: 5900, Qty: 100}},
		YesAsks: []Level{{Px: 6000, Qty: 100}},
	}
	if normal.Crossed() {
		t.Error("normal book reported crossed")
	}
}

func TestUnitConversions(t *testing.T) {
	if got := Qty(2250).Contracts(); got != 22.5 {
		t.Errorf("Contracts = %v; want 22.5", got)
	}
	if got := QtyOf(0.29); got != 29 { // 0.29*100 = 28.999…: truncating gave 28
		t.Errorf("QtyOf(0.29) = %d; want 29", got)
	}
	if got := Price(3700).Dollars(); got != 0.37 {
		t.Errorf("Dollars = %v; want 0.37", got)
	}
	if got := Price(3700).Cents(); got != 37 {
		t.Errorf("Cents = %v; want 37", got)
	}
	if got := FromUSD(83.02); got != 830200 { // truncating gave 830199
		t.Errorf("FromUSD(83.02) = %d; want 830200", got)
	}
	if got := FromCents(1.5); got != 150 {
		t.Errorf("FromCents(1.5) = %d; want 150", got)
	}
	if got := USD(45918); got != 4.5918 {
		t.Errorf("USD = %v; want 4.5918", got)
	}
	if got := Notional(3700, 2200); got != 81400 { // 22 contracts at $0.37 = $8.14
		t.Errorf("Notional = %d; want 81400", got)
	}
}
