package usecase

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sort"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"tradingbot/internal/domain"
)

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

// fakeMarket genereert voor elke minuut een candle met slotkoers = minuten sinds base,
// inclusief de lopende minuut (zoals Coinbase doet).
type fakeMarket struct {
	base     time.Time
	clock    *fakeClock
	requests [][2]time.Time
	products map[string]domain.Product
	err      error
}

func (f *fakeMarket) Product(_ context.Context, id string) (domain.Product, error) {
	if p, ok := f.products[id]; ok {
		return p, nil
	}
	return domain.Product{}, ErrProductUnavailable
}

func (f *fakeMarket) Candles(_ context.Context, _ string, start, end time.Time) ([]domain.Candle, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.requests = append(f.requests, [2]time.Time{start, end})
	if end.Sub(start) > 350*time.Minute {
		return nil, errors.New("te veel candles gevraagd")
	}
	var out []domain.Candle
	current := f.clock.now.Truncate(time.Minute)
	for ts := start; ts.Before(end) || ts.Equal(current); ts = ts.Add(time.Minute) {
		if ts.After(current) {
			break
		}
		price := decimal.NewFromInt(int64(ts.Sub(f.base) / time.Minute))
		out = append(out, domain.Candle{Start: ts, Open: price, High: price, Low: price, Close: price, Volume: decimal.NewFromInt(1)})
	}
	return out, nil
}

type memRepo struct {
	candles map[int64]domain.Candle
	err     error
}

func newMemRepo() *memRepo { return &memRepo{candles: map[int64]domain.Candle{}} }

func (r *memRepo) SaveCandles(_ context.Context, _ string, candles []domain.Candle) error {
	if r.err != nil {
		return r.err
	}
	for _, c := range candles {
		r.candles[c.Start.Unix()] = c
	}
	return nil
}

func (r *memRepo) Candles(_ context.Context, _ string, from, to time.Time) ([]domain.Candle, error) {
	var out []domain.Candle
	for _, c := range r.candles {
		if !c.Start.Before(from) && c.Start.Before(to) {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out, nil
}

func (r *memRepo) LatestCandleStart(_ context.Context, _ string) (time.Time, bool, error) {
	var latest int64
	for ts := range r.candles {
		if ts > latest {
			latest = ts
		}
	}
	if latest == 0 {
		return time.Time{}, false, nil
	}
	return time.Unix(latest, 0).UTC(), true, nil
}

var base = time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)

func newService(t *testing.T, now time.Time) (*MarketDataService, *fakeMarket, *memRepo, *fakeClock) {
	t.Helper()
	clock := &fakeClock{now: now}
	market := &fakeMarket{base: base, clock: clock}
	repo := newMemRepo()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc, err := NewMarketDataService(market, repo, clock, log, "BTC-EUR", 10*time.Minute, 50)
	if err != nil {
		t.Fatal(err)
	}
	return svc, market, repo, clock
}

func TestSyncWarmupOnEmptyDatabase(t *testing.T) {
	now := base.Add(24*time.Hour + 3*time.Minute + 20*time.Second) // 00:03:20 de volgende dag
	svc, market, repo, _ := newService(t, now)

	saved, err := svc.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// warm-up vanaf 00:00 − 50 × 10 min tot en met de laatst afgesloten minuut (00:02)
	want := 500 + 3
	if saved != want || len(repo.candles) != want {
		t.Errorf("opgeslagen = %d (repo %d), verwacht %d", saved, len(repo.candles), want)
	}
	if _, ok := repo.candles[now.Truncate(time.Minute).Unix()]; ok {
		t.Error("lopende minuut mag niet opgeslagen worden")
	}
	for _, r := range market.requests {
		if r[1].Sub(r[0]) > maxCandlesPerRequest*time.Minute {
			t.Errorf("verzoek te groot: %s - %s", r[0], r[1])
		}
	}
	if len(market.requests) != 2 {
		t.Errorf("verwacht 2 verzoeken (300 + 203), kreeg %d", len(market.requests))
	}
}

func TestSyncResumesAfterLatestCandle(t *testing.T) {
	now := base.Add(24 * time.Hour)
	svc, market, repo, clock := newService(t, now)
	if _, err := svc.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := len(repo.candles)
	market.requests = nil

	clock.now = now.Add(3*time.Minute + 10*time.Second)
	saved, err := svc.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if saved != 3 || len(repo.candles) != before+3 {
		t.Errorf("opgeslagen = %d, verwacht 3", saved)
	}
	if len(market.requests) != 1 || !market.requests[0][0].Equal(now) {
		t.Errorf("verwacht één verzoek vanaf %s, kreeg %v", now, market.requests)
	}
}

func TestSyncAfterLongDowntimeOnlyFetchesWarmup(t *testing.T) {
	now := base.Add(24 * time.Hour)
	svc, _, repo, clock := newService(t, now)
	if _, err := svc.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}

	clock.now = now.Add(7 * 24 * time.Hour) // een week down
	if _, err := svc.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(repo.candles); got != 1000 {
		t.Errorf("candles = %d, verwacht 2 × 500 (geen hele week)", got)
	}
}

func TestSyncNothingToDo(t *testing.T) {
	now := base.Add(24*time.Hour + 30*time.Second)
	svc, market, _, _ := newService(t, now)
	if _, err := svc.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	market.requests = nil
	saved, err := svc.Sync(context.Background())
	if err != nil || saved != 0 || len(market.requests) != 0 {
		t.Errorf("saved = %d, err = %v, verzoeken = %d; verwacht niets", saved, err, len(market.requests))
	}
}

func TestSyncErrors(t *testing.T) {
	now := base.Add(24 * time.Hour)
	svc, market, repo, _ := newService(t, now)

	market.err = errors.New("netwerk weg")
	if _, err := svc.Sync(context.Background()); err == nil {
		t.Error("verwacht fout van de exchange")
	}
	market.err = nil
	repo.err = errors.New("schijf vol")
	if _, err := svc.Sync(context.Background()); err == nil {
		t.Error("verwacht fout van de opslag")
	}
}

func TestStrategyCandles(t *testing.T) {
	now := base.Add(24*time.Hour + 5*time.Minute)
	svc, _, _, _ := newService(t, now)
	if _, err := svc.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}

	candles, err := svc.StrategyCandles(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(candles) != 50 {
		t.Fatalf("candles = %d, verwacht 50", len(candles))
	}
	last := candles[len(candles)-1]
	// laatste afgesloten bucket: 23:50 - 00:00, slotkoers = minuut 23:59
	if !last.Start.Equal(base.Add(24*time.Hour - 10*time.Minute)) {
		t.Errorf("laatste start = %s", last.Start)
	}
	if !last.Close.Equal(decimal.NewFromInt(24*60 - 1)) {
		t.Errorf("laatste slot = %s, verwacht %d", last.Close, 24*60-1)
	}
	if !last.Open.Equal(decimal.NewFromInt(24*60 - 10)) {
		t.Errorf("laatste open = %s", last.Open)
	}
}

func TestNewMarketDataServiceValidation(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := NewMarketDataService(nil, nil, nil, log, "BTC-EUR", 90*time.Second, 50); err == nil {
		t.Error("verwacht fout bij interval dat geen veelvoud van een minuut is")
	}
	if _, err := NewMarketDataService(nil, nil, nil, log, "BTC-EUR", 10*time.Minute, 0); err == nil {
		t.Error("verwacht fout bij history 0")
	}
}

func TestResolveProduct(t *testing.T) {
	eur := domain.Product{ID: "BTC-EUR"}
	usd := domain.Product{ID: "BTC-USD"}
	ctx := context.Background()

	tests := []struct {
		name     string
		products map[string]domain.Product
		fallback string
		want     string
		wantErr  bool
	}{
		{"primair beschikbaar", map[string]domain.Product{"BTC-EUR": eur, "BTC-USD": usd}, "BTC-USD", "BTC-EUR", false},
		{"terugval", map[string]domain.Product{"BTC-USD": usd}, "BTC-USD", "BTC-USD", false},
		{"geen terugval ingesteld", map[string]domain.Product{"BTC-USD": usd}, "", "", true},
		{"beide niet beschikbaar", map[string]domain.Product{}, "BTC-USD", "", true},
	}
	for _, tt := range tests {
		got, err := ResolveProduct(ctx, &fakeMarket{products: tt.products}, "BTC-EUR", tt.fallback)
		if (err != nil) != tt.wantErr || got.ID != tt.want {
			t.Errorf("%s: product = %q, fout = %v", tt.name, got.ID, err)
		}
	}
}
