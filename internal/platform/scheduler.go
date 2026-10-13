package platform

import (
	"context"
	"time"
)

// NextMinuteTick geeft het eerstvolgende tijdstip na now dat offset na een hele minuut ligt.
// Met een kleine offset (bijv. 5s) heeft de exchange de vorige minuut-candle zeker afgesloten.
func NextMinuteTick(now time.Time, offset time.Duration) time.Time {
	next := now.Truncate(time.Minute).Add(offset)
	if !next.After(now) {
		next = next.Add(time.Minute)
	}
	return next
}

// EveryMinute roept fn elke minuut aan op offset na de hele minuut, tot ctx stopt.
func EveryMinute(ctx context.Context, offset time.Duration, fn func(context.Context)) {
	for {
		now := time.Now()
		timer := time.NewTimer(NextMinuteTick(now, offset).Sub(now))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			fn(ctx)
		}
	}
}
