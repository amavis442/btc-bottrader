package paper

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"tradingbot/internal/domain"
	"tradingbot/internal/domain/strategy"
	"tradingbot/internal/usecase"
)

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

type memKV map[string][]byte

func (m memKV) GetValue(_ context.Context, key string) ([]byte, bool, error) {
	v, ok := m[key]
	return v, ok, nil
}

func (m memKV) SetValue(_ context.Context, key string, value []byte) error {
	m[key] = value
	return nil
}

type memCandles []domain.Candle

func (m *memCandles) SaveCandles(_ context.Context, _ string, candles []domain.Candle) error {
	*m = append(*m, candles...)
	return nil
}

func (m *memCandles) Candles(_ context.Context, _ string, from, to time.Time) ([]domain.Candle, error) {
	var out []domain.Candle
	for _, c := range *m {
		if !c.Start.Before(from) && c.Start.Before(to) {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out, nil
}

func (m *memCandles) LatestCandleStart(context.Context, string) (time.Time, bool, error) {
	return time.Time{}, false, nil
}

type fixedTicker struct{ bid, ask string }

func (f *fixedTicker) Ticker(context.Context, string) (domain.Ticker, error) {
	return domain.Ticker{Price: d(f.bid), BestBid: d(f.bid), BestAsk: d(f.ask)}, nil
}

var (
	t0      = time.Date(2026, 10, 13, 10, 0, 30, 0, time.UTC)
	product = domain.Product{ID: "BTC-EUR", BaseIncrement: d("0.00000001"), QuoteIncrement: d("0.01"),
		BaseMinSize: d("0.00000001"), QuoteMinSize: d("1")}
	fees = usecase.StaticFees(strategy.Fees{Maker: d("0.0025"), Taker: d("0.005")})
)

type env struct {
	kv      memKV
	candles *memCandles
	ticker  *fixedTicker
	clock   *fakeClock
	ex      *Exchange
}

func newEnv() *env {
	e := &env{kv: memKV{}, candles: &memCandles{}, ticker: &fixedTicker{bid: "60000", ask: "60000.01"}, clock: &fakeClock{now: t0}}
	e.ex = New(e.kv, e.candles, e.ticker, fees, e.clock, product, d("100"))
	return e
}

// candle voegt een 1-minuut candle toe die op minute minuten na 10:00 begint.
func (e *env) candle(minute int, low, high string) {
	start := time.Date(2026, 10, 13, 10, minute, 0, 0, time.UTC)
	*e.candles = append(*e.candles, domain.Candle{Start: start, Open: d(low), High: d(high), Low: d(low), Close: d(high), Volume: d("1")})
}

func (e *env) at(minute int) { e.clock.now = time.Date(2026, 10, 13, 10, minute, 5, 0, time.UTC) }

func (e *env) place(t *testing.T, id string, side domain.Side, size, price string) domain.Order {
	t.Helper()
	o, err := e.ex.PlaceLimitOrder(context.Background(), usecase.OrderRequest{
		ClientOrderID: id, ProductID: "BTC-EUR", Side: side, Size: d(size), Price: d(price)})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func (e *env) balances(t *testing.T) map[string]domain.Balance {
	t.Helper()
	b, err := e.ex.Balances(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (e *env) order(t *testing.T, id string) domain.Order {
	t.Helper()
	o, err := e.ex.Order(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func assertDec(t *testing.T, name string, got decimal.Decimal, want string) {
	t.Helper()
	if !got.Equal(d(want)) {
		t.Errorf("%s = %s, verwacht %s", name, got, want)
	}
}

func TestStartBalance(t *testing.T) {
	b := newEnv().balances(t)
	assertDec(t, "EUR", b["EUR"].Available, "100")
	assertDec(t, "BTC", b["BTC"].Available, "0")
}

func TestMarketableBuyFillsImmediatelyAsTaker(t *testing.T) {
	e := newEnv()
	o := e.place(t, "a", domain.SideBuy, "0.001", "60100")
	if o.Status != domain.OrderFilled {
		t.Fatalf("status = %s, verwacht FILLED", o.Status)
	}
	// gevuld tegen de best ask, niet tegen de hogere limiet
	assertDec(t, "waarde", o.FilledValue, "60.00001")
	assertDec(t, "fee (taker 0,5%)", o.Fee, "0.30000005")

	b := e.balances(t)
	assertDec(t, "EUR", b["EUR"].Available, "39.69998995")
	assertDec(t, "EUR vast", b["EUR"].Hold, "0")
	assertDec(t, "BTC", b["BTC"].Available, "0.001")

	fills, _ := e.ex.Fills(context.Background(), o.ID)
	if len(fills) != 1 || fills[0].Liquidity != domain.LiquidityTaker {
		t.Errorf("fills = %+v", fills)
	}
}

func TestRestingBuyFillsOnlyWhenPriceGoesThrough(t *testing.T) {
	e := newEnv()
	o := e.place(t, "a", domain.SideBuy, "0.001", "59000")
	if o.Status != domain.OrderOpen {
		t.Fatalf("status = %s", o.Status)
	}
	b := e.balances(t)
	assertDec(t, "vast (incl. taker-fee)", b["EUR"].Hold, "59.295")
	assertDec(t, "vrij", b["EUR"].Available, "40.705")

	e.candle(0, "58000", "60000") // minuut van plaatsing telt niet mee
	e.candle(1, "59000", "60000") // raakt de limiet alleen
	e.at(2)
	if got := e.order(t, o.ID); got.Status != domain.OrderOpen {
		t.Fatalf("order gevuld door candle van plaatsing of door alleen raken: %s", got.Status)
	}

	e.candle(2, "58999.99", "60000")
	e.at(3)
	got := e.order(t, o.ID)
	if got.Status != domain.OrderFilled {
		t.Fatalf("status = %s, verwacht FILLED", got.Status)
	}
	assertDec(t, "waarde", got.FilledValue, "59")
	assertDec(t, "fee (maker 0,25%)", got.Fee, "0.1475")
	if !got.DoneAt.Equal(time.Date(2026, 10, 13, 10, 3, 0, 0, time.UTC)) {
		t.Errorf("gevuld op %s, verwacht einde van de candle", got.DoneAt)
	}
	b = e.balances(t)
	assertDec(t, "EUR", b["EUR"].Available, "40.8525")
	assertDec(t, "EUR vast", b["EUR"].Hold, "0")
	assertDec(t, "BTC", b["BTC"].Available, "0.001")
}

func TestRestingSell(t *testing.T) {
	e := newEnv()
	e.place(t, "koop", domain.SideBuy, "0.001", "60000.01") // direct gevuld
	o := e.place(t, "verkoop", domain.SideSell, "0.0005", "63000")
	if o.Status != domain.OrderOpen {
		t.Fatalf("status = %s", o.Status)
	}
	assertDec(t, "BTC vast", e.balances(t)["BTC"].Hold, "0.0005")

	e.candle(1, "62000", "63000.01")
	e.at(2)
	got := e.order(t, o.ID)
	if got.Status != domain.OrderFilled {
		t.Fatalf("status = %s", got.Status)
	}
	b := e.balances(t)
	assertDec(t, "BTC", b["BTC"].Total(), "0.0005")
	// 39.69998995 + 31,5 − 0,07875 fee
	assertDec(t, "EUR", b["EUR"].Available, "71.12123995")
}

func TestMarketableSellFillsAtBid(t *testing.T) {
	e := newEnv()
	e.place(t, "koop", domain.SideBuy, "0.001", "60000.01")
	o := e.place(t, "verkoop", domain.SideSell, "0.001", "59000")
	if o.Status != domain.OrderFilled || !o.AveragePrice().Equal(d("60000")) {
		t.Errorf("verwacht directe vulling tegen bid 60000: %+v", o)
	}
}

func TestInsufficientFunds(t *testing.T) {
	e := newEnv()
	tests := []struct {
		side        domain.Side
		size, price string
	}{
		{domain.SideBuy, "0.002", "59000"}, // 118 EUR > 100
		{domain.SideSell, "0.001", "70000"},
	}
	for _, tt := range tests {
		_, err := e.ex.PlaceLimitOrder(context.Background(), usecase.OrderRequest{
			ClientOrderID: string(tt.side), ProductID: "BTC-EUR", Side: tt.side, Size: d(tt.size), Price: d(tt.price)})
		if !errors.Is(err, usecase.ErrInsufficientFunds) {
			t.Errorf("%s: verwacht ErrInsufficientFunds, kreeg %v", tt.side, err)
		}
	}
}

func TestInvalidOrders(t *testing.T) {
	e := newEnv()
	tests := []usecase.OrderRequest{
		{ClientOrderID: "a", ProductID: "BTC-USD", Side: domain.SideBuy, Size: d("0.001"), Price: d("59000")},
		{ClientOrderID: "b", ProductID: "BTC-EUR", Side: domain.SideBuy, Size: d("0.00001"), Price: d("59000")}, // < 1 EUR
		{ClientOrderID: "c", ProductID: "BTC-EUR", Side: domain.SideBuy, Size: d("0.001"), Price: d("59000.001")},
		{ClientOrderID: "d", ProductID: "BTC-EUR", Side: "HOLD", Size: d("0.001"), Price: d("59000")},
	}
	for _, req := range tests {
		if _, err := e.ex.PlaceLimitOrder(context.Background(), req); err == nil {
			t.Errorf("%s: verwacht fout", req.ClientOrderID)
		}
	}
}

func TestPlaceIsIdempotent(t *testing.T) {
	e := newEnv()
	first := e.place(t, "zelfde", domain.SideBuy, "0.001", "59000")
	second := e.place(t, "zelfde", domain.SideBuy, "0.001", "59000")
	if first.ID != second.ID {
		t.Errorf("tweede plaatsing gaf nieuwe order %s (eerste %s)", second.ID, first.ID)
	}
	assertDec(t, "vast", e.balances(t)["EUR"].Hold, "59.295")
}

func TestCancel(t *testing.T) {
	e := newEnv()
	o := e.place(t, "a", domain.SideBuy, "0.001", "59000")
	if err := e.ex.CancelOrder(context.Background(), o.ID); err != nil {
		t.Fatal(err)
	}
	got := e.order(t, o.ID)
	if got.Status != domain.OrderCancelled || got.DoneAt.IsZero() {
		t.Errorf("order = %+v", got)
	}
	b := e.balances(t)
	assertDec(t, "EUR", b["EUR"].Available, "100")
	assertDec(t, "vast", b["EUR"].Hold, "0")

	// annuleren van een afgeronde order verandert niets
	if err := e.ex.CancelOrder(context.Background(), o.ID); err != nil {
		t.Error(err)
	}
	if err := e.ex.CancelOrder(context.Background(), "bestaat-niet"); !errors.Is(err, ErrOrderNotFound) {
		t.Errorf("verwacht ErrOrderNotFound, kreeg %v", err)
	}
	if _, err := e.ex.Order(context.Background(), "bestaat-niet"); !errors.Is(err, ErrOrderNotFound) {
		t.Errorf("verwacht ErrOrderNotFound, kreeg %v", err)
	}
}

func TestCancelAfterMissedFillKeepsFill(t *testing.T) {
	e := newEnv()
	o := e.place(t, "a", domain.SideBuy, "0.001", "59000")
	e.candle(1, "58000", "60000")
	e.at(2)
	if err := e.ex.CancelOrder(context.Background(), o.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.order(t, o.ID); got.Status != domain.OrderFilled {
		t.Errorf("order die al gevuld zou zijn, mag niet geannuleerd worden: %s", got.Status)
	}
}

func TestStatePersistsAndCatchesUpAfterRestart(t *testing.T) {
	e := newEnv()
	o := e.place(t, "a", domain.SideBuy, "0.001", "59000")

	// "herstart": nieuwe exchange op dezelfde opslag, terwijl er candles gemist zijn
	e.candle(30, "58000", "60000")
	e.at(45)
	restarted := New(e.kv, e.candles, e.ticker, fees, e.clock, product, d("100"))
	got, err := restarted.Order(context.Background(), o.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.OrderFilled {
		t.Errorf("gemiste vulling niet verwerkt: %s", got.Status)
	}
	b, _ := restarted.Balances(context.Background())
	assertDec(t, "BTC", b["BTC"].Available, "0.001")
}

func TestPruneKeepsOpenOrders(t *testing.T) {
	e := newEnv()
	e.ex.start = d("1000000")
	e.kv = memKV{}
	e.ex.kv = e.kv
	open := e.place(t, "open", domain.SideBuy, "0.001", "50000")
	for i := 0; i < keepDoneOrders+5; i++ {
		e.place(t, fmt.Sprintf("koop-%d", i), domain.SideBuy, "0.0001", "60000.01")
	}
	done := 0
	for _, o := range e.ex.st.Orders {
		if o.Status.Done() {
			done++
		}
	}
	if done != keepDoneOrders {
		t.Errorf("afgeronde orders = %d, verwacht %d", done, keepDoneOrders)
	}
	if got := e.order(t, open.ID); got.Status != domain.OrderOpen {
		t.Error("open order verdwenen")
	}
	if len(e.ex.st.Fills) != keepDoneOrders {
		t.Errorf("fills = %d, verwacht %d", len(e.ex.st.Fills), keepDoneOrders)
	}
}
