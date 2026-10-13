package usecase

import (
	"context"
	"sync"
	"time"

	"tradingbot/internal/domain/strategy"
)

// StaticFees levert vaste fee-tarieven, bijvoorbeeld de terugvalwaarden uit de config.
type StaticFees strategy.Fees

// Fees geeft de vaste tarieven.
func (f StaticFees) Fees(context.Context) (strategy.Fees, error) { return strategy.Fees(f), nil }

// CachedFees bewaart de tarieven van een FeeSource gedurende ttl. Lukt verversen niet, dan blijven de
// laatst bekende tarieven gelden; zijn die er nog niet, dan wordt de fout teruggegeven.
type CachedFees struct {
	source FeeSource
	ttl    time.Duration
	clock  Clock

	mu      sync.Mutex
	fees    strategy.Fees
	fetched time.Time
}

// NewCachedFees maakt een cache rond source.
func NewCachedFees(source FeeSource, ttl time.Duration, clock Clock) *CachedFees {
	return &CachedFees{source: source, ttl: ttl, clock: clock}
}

// Fees geeft de tarieven uit de cache, of haalt ze op als de cache verlopen is.
func (c *CachedFees) Fees(ctx context.Context) (strategy.Fees, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock.Now()
	if !c.fetched.IsZero() && now.Sub(c.fetched) < c.ttl {
		return c.fees, nil
	}
	fees, err := c.source.Fees(ctx)
	if err != nil {
		if c.fetched.IsZero() {
			return strategy.Fees{}, err
		}
		return c.fees, nil
	}
	c.fees, c.fetched = fees, now
	return fees, nil
}
