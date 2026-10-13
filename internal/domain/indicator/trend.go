// Package indicator bevat technische indicatoren voor de strategie.
package indicator

import (
	"errors"
	"fmt"

	"github.com/shopspring/decimal"
)

// Trend is de richting van de koers.
type Trend string

const (
	TrendRising  Trend = "STIJGEND"
	TrendFalling Trend = "DALEND"
	TrendFlat    Trend = "VLAK"
)

// ErrNotEnoughData wordt teruggegeven als er te weinig koersen zijn voor de berekening.
var ErrNotEnoughData = errors.New("indicator: te weinig koersdata")

// SMA berekent het gemiddelde van de laatste period waarden.
func SMA(values []decimal.Decimal, period int) (decimal.Decimal, error) {
	if period <= 0 {
		return decimal.Zero, fmt.Errorf("indicator: ongeldige periode %d", period)
	}
	if len(values) < period {
		return decimal.Zero, ErrNotEnoughData
	}
	sum := decimal.Zero
	for _, v := range values[len(values)-period:] {
		sum = sum.Add(v)
	}
	return sum.Div(decimal.NewFromInt(int64(period))), nil
}

// TrendConfig bevat de parameters voor de trendbepaling.
type TrendConfig struct {
	ShortPeriod   int
	LongPeriod    int
	FlatThreshold decimal.Decimal // als fractie, bijv. 0.005 voor 0,5%
}

// Validate controleert de trendparameters.
func (c TrendConfig) Validate() error {
	if c.ShortPeriod <= 0 || c.LongPeriod <= 0 {
		return errors.New("trend: periodes moeten positief zijn")
	}
	if c.ShortPeriod >= c.LongPeriod {
		return errors.New("trend: korte periode moet kleiner zijn dan de lange periode")
	}
	if c.FlatThreshold.IsNegative() {
		return errors.New("trend: drempel voor vlak mag niet negatief zijn")
	}
	return nil
}

// DetermineTrend vergelijkt de korte SMA met de lange SMA over de slotkoersen (oudste eerst).
// Ligt het relatieve verschil binnen de drempel, dan is de trend vlak.
func DetermineTrend(closes []decimal.Decimal, cfg TrendConfig) (Trend, error) {
	short, err := SMA(closes, cfg.ShortPeriod)
	if err != nil {
		return "", err
	}
	long, err := SMA(closes, cfg.LongPeriod)
	if err != nil {
		return "", err
	}
	if long.IsZero() {
		return "", errors.New("indicator: lange SMA is nul")
	}

	diff := short.Sub(long).Div(long)
	switch {
	case diff.GreaterThan(cfg.FlatThreshold):
		return TrendRising, nil
	case diff.LessThan(cfg.FlatThreshold.Neg()):
		return TrendFalling, nil
	default:
		return TrendFlat, nil
	}
}
