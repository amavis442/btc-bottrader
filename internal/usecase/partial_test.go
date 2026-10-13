package usecase_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"tradingbot/internal/adapter/storage/sqlite"
	"tradingbot/internal/domain"
	"tradingbot/internal/domain/indicator"
	"tradingbot/internal/domain/strategy"
	"tradingbot/internal/usecase"
)

// scripted is een exchange waarvan de test precies bepaalt wanneer en hoeveel er gevuld wordt.
// Daarmee zijn gedeeltelijke vullingen te testen, die de paper-exchange niet doet.
type scripted struct {
	clock    *clock
	orders   map[string]*domain.Order
	fills    map[string][]domain.Fill
	balances map[string]domain.Balance
	placed   []usecase.OrderRequest
	seq      int
}

func newScripted(c *clock) *scripted {
	return &scripted{clock: c, orders: map[string]*domain.Order{}, fills: map[string][]domain.Fill{},
		balances: map[string]domain.Balance{"EUR": {Available: d("100")}, "BTC": {Available: d("0")}}}
}

func (x *scripted) PlaceLimitOrder(_ context.Context, req usecase.OrderRequest) (domain.Order, error) {
	for _, o := range x.orders {
		if o.ClientOrderID == req.ClientOrderID {
			return *o, nil
		}
	}
	x.seq++
	o := &domain.Order{ID: fmt.Sprintf("o-%d", x.seq), ClientOrderID: req.ClientOrderID, ProductID: req.ProductID,
		Side: req.Side, Price: req.Price, Size: req.Size, Status: domain.OrderOpen, CreatedAt: x.clock.Now()}
	x.orders[o.ID] = o
	x.placed = append(x.placed, req)
	return *o, nil
}

func (x *scripted) CancelOrder(_ context.Context, id string) error {
	if o := x.orders[id]; o.Status == domain.OrderOpen {
		o.Status, o.DoneAt = domain.OrderCancelled, x.clock.Now()
	}
	return nil
}

func (x *scripted) Order(_ context.Context, id string) (domain.Order, error) {
	return *x.orders[id], nil
}

func (x *scripted) Fills(_ context.Context, id string) ([]domain.Fill, error) {
	return x.fills[id], nil
}

func (x *scripted) Balances(context.Context) (map[string]domain.Balance, error) {
	out := map[string]domain.Balance{}
	for k, v := range x.balances {
		out[k] = v
	}
	return out, nil
}

// fill vult size van de laatst geplaatste order tegen price.
func (x *scripted) fill(size, price string) *domain.Order {
	o := x.orders[fmt.Sprintf("o-%d", x.seq)]
	value := d(size).Mul(d(price))
	fee := value.Mul(d("0.0025"))
	x.fills[o.ID] = append(x.fills[o.ID], domain.Fill{TradeID: fmt.Sprintf("%s-f%d", o.ID, len(x.fills[o.ID])),
		OrderID: o.ID, Price: d(price), Size: d(size), Fee: fee, Liquidity: domain.LiquidityMaker, Time: x.clock.Now()})
	o.FilledSize = o.FilledSize.Add(d(size))
	o.FilledValue = o.FilledValue.Add(value)
	o.Fee = o.Fee.Add(fee)
	if o.FilledSize.Equal(o.Size) {
		o.Status, o.DoneAt = domain.OrderFilled, x.clock.Now()
	}
	return o
}

type partialSim struct {
	t      *testing.T
	ctx    context.Context
	clock  *clock
	db     *sqlite.DB
	ex     *scripted
	trader *usecase.Trader
}

// newPartialSim start een trader op de scripted exchange; is initial gezet, dan begint de trader in
// die toestand (zoals na een herstart).
func newPartialSim(t *testing.T, p *path, initial *usecase.TraderState) *partialSim {
	t.Helper()
	db, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	c := &clock{now: p.start.Add(5 * time.Second)}
	s := &partialSim{t: t, ctx: context.Background(), clock: c, db: db, ex: newScripted(c)}

	if initial != nil {
		data, _ := json.Marshal(initial)
		if err := db.SetValue(s.ctx, "trader", data); err != nil {
			t.Fatal(err)
		}
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	machine, _ := strategy.NewMachine(strategy.Config{
		Trend: indicator.TrendConfig{ShortPeriod: 2, LongPeriod: 4, FlatThreshold: d("0.005")},
		TakeProfits: [2]strategy.TakeProfit{
			{NetProfit: d("0.06"), SellFraction: d("0.5")},
			{NetProfit: d("0.12"), SellFraction: d("0.65")},
		},
		TrailingDistance: d("0.05"), CooldownReversal: 3, CooldownRising: 1,
	}, product)
	f := &feed{path: p, clock: c}
	market, _ := usecase.NewMarketDataService(f, db, c, log, product.ID, 10*time.Minute, 4)
	fees := usecase.StaticFees(strategy.Fees{Maker: d("0.0025"), Taker: d("0.005")})
	s.trader, err = usecase.NewTrader(s.ctx, usecase.TraderConfig{
		Budget: d("50"), BuyOrderTimeout: 10 * time.Minute, StopOrderTimeout: 2 * time.Minute, FeeRefresh: time.Hour,
	}, usecase.TraderDeps{Product: product, Machine: machine, Market: market, Tickers: f, Exchange: s.ex,
		Fees: fees, Store: db, KV: db, Notifier: &recorder{}, Clock: c, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (s *partialSim) tick() {
	s.t.Helper()
	if err := s.trader.Tick(s.ctx); err != nil {
		s.t.Fatal(err)
	}
	s.clock.set(s.clock.Now().Add(time.Minute))
}

func TestPartialStopFillIsReplacedForRemainder(t *testing.T) {
	p := newPath(t0, "66000").flat(100)
	s := newPartialSim(t, p, &usecase.TraderState{
		InstanceID: "test",
		Strategy: strategy.State{
			Phase:         strategy.PhaseTrailing,
			Position:      &strategy.Position{EntryQuantity: d("0.001"), Quantity: d("0.001"), CostPrice: d("60000")},
			TrailingHigh:  d("70000"),
			StopTriggered: true,
		},
	})
	s.ex.balances["BTC"] = domain.Balance{Available: d("0.001")}

	s.tick()
	if len(s.ex.placed) != 1 || !s.ex.placed[0].Size.Equal(d("0.001")) || s.ex.placed[0].Side != domain.SideSell {
		t.Fatalf("verwacht stopverkoop van 0,001 BTC: %+v", s.ex.placed)
	}

	s.ex.fill("0.0004", "65999.99")
	s.tick()
	st := s.trader.State()
	if !st.Strategy.Position.Quantity.Equal(d("0.0006")) || st.SellOrder == nil {
		t.Fatalf("na deelvulling verwacht 0,0006 BTC over en de order nog open: %+v", st.Strategy.Position)
	}

	// na de timeout van 2 minuten: annuleren en het restant opnieuw aanbieden
	s.ex.balances["BTC"] = domain.Balance{Available: d("0.0006")}
	s.tick()
	if len(s.ex.placed) != 2 || !s.ex.placed[1].Size.Equal(d("0.0006")) {
		t.Fatalf("verwacht nieuwe stopverkoop voor het restant: %+v", s.ex.placed)
	}
	if s.ex.orders["o-1"].Status != domain.OrderCancelled {
		t.Errorf("eerste stoporder: %s, verwacht CANCELLED", s.ex.orders["o-1"].Status)
	}

	s.ex.fill("0.0006", "65999.99")
	s.tick()
	if st := s.trader.State(); st.Strategy.Phase != strategy.PhaseCooldown || st.SellOrder != nil {
		t.Fatalf("verwacht COOLDOWN na volledige verkoop, kreeg %s", st.Strategy.Phase)
	}
}

func TestPartialBuyFillOpensSmallerPosition(t *testing.T) {
	p := newPath(t0, "60000").flat(60).move("0.1", 60).flat(60)
	s := newPartialSim(t, p, nil)

	for range 120 {
		s.tick()
		if len(s.ex.placed) > 0 {
			break
		}
	}
	if len(s.ex.placed) != 1 || s.ex.placed[0].Side != domain.SideBuy {
		t.Fatalf("verwacht één kooporder: %+v", s.ex.placed)
	}
	buyPrice := s.ex.placed[0].Price.String()

	s.ex.fill("0.0003", buyPrice)
	s.ex.balances["BTC"] = domain.Balance{Available: d("0.0003")}
	for range 10 {
		s.tick()
	}

	st := s.trader.State()
	if st.Strategy.Phase != strategy.PhaseInPosition || !st.Strategy.Position.EntryQuantity.Equal(d("0.0003")) {
		t.Fatalf("verwacht positie van 0,0003 BTC na deelvulling en timeout: %s %+v", st.Strategy.Phase, st.Strategy.Position)
	}
	if s.ex.orders["o-1"].Status != domain.OrderCancelled {
		t.Errorf("kooporder: %s, verwacht CANCELLED", s.ex.orders["o-1"].Status)
	}
	// kostprijs = (waarde + fee) / hoeveelheid
	value := d("0.0003").Mul(d(buyPrice))
	want := value.Add(value.Mul(d("0.0025"))).Div(d("0.0003"))
	if !st.Strategy.Position.CostPrice.Equal(want) {
		t.Errorf("kostprijs %s, verwacht %s", st.Strategy.Position.CostPrice, want)
	}
	if st.SellOrder == nil || !st.SellOrder.Size.Equal(d("0.00015")) {
		t.Errorf("verwacht winstorder voor de helft van de deelvulling: %+v", st.SellOrder)
	}
}

func TestCalculateResult(t *testing.T) {
	fills := []domain.Fill{
		{OrderID: "koop", Price: d("60000"), Size: d("0.001"), Fee: d("0.15")},
		{OrderID: "w1", Price: d("63759.40"), Size: d("0.0005"), Fee: d("0.0797")},
		{OrderID: "stop", Price: d("70000"), Size: d("0.0005"), Fee: d("0.175")},
	}
	side := map[string]domain.Side{"koop": domain.SideBuy, "w1": domain.SideSell, "stop": domain.SideSell}
	r := usecase.CalculateResult(fills, side)

	checks := map[string][2]decimal.Decimal{
		"ingelegd":  {r.Invested, d("60.15")},
		"opbrengst": {r.Proceeds, d("31.8797").Sub(d("0.0797")).Add(d("35")).Sub(d("0.175"))},
		"fees":      {r.Fees, d("0.4047")},
	}
	for name, c := range checks {
		if !c[0].Equal(c[1]) {
			t.Errorf("%s = %s, verwacht %s", name, c[0], c[1])
		}
	}
	if !r.Profit.Equal(r.Proceeds.Sub(r.Invested)) || !r.ProfitPct.IsPositive() {
		t.Errorf("resultaat onjuist: %+v", r)
	}
	if empty := usecase.CalculateResult(nil, nil); !empty.ProfitPct.IsZero() {
		t.Errorf("leeg resultaat: %+v", empty)
	}
}
