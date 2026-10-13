package domain

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func minute(base time.Time, n int, open, high, low, close, volume string) Candle {
	return Candle{
		Start: base.Add(time.Duration(n) * time.Minute),
		Open:  d(open), High: d(high), Low: d(low), Close: d(close), Volume: d(volume),
	}
}

func TestAggregate(t *testing.T) {
	base := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	candles := []Candle{
		// bewust niet op volgorde, en minuut 3 ontbreekt
		minute(base, 1, "101", "105", "100", "104", "2"),
		minute(base, 0, "100", "102", "99", "101", "1"),
		minute(base, 9, "104", "106", "98", "103", "3"),
		minute(base, 10, "103", "110", "103", "109", "1"), // volgende bucket
		minute(base, 21, "109", "109", "108", "108", "1"), // bucket nog niet afgesloten
	}

	got := Aggregate(candles, 10*time.Minute, base.Add(25*time.Minute))
	if len(got) != 2 {
		t.Fatalf("verwacht 2 afgesloten candles, kreeg %d", len(got))
	}

	first := got[0]
	if !first.Start.Equal(base) {
		t.Errorf("start = %s, verwacht %s", first.Start, base)
	}
	checks := map[string][2]decimal.Decimal{
		"open":   {first.Open, d("100")},
		"high":   {first.High, d("106")},
		"low":    {first.Low, d("98")},
		"close":  {first.Close, d("103")},
		"volume": {first.Volume, d("6")},
	}
	for name, c := range checks {
		if !c[0].Equal(c[1]) {
			t.Errorf("%s = %s, verwacht %s", name, c[0], c[1])
		}
	}
	if !got[1].Start.Equal(base.Add(10*time.Minute)) || !got[1].Close.Equal(d("109")) {
		t.Errorf("tweede candle onjuist: %+v", got[1])
	}
}

func TestAggregateBucketClosesExactlyAtEnd(t *testing.T) {
	base := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	candles := []Candle{minute(base, 9, "1", "1", "1", "1", "1")}

	if got := Aggregate(candles, 10*time.Minute, base.Add(10*time.Minute-time.Second)); len(got) != 0 {
		t.Errorf("bucket mag nog niet afgesloten zijn, kreeg %d", len(got))
	}
	if got := Aggregate(candles, 10*time.Minute, base.Add(10*time.Minute)); len(got) != 1 {
		t.Errorf("bucket moet afgesloten zijn, kreeg %d", len(got))
	}
}

func TestAggregateEmpty(t *testing.T) {
	if got := Aggregate(nil, 10*time.Minute, time.Now()); got != nil {
		t.Errorf("verwacht nil, kreeg %v", got)
	}
}

func TestCloses(t *testing.T) {
	base := time.Now()
	got := Closes([]Candle{minute(base, 0, "1", "1", "1", "5", "1"), minute(base, 1, "1", "1", "1", "6", "1")})
	if len(got) != 2 || !got[0].Equal(d("5")) || !got[1].Equal(d("6")) {
		t.Errorf("Closes = %v", got)
	}
}

func btcEur() Product {
	return Product{ID: "BTC-EUR", BaseIncrement: d("0.00000001"), QuoteIncrement: d("0.01"), BaseMinSize: d("0.00000001"), QuoteMinSize: d("1")}
}

func TestProductRounding(t *testing.T) {
	p := btcEur()
	tests := []struct {
		name string
		got  decimal.Decimal
		want string
	}{
		{"size naar beneden", p.RoundSizeDown(d("0.123456789")), "0.12345678"},
		{"prijs naar beneden", p.RoundPriceDown(d("60000.019")), "60000.01"},
		{"prijs naar boven", p.RoundPriceUp(d("60000.011")), "60000.02"},
		{"prijs al afgerond", p.RoundPriceUp(d("60000.01")), "60000.01"},
	}
	for _, tt := range tests {
		if !tt.got.Equal(d(tt.want)) {
			t.Errorf("%s: %s, verwacht %s", tt.name, tt.got, tt.want)
		}
	}
}

func TestProductIsTradable(t *testing.T) {
	p := btcEur()
	tests := []struct {
		size, price string
		want        bool
	}{
		{"0.001", "60000", true},
		{"0.00001", "60000", false}, // 0,60 EUR < minimum 1 EUR
		{"0", "60000", false},
		{"-1", "60000", false},
	}
	for _, tt := range tests {
		if got := p.IsTradable(d(tt.size), d(tt.price)); got != tt.want {
			t.Errorf("IsTradable(%s, %s) = %v, verwacht %v", tt.size, tt.price, got, tt.want)
		}
	}
}

func TestProductValidate(t *testing.T) {
	if err := btcEur().Validate(); err != nil {
		t.Errorf("geldig product afgekeurd: %v", err)
	}
	invalid := []Product{
		{},
		{ID: "BTC-EUR", QuoteIncrement: d("0.01")},
		{ID: "BTC-EUR", BaseIncrement: d("0.01"), QuoteIncrement: d("0.01"), BaseMinSize: d("-1")},
	}
	for i, p := range invalid {
		if err := p.Validate(); err == nil {
			t.Errorf("ongeldig product %d goedgekeurd", i)
		}
	}
}
