// Package domain bevat de kernbegrippen van de bot. Alles hierin is pure logica zonder I/O.
package domain

import (
	"sort"
	"time"

	"github.com/shopspring/decimal"
)

// Candle is een OHLCV-candle. Start is het begin van de periode in UTC.
type Candle struct {
	Start  time.Time
	Open   decimal.Decimal
	High   decimal.Decimal
	Low    decimal.Decimal
	Close  decimal.Decimal
	Volume decimal.Decimal
}

// Aggregate voegt candles samen tot candles van het opgegeven interval. De buckets worden uitgelijnd op
// het interval (bijv. 10:00, 10:10, ...). Alleen buckets die op tijdstip now volledig afgesloten zijn,
// worden teruggegeven. Ontbrekende candles binnen een bucket (minuten zonder trades) worden overgeslagen.
func Aggregate(candles []Candle, interval time.Duration, now time.Time) []Candle {
	if interval <= 0 || len(candles) == 0 {
		return nil
	}

	sorted := make([]Candle, len(candles))
	copy(sorted, candles)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Start.Before(sorted[j].Start) })

	var result []Candle
	var current *Candle
	for _, c := range sorted {
		bucket := c.Start.UTC().Truncate(interval)
		if current != nil && current.Start.Equal(bucket) {
			current.High = decimal.Max(current.High, c.High)
			current.Low = decimal.Min(current.Low, c.Low)
			current.Close = c.Close
			current.Volume = current.Volume.Add(c.Volume)
			continue
		}
		if current != nil {
			result = append(result, *current)
		}
		current = &Candle{Start: bucket, Open: c.Open, High: c.High, Low: c.Low, Close: c.Close, Volume: c.Volume}
	}
	result = append(result, *current)

	closed := result[:0]
	for _, c := range result {
		if !c.Start.Add(interval).After(now) {
			closed = append(closed, c)
		}
	}
	return closed
}

// Closes geeft de slotkoersen van de candles terug, in dezelfde volgorde.
func Closes(candles []Candle) []decimal.Decimal {
	closes := make([]decimal.Decimal, len(candles))
	for i, c := range candles {
		closes[i] = c.Close
	}
	return closes
}
