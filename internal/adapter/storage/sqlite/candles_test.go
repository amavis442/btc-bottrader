package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"tradingbot/internal/domain"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "sub", "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func candle(start time.Time, close string) domain.Candle {
	c := decimal.RequireFromString(close)
	return domain.Candle{Start: start, Open: c, High: c.Add(decimal.NewFromInt(1)), Low: c.Sub(decimal.NewFromInt(1)),
		Close: c, Volume: decimal.RequireFromString("0.12345678")}
}

var t0 = time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)

func TestSaveAndLoadCandles(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	in := []domain.Candle{candle(t0, "74340.82"), candle(t0.Add(time.Minute), "74344.17"), candle(t0.Add(2*time.Minute), "74365.35")}
	if err := db.SaveCandles(ctx, "BTC-EUR", in); err != nil {
		t.Fatal(err)
	}
	// ander product mag niet meekomen
	if err := db.SaveCandles(ctx, "BTC-USD", []domain.Candle{candle(t0, "1")}); err != nil {
		t.Fatal(err)
	}

	out, err := db.Candles(ctx, "BTC-EUR", t0, t0.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("candles = %d, verwacht 2 (eind exclusief)", len(out))
	}
	for i, c := range out {
		want := in[i]
		if !c.Start.Equal(want.Start) || !c.Close.Equal(want.Close) || !c.High.Equal(want.High) ||
			!c.Low.Equal(want.Low) || !c.Open.Equal(want.Open) || !c.Volume.Equal(want.Volume) {
			t.Errorf("candle %d = %+v, verwacht %+v", i, c, want)
		}
		if c.Start.Location() != time.UTC {
			t.Errorf("start niet in UTC")
		}
	}
}

func TestSaveCandlesOverwrites(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if err := db.SaveCandles(ctx, "BTC-EUR", []domain.Candle{candle(t0, "100")}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveCandles(ctx, "BTC-EUR", []domain.Candle{candle(t0, "200")}); err != nil {
		t.Fatal(err)
	}
	out, _ := db.Candles(ctx, "BTC-EUR", t0, t0.Add(time.Minute))
	if len(out) != 1 || !out[0].Close.Equal(decimal.NewFromInt(200)) {
		t.Errorf("verwacht één overschreven candle met slot 200, kreeg %+v", out)
	}
}

func TestSaveNoCandles(t *testing.T) {
	if err := openTestDB(t).SaveCandles(context.Background(), "BTC-EUR", nil); err != nil {
		t.Error(err)
	}
}

func TestLatestCandleStart(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if _, ok, err := db.LatestCandleStart(ctx, "BTC-EUR"); err != nil || ok {
		t.Fatalf("lege database: ok = %v, fout = %v", ok, err)
	}
	db.SaveCandles(ctx, "BTC-EUR", []domain.Candle{candle(t0.Add(5*time.Minute), "1"), candle(t0, "1")})
	db.SaveCandles(ctx, "BTC-USD", []domain.Candle{candle(t0.Add(time.Hour), "1")})

	latest, ok, err := db.LatestCandleStart(ctx, "BTC-EUR")
	if err != nil || !ok || !latest.Equal(t0.Add(5*time.Minute)) {
		t.Errorf("laatste = %s, ok = %v, fout = %v", latest, ok, err)
	}
}

func TestReopenKeepsDataAndSkipsMigrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	db.SaveCandles(context.Background(), "BTC-EUR", []domain.Candle{candle(t0, "1")})
	db.Close()

	db, err = Open(path)
	if err != nil {
		t.Fatalf("heropenen: %v", err)
	}
	defer db.Close()
	if _, ok, _ := db.LatestCandleStart(context.Background(), "BTC-EUR"); !ok {
		t.Error("data verdwenen na heropenen")
	}
}

func TestInMemory(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.SaveCandles(context.Background(), "BTC-EUR", []domain.Candle{candle(t0, "1")}); err != nil {
		t.Error(err)
	}
}
