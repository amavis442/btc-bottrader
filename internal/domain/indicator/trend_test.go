package indicator

import (
	"errors"
	"testing"

	"github.com/shopspring/decimal"
)

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func series(values ...float64) []decimal.Decimal {
	out := make([]decimal.Decimal, len(values))
	for i, v := range values {
		out[i] = decimal.NewFromFloat(v)
	}
	return out
}

// repeat maakt n koersen met dezelfde waarde.
func repeat(v float64, n int) []decimal.Decimal {
	out := make([]decimal.Decimal, n)
	for i := range out {
		out[i] = decimal.NewFromFloat(v)
	}
	return out
}

func TestSMA(t *testing.T) {
	got, err := SMA(series(1, 2, 3, 4, 5), 3)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(d("4")) {
		t.Errorf("SMA = %s, verwacht 4 (gemiddelde van de laatste 3)", got)
	}
}

func TestSMAErrors(t *testing.T) {
	if _, err := SMA(series(1, 2), 3); !errors.Is(err, ErrNotEnoughData) {
		t.Errorf("verwacht ErrNotEnoughData, kreeg %v", err)
	}
	if _, err := SMA(series(1, 2), 0); err == nil {
		t.Error("verwacht fout bij periode 0")
	}
}

func TestDetermineTrend(t *testing.T) {
	cfg := TrendConfig{ShortPeriod: 2, LongPeriod: 4, FlatThreshold: d("0.005")}

	tests := []struct {
		name   string
		closes []decimal.Decimal
		want   Trend
	}{
		// lang = 100, kort = 101 → +1%
		{"stijgend", series(99, 99, 101, 101), TrendRising},
		// lang = 100, kort = 99 → −1%
		{"dalend", series(101, 101, 99, 99), TrendFalling},
		{"vlak", repeat(100, 4), TrendFlat},
		// lang = 100, kort = 100,5 → precies +0,5%: nog vlak
		{"op bovengrens is vlak", series(99.5, 99.5, 100.5, 100.5), TrendFlat},
		// lang = 100, kort = 99,5 → precies −0,5%: nog vlak
		{"op ondergrens is vlak", series(100.5, 100.5, 99.5, 99.5), TrendFlat},
		// alleen de laatste 4 tellen mee
		{"oudere koersen tellen niet mee", series(1, 1, 99, 99, 101, 101), TrendRising},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DetermineTrend(tt.closes, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("trend = %s, verwacht %s", got, tt.want)
			}
		})
	}
}

func TestDetermineTrendNotEnoughData(t *testing.T) {
	cfg := TrendConfig{ShortPeriod: 10, LongPeriod: 50, FlatThreshold: d("0.005")}
	if _, err := DetermineTrend(repeat(100, 49), cfg); !errors.Is(err, ErrNotEnoughData) {
		t.Errorf("verwacht ErrNotEnoughData, kreeg %v", err)
	}
}

func TestTrendConfigValidate(t *testing.T) {
	valid := TrendConfig{ShortPeriod: 10, LongPeriod: 50, FlatThreshold: d("0.005")}
	if err := valid.Validate(); err != nil {
		t.Errorf("geldige config afgekeurd: %v", err)
	}
	invalid := []TrendConfig{
		{ShortPeriod: 0, LongPeriod: 50},
		{ShortPeriod: 50, LongPeriod: 50},
		{ShortPeriod: 10, LongPeriod: 50, FlatThreshold: d("-0.1")},
	}
	for i, c := range invalid {
		if err := c.Validate(); err == nil {
			t.Errorf("ongeldige config %d goedgekeurd", i)
		}
	}
}
