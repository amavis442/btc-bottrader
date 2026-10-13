package usecase

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"tradingbot/internal/domain"
)

// maxCandlesPerRequest blijft onder de limiet van Coinbase (350 candles per verzoek).
const maxCandlesPerRequest = 300

// ResolveProduct zoekt het primaire product op en valt terug op fallback als het primaire niet
// beschikbaar is. Een lege fallback betekent: geen terugval.
func ResolveProduct(ctx context.Context, source MarketData, primary, fallback string) (domain.Product, error) {
	product, err := source.Product(ctx, primary)
	if err == nil || !errors.Is(err, ErrProductUnavailable) || fallback == "" {
		return product, err
	}
	product, fbErr := source.Product(ctx, fallback)
	if fbErr != nil {
		return domain.Product{}, fmt.Errorf("%s: %w; terugval %s: %w", primary, err, fallback, fbErr)
	}
	return product, nil
}

// MarketDataService haalt 1-minuut candles op, slaat ze op en levert de strategie-candles.
type MarketDataService struct {
	source    MarketData
	repo      CandleRepository
	clock     Clock
	log       *slog.Logger
	productID string
	interval  time.Duration // interval van de strategie-candles, bijv. 10 minuten
	history   int           // aantal strategie-candles dat de strategie nodig heeft
}

// NewMarketDataService maakt de service. interval moet een veelvoud van een minuut zijn.
func NewMarketDataService(source MarketData, repo CandleRepository, clock Clock, log *slog.Logger,
	productID string, interval time.Duration, history int) (*MarketDataService, error) {
	if interval < time.Minute || interval%time.Minute != 0 {
		return nil, fmt.Errorf("marktdata: interval %s is geen veelvoud van een minuut", interval)
	}
	if history <= 0 {
		return nil, errors.New("marktdata: history moet positief zijn")
	}
	return &MarketDataService{source: source, repo: repo, clock: clock, log: log,
		productID: productID, interval: interval, history: history}, nil
}

// warmupStart is het begin van de periode die nodig is voor history afgesloten strategie-candles.
func (s *MarketDataService) warmupStart(now time.Time) time.Time {
	return now.Truncate(s.interval).Add(-time.Duration(s.history) * s.interval)
}

// Sync haalt alle afgesloten 1-minuut candles op die nog ontbreken: vanaf de nieuwste opgeslagen
// candle, maar nooit verder terug dan nodig is voor de warm-up. Geeft het aantal opgeslagen candles terug.
func (s *MarketDataService) Sync(ctx context.Context) (int, error) {
	now := s.clock.Now().UTC()
	from := s.warmupStart(now)

	latest, ok, err := s.repo.LatestCandleStart(ctx, s.productID)
	if err != nil {
		return 0, fmt.Errorf("marktdata: laatste candle opvragen: %w", err)
	}
	if ok && latest.Add(time.Minute).After(from) {
		from = latest.Add(time.Minute)
	}
	return s.SyncFrom(ctx, from)
}

// SyncFrom haalt alle afgesloten 1-minuut candles vanaf from op, in delen, en slaat ze op.
// Bruikbaar om een specifieke periode aan te vullen, bijvoorbeeld bij herstel na een herstart.
func (s *MarketDataService) SyncFrom(ctx context.Context, from time.Time) (int, error) {
	now := s.clock.Now().UTC()
	end := now.Truncate(time.Minute) // de lopende minuut is nog niet afgesloten
	from = from.UTC().Truncate(time.Minute)

	saved := 0
	for chunkStart := from; chunkStart.Before(end); {
		chunkEnd := chunkStart.Add(maxCandlesPerRequest * time.Minute)
		if chunkEnd.After(end) {
			chunkEnd = end
		}

		candles, err := s.source.Candles(ctx, s.productID, chunkStart, chunkEnd)
		if err != nil {
			return saved, fmt.Errorf("marktdata: candles %s - %s ophalen: %w", chunkStart, chunkEnd, err)
		}
		closed := candles[:0]
		for _, c := range candles {
			if !c.Start.Before(chunkStart) && c.Start.Before(chunkEnd) {
				closed = append(closed, c)
			}
		}
		if err := s.repo.SaveCandles(ctx, s.productID, closed); err != nil {
			return saved, fmt.Errorf("marktdata: candles opslaan: %w", err)
		}
		saved += len(closed)
		chunkStart = chunkEnd
	}

	if saved > 0 {
		s.log.Debug("candles gesynchroniseerd", "product", s.productID, "aantal", saved, "vanaf", from)
	}
	return saved, nil
}

// StrategyCandles geeft de laatste (maximaal count) afgesloten strategie-candles, oudste eerst.
func (s *MarketDataService) StrategyCandles(ctx context.Context, count int) ([]domain.Candle, error) {
	now := s.clock.Now().UTC()
	from := now.Truncate(s.interval).Add(-time.Duration(count) * s.interval)
	minutes, err := s.repo.Candles(ctx, s.productID, from, now)
	if err != nil {
		return nil, fmt.Errorf("marktdata: candles laden: %w", err)
	}
	candles := domain.Aggregate(minutes, s.interval, now)
	if len(candles) > count {
		candles = candles[len(candles)-count:]
	}
	return candles, nil
}

// Interval geeft het interval van de strategie-candles.
func (s *MarketDataService) Interval() time.Duration { return s.interval }

// History geeft het aantal strategie-candles dat de strategie nodig heeft.
func (s *MarketDataService) History() int { return s.history }

// MinuteCandles geeft de opgeslagen 1-minuut candles met een start in [from, to).
func (s *MarketDataService) MinuteCandles(ctx context.Context, from, to time.Time) ([]domain.Candle, error) {
	return s.repo.Candles(ctx, s.productID, from, to)
}
