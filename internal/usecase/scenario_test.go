package usecase_test

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"tradingbot/internal/adapter/exchange/paper"
	"tradingbot/internal/adapter/storage/sqlite"
	"tradingbot/internal/domain"
	"tradingbot/internal/domain/indicator"
	"tradingbot/internal/domain/strategy"
	"tradingbot/internal/usecase"
)

// Scenariotests: de trader met de echte paper-exchange en SQLite, een nep-klok en een vaste
// prijsreeks. De strategie gebruikt kleine periodes (SMA2/SMA4 op 10-minuut candles) zodat een
// volledige cyclus in een paar honderd gesimuleerde minuten past.

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

var (
	t0      = time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	product = domain.Product{ID: "BTC-EUR", BaseIncrement: d("0.00000001"), QuoteIncrement: d("0.01"),
		BaseMinSize: d("0.00000001"), QuoteMinSize: d("1")}
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// path is een prijsreeks per minuut. Vóór het begin en na het einde geldt de eerste/laatste prijs.
type path struct {
	start  time.Time
	prices []decimal.Decimal
}

func newPath(start time.Time, price string) *path {
	return &path{start: start, prices: []decimal.Decimal{d(price)}}
}

func (p *path) last() decimal.Decimal { return p.prices[len(p.prices)-1] }

// flat houdt de prijs minutes minuten gelijk.
func (p *path) flat(minutes int) *path {
	for range minutes {
		p.prices = append(p.prices, p.last())
	}
	return p
}

// move verandert de prijs minutes minuten lang met pctPerMinute procent per minuut.
func (p *path) move(pctPerMinute string, minutes int) *path {
	factor := decimal.NewFromInt(1).Add(d(pctPerMinute).Div(decimal.NewFromInt(100)))
	for range minutes {
		p.prices = append(p.prices, p.last().Mul(factor).Round(2))
	}
	return p
}

// jump zet de prijs in één minuut op een veelvoud van de huidige prijs.
func (p *path) jump(factor string) *path {
	p.prices = append(p.prices, p.last().Mul(d(factor)).Round(2))
	return p
}

func (p *path) at(ts time.Time) decimal.Decimal {
	i := int(ts.Sub(p.start) / time.Minute)
	if i < 0 {
		return p.prices[0]
	}
	if i >= len(p.prices) {
		return p.last()
	}
	return p.prices[i]
}

// feed levert candles en ticker op basis van de prijsreeks. Een candle heeft een bandbreedte van
// ±0,2% rond de prijs; de ticker heeft een spread van één cent.
type feed struct {
	path  *path
	clock *clock
}

func (f *feed) Product(context.Context, string) (domain.Product, error) { return product, nil }

func (f *feed) Candles(_ context.Context, _ string, start, end time.Time) ([]domain.Candle, error) {
	var out []domain.Candle
	for ts := start; ts.Before(end); ts = ts.Add(time.Minute) {
		p := f.path.at(ts)
		out = append(out, domain.Candle{Start: ts, Open: p, Close: p, Volume: d("1"),
			High: p.Mul(d("1.002")).Round(2), Low: p.Mul(d("0.998")).Round(2)})
	}
	return out, nil
}

func (f *feed) Ticker(context.Context, string) (domain.Ticker, error) {
	p := f.path.at(f.clock.Now().Truncate(time.Minute))
	return domain.Ticker{Price: p, BestBid: p.Sub(d("0.01")), BestAsk: p.Add(d("0.01")), Time: f.clock.Now()}, nil
}

type mutableFees struct {
	mu   sync.Mutex
	fees strategy.Fees
}

func (m *mutableFees) Fees(context.Context) (strategy.Fees, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.fees, nil
}

func (m *mutableFees) set(f strategy.Fees) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fees = f
}

type notification struct {
	level usecase.NotificationLevel
	title string
	msg   string
}

type recorder struct{ list []notification }

func (r *recorder) Notify(_ context.Context, level usecase.NotificationLevel, title, msg string) {
	r.list = append(r.list, notification{level, title, msg})
}

func (r *recorder) count(title string) int {
	n := 0
	for _, x := range r.list {
		if strings.HasPrefix(x.title, title) {
			n++
		}
	}
	return n
}

// exchangeWrapper maakt het mogelijk saldi te vervalsen (afwijking simuleren).
type exchangeWrapper struct {
	usecase.Exchange
	missingBTC decimal.Decimal
}

func (w *exchangeWrapper) Balances(ctx context.Context) (map[string]domain.Balance, error) {
	b, err := w.Exchange.Balances(ctx)
	if err != nil {
		return nil, err
	}
	btc := b["BTC"]
	btc.Available = btc.Available.Sub(w.missingBTC)
	b["BTC"] = btc
	return b, nil
}

type sim struct {
	t        *testing.T
	ctx      context.Context
	clock    *clock
	path     *path
	feed     *feed
	db       *sqlite.DB
	fees     *mutableFees
	notes    *recorder
	paper    *paper.Exchange
	exchange *exchangeWrapper
	trader   *usecase.Trader
	machine  *strategy.Machine
	market   *usecase.MarketDataService
	logs     *strings.Builder
	budget   string
	start    string
}

func newSim(t *testing.T, p *path) *sim {
	t.Helper()
	db, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	c := &clock{now: p.start.Add(5 * time.Second)}
	s := &sim{t: t, ctx: context.Background(), clock: c, path: p, feed: &feed{path: p, clock: c}, db: db,
		fees:   &mutableFees{fees: strategy.Fees{Maker: d("0.0025"), Taker: d("0.005")}},
		notes:  &recorder{},
		logs:   &strings.Builder{},
		budget: "50", start: "100"}
	return s
}

// boot maakt (opnieuw) een paper-exchange en trader op dezelfde database, zoals bij een herstart.
func (s *sim) boot() {
	s.t.Helper()
	log := slog.New(slog.NewTextHandler(s.logs, nil))
	machine, err := strategy.NewMachine(strategy.Config{
		Trend: indicator.TrendConfig{ShortPeriod: 2, LongPeriod: 4, FlatThreshold: d("0.005")},
		TakeProfits: [2]strategy.TakeProfit{
			{NetProfit: d("0.06"), SellFraction: d("0.5")},
			{NetProfit: d("0.12"), SellFraction: d("0.65")},
		},
		TrailingDistance: d("0.05"),
		CooldownReversal: 3,
		CooldownRising:   1,
	}, product)
	if err != nil {
		s.t.Fatal(err)
	}
	market, err := usecase.NewMarketDataService(s.feed, s.db, s.clock, log, product.ID, 10*time.Minute, 4)
	if err != nil {
		s.t.Fatal(err)
	}
	s.machine, s.market = machine, market
	s.paper = paper.New(s.db, s.db, s.feed, s.fees, s.clock, product, d(s.start))
	missing := decimal.Zero
	if s.exchange != nil {
		missing = s.exchange.missingBTC
	}
	s.exchange = &exchangeWrapper{Exchange: s.paper, missingBTC: missing}
	s.trader, err = usecase.NewTrader(s.ctx, usecase.TraderConfig{
		Budget:           d(s.budget),
		BuyOrderTimeout:  10 * time.Minute,
		StopOrderTimeout: time.Minute,
		FeeRefresh:       time.Hour,
	}, usecase.TraderDeps{
		Product: product, Machine: machine, Market: market, Tickers: s.feed, Exchange: s.exchange,
		Fees: s.fees, Store: s.db, KV: s.db, Notifier: s.notes, Clock: s.clock, Log: log,
	})
	if err != nil {
		s.t.Fatal(err)
	}
}

// run laat de bot minutes minuten draaien, met één ronde per minuut.
func (s *sim) run(minutes int) {
	s.t.Helper()
	for range minutes {
		if err := s.trader.Tick(s.ctx); err != nil {
			s.t.Fatalf("ronde om %s: %v", s.clock.Now().Format("15:04"), err)
		}
		s.assertAtMostOneOrderPerSide()
		s.clock.set(s.clock.Now().Add(time.Minute))
	}
}

// runUntil laat de bot draaien tot cond waar is, met een maximum aantal minuten.
func (s *sim) runUntil(maxMinutes int, what string, cond func(usecase.TraderState) bool) {
	s.t.Helper()
	for range maxMinutes {
		s.run(1)
		if cond(s.trader.State()) {
			return
		}
	}
	st := s.trader.State()
	s.t.Fatalf("niet bereikt binnen %d minuten: %s (fase %s, om %s)", maxMinutes, what, st.Strategy.Phase, s.clock.Now().Format("15:04"))
}

func phaseIs(p strategy.Phase) func(usecase.TraderState) bool {
	return func(st usecase.TraderState) bool { return st.Strategy.Phase == p }
}

// assertAtMostOneOrderPerSide controleert dat de bot nooit twee open orders aan dezelfde kant heeft.
func (s *sim) assertAtMostOneOrderPerSide() {
	s.t.Helper()
	b, err := s.paper.Balances(s.ctx)
	if err != nil {
		s.t.Fatal(err)
	}
	st := s.trader.State()
	if b["BTC"].Hold.IsPositive() {
		if st.SellOrder == nil || !b["BTC"].Hold.Equal(st.SellOrder.Size) {
			s.t.Fatalf("BTC vast %s past niet bij de gevolgde verkooporder %+v", b["BTC"].Hold, st.SellOrder)
		}
	}
	if b["EUR"].Hold.IsPositive() && st.BuyOrder == nil {
		s.t.Fatalf("EUR vast %s zonder gevolgde kooporder", b["EUR"].Hold)
	}
}

func (s *sim) balances() map[string]domain.Balance {
	s.t.Helper()
	b, err := s.paper.Balances(s.ctx)
	if err != nil {
		s.t.Fatal(err)
	}
	return b
}

// risingMarket: 60 minuten vlak, daarna 0,1% per minuut stijgend (ruim genoeg tot +12%).
func risingMarket() *path {
	return newPath(t0, "60000").flat(60).move("0.1", 200)
}

func TestScenarioFullCycle(t *testing.T) {
	// vlak → stijgen tot ruim boven +12% → dalen door de trailing stop → verder dalen → weer stijgen
	p := newPath(t0, "60000").flat(60).move("0.1", 200).move("-0.2", 120).flat(30).move("0.1", 100)
	s := newSim(t, p)
	s.boot()

	s.run(60)
	if st := s.trader.State(); st.Strategy.Phase != strategy.PhaseSeekingEntry || st.BuyOrder != nil {
		t.Fatalf("vlakke markt: verwacht ZOEK_INSTAP zonder order, kreeg %s / %+v", st.Strategy.Phase, st.BuyOrder)
	}

	s.runUntil(60, "aankoop", phaseIs(strategy.PhaseInPosition))
	st := s.trader.State()
	entry := st.Strategy.Position.EntryQuantity
	if st.PositionID == 0 || st.SellOrder == nil || st.SellOrder.Purpose != usecase.PurposeTakeProfit1 {
		t.Fatalf("na aankoop verwacht positie + winstorder 1: %+v", st)
	}
	if invested := entry.Mul(st.Strategy.Position.CostPrice); invested.GreaterThan(d("50")) {
		t.Errorf("inleg %s overschrijdt budget", invested)
	}

	s.runUntil(120, "winststap 1", phaseIs(strategy.PhasePart1Sold))
	st = s.trader.State()
	if !st.Strategy.Position.Quantity.Equal(entry.Sub(product.RoundSizeDown(entry.Mul(d("0.5"))))) {
		t.Errorf("na stap 1: %s BTC over van %s", st.Strategy.Position.Quantity, entry)
	}

	s.runUntil(120, "winststap 2", phaseIs(strategy.PhaseTrailing))
	s.runUntil(200, "trailing stop en afronding", phaseIs(strategy.PhaseCooldown))

	st = s.trader.State()
	if st.Strategy.Cooldown != strategy.CooldownReversal {
		t.Errorf("exit in dalende markt: verwacht lange cooldown, kreeg %s", st.Strategy.Cooldown)
	}
	b := s.balances()
	if !b["BTC"].Total().IsZero() {
		t.Errorf("na afronding nog %s BTC", b["BTC"].Total())
	}
	if !b["EUR"].Available.GreaterThan(d("100")) {
		t.Errorf("verwacht winst, EUR = %s", b["EUR"].Available)
	}
	for _, title := range []string{"Gekocht", "Winst genomen (stap 1)", "Winst genomen (stap 2)", "Trailing stop geraakt", "Positie afgerond"} {
		if s.notes.count(title) != 1 {
			t.Errorf("melding %q: %d keer, verwacht 1", title, s.notes.count(title))
		}
	}

	// tijdens de daling en de vlakke periode wordt er niet gekocht; daarna wel weer
	s.runUntil(200, "nieuwe aankoop", phaseIs(strategy.PhaseInPosition))
	if s.notes.count("Gekocht") != 2 {
		t.Errorf("verwacht tweede aankoop")
	}
}

func TestScenarioPriceDropAfterBuyDoesNothing(t *testing.T) {
	p := newPath(t0, "60000").flat(60).move("0.1", 30).move("-0.3", 100).flat(200)
	s := newSim(t, p)
	s.boot()

	s.runUntil(90, "aankoop", phaseIs(strategy.PhaseInPosition))
	order := s.trader.State().SellOrder.OrderID
	s.run(300)

	st := s.trader.State()
	if st.Strategy.Phase != strategy.PhaseInPosition {
		t.Fatalf("verwacht IN_POSITIE na daling, kreeg %s", st.Strategy.Phase)
	}
	if st.SellOrder == nil || st.SellOrder.OrderID != order {
		t.Errorf("winstorder 1 hoort ongewijzigd te blijven staan: %+v", st.SellOrder)
	}
	if s.notes.count("Winst genomen") != 0 || s.notes.count("Positie afgerond") != 0 {
		t.Error("er is verkocht tijdens een daling")
	}
}

func TestScenarioRisingExitGivesShortCooldown(t *testing.T) {
	// na +12% even een scherpe dip (stop geraakt), daarna direct weer stijgen
	p := risingMarket().jump("0.93").move("0.15", 100)
	s := newSim(t, p)
	s.boot()

	s.runUntil(300, "trailing", phaseIs(strategy.PhaseTrailing))
	s.runUntil(100, "afronding", phaseIs(strategy.PhaseCooldown))
	s.runUntil(40, "herinstap", phaseIs(strategy.PhaseInPosition))
}

func TestScenarioInsufficientBalance(t *testing.T) {
	s := newSim(t, risingMarket())
	s.start = "30"
	s.boot()

	s.run(120)
	st := s.trader.State()
	if st.BuyOrder != nil || st.PositionID != 0 {
		t.Fatalf("verwacht geen order bij te weinig saldo: %+v", st)
	}
	if s.notes.count("Onvoldoende saldo") == 0 {
		t.Error("verwacht melding over onvoldoende saldo")
	}
}

func TestScenarioBuyOrderTimesOut(t *testing.T) {
	// na het koopsignaal springt de prijs omhoog en blijft stijgen: de koop-limit wordt nooit gevuld
	p := newPath(t0, "60000").flat(60).move("0.1", 20).jump("1.01").move("0.5", 60).flat(100)
	s := newSim(t, p)
	s.boot()

	s.runUntil(90, "kooporder", func(st usecase.TraderState) bool { return st.BuyOrder != nil })
	first := s.trader.State().BuyOrder.OrderID
	s.run(11)

	o, err := s.paper.Order(s.ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	if o.Status != domain.OrderCancelled {
		t.Errorf("kooporder na timeout: %s, verwacht CANCELLED", o.Status)
	}
	if st := s.trader.State(); st.PositionID != 0 || st.Strategy.Phase != strategy.PhaseSeekingEntry {
		t.Errorf("verwacht geen positie na timeout: %+v", st)
	}
	// bij de volgende candle mag er een nieuwe kooporder staan, maar er is niets gekocht
	if b := s.balances(); !b["EUR"].Total().Equal(d("100")) || !b["BTC"].Total().IsZero() {
		t.Errorf("na timeout: EUR %s, BTC %s", b["EUR"].Total(), b["BTC"].Total())
	}
}

func TestScenarioRestartRecoversMissedFills(t *testing.T) {
	s := newSim(t, risingMarket())
	s.boot()
	s.runUntil(90, "aankoop", phaseIs(strategy.PhaseInPosition))

	// bot ligt 3 uur stil terwijl de koers voorbij +6% en +12% stijgt
	s.clock.set(s.clock.Now().Add(3 * time.Hour))
	s.boot()
	s.run(1)
	if st := s.trader.State(); st.Strategy.Phase != strategy.PhasePart1Sold || st.SellOrder == nil ||
		st.SellOrder.Purpose != usecase.PurposeTakeProfit2 {
		t.Fatalf("na herstart verwacht DEEL1_VERKOCHT met winstorder 2, kreeg %s / %+v", st.Strategy.Phase, st.SellOrder)
	}
	s.run(2)
	if st := s.trader.State(); st.Strategy.Phase != strategy.PhaseTrailing {
		t.Fatalf("verwacht TRAILING, kreeg %s", st.Strategy.Phase)
	}
}

func TestScenarioRestartDuringTrailingReconstructsHigh(t *testing.T) {
	p := risingMarket().move("0.1", 60).move("-0.2", 60)
	s := newSim(t, p)
	s.boot()
	s.runUntil(300, "trailing", phaseIs(strategy.PhaseTrailing))
	before := s.trader.State().Strategy.TrailingHigh

	// bot ligt 4 uur stil: de koers stijgt eerst verder en zakt daarna door de stop
	s.clock.set(s.clock.Now().Add(4 * time.Hour))
	s.boot()
	s.run(1)
	st := s.trader.State()
	if !st.Strategy.TrailingHigh.GreaterThan(before) {
		t.Errorf("hoogste koers niet gereconstrueerd: %s (was %s)", st.Strategy.TrailingHigh, before)
	}
	if !st.Strategy.StopTriggered && st.Strategy.Phase == strategy.PhaseTrailing {
		t.Error("stop had geraakt moeten zijn na de daling tijdens downtime")
	}
}

func TestScenarioRepeatedRestartsDoNotDuplicateOrders(t *testing.T) {
	s := newSim(t, risingMarket())
	s.boot()
	s.runUntil(90, "aankoop", phaseIs(strategy.PhaseInPosition))
	order := s.trader.State().SellOrder.OrderID

	for range 5 {
		s.boot()
		s.run(1) // run controleert dat er maar één verkooporder openstaat
	}
	if got := s.trader.State().SellOrder; got == nil || got.OrderID != order {
		t.Errorf("winstorder vervangen of verdwenen na herstarts: %+v", got)
	}
}

func TestScenarioOrderCancelledOutsideBotHalts(t *testing.T) {
	s := newSim(t, risingMarket())
	s.boot()
	s.runUntil(90, "aankoop", phaseIs(strategy.PhaseInPosition))

	if err := s.paper.CancelOrder(s.ctx, s.trader.State().SellOrder.OrderID); err != nil {
		t.Fatal(err)
	}
	s.run(3)
	st := s.trader.State()
	if !st.Halted || st.SellOrder != nil {
		t.Fatalf("verwacht stilstand zonder nieuwe order: %+v", st)
	}
	if s.notes.count("Bot gepauzeerd door afwijking") != 1 {
		t.Error("verwacht één melding over de afwijking")
	}

	if err := s.trader.Resume(s.ctx); err != nil {
		t.Fatal(err)
	}
	s.run(1)
	if st := s.trader.State(); st.Halted || st.SellOrder == nil {
		t.Errorf("na hervatten verwacht een nieuwe winstorder: %+v", st)
	}
}

// flatAfterBuy: kort stijgen (koopsignaal) en daarna vlak, zodat de winstorder blijft staan.
func flatAfterBuy() *path {
	return newPath(t0, "60000").flat(60).move("0.1", 30).flat(300)
}

func TestScenarioBalanceMismatchHalts(t *testing.T) {
	s := newSim(t, flatAfterBuy())
	s.boot()
	s.runUntil(90, "aankoop", phaseIs(strategy.PhaseInPosition))

	// iemand verkoopt handmatig BTC: de winstorder wordt door de bot vervangen (fee-wijziging),
	// maar dan blijkt er te weinig BTC te zijn
	s.exchange.missingBTC = d("0.0001")
	s.fees.set(strategy.Fees{Maker: d("0.004"), Taker: d("0.006")})
	s.clock.set(s.clock.Now().Add(time.Hour))
	s.run(1)

	st := s.trader.State()
	if !st.Halted || st.SellOrder != nil {
		t.Fatalf("verwacht stilstand zonder nieuwe verkooporder: %+v", st)
	}
	if !strings.Contains(st.PauseReason, "vrij beschikbaar") {
		t.Errorf("reden = %q", st.PauseReason)
	}
}

func TestScenarioFeeChangeReplacesTakeProfit(t *testing.T) {
	s := newSim(t, flatAfterBuy())
	s.boot()
	s.runUntil(90, "aankoop", phaseIs(strategy.PhaseInPosition))
	old := *s.trader.State().SellOrder

	s.fees.set(strategy.Fees{Maker: d("0.004"), Taker: d("0.006")})
	s.clock.set(s.clock.Now().Add(time.Hour)) // fees worden elk uur opgehaald
	s.run(1)

	st := s.trader.State()
	if st.SellOrder == nil || st.SellOrder.OrderID == old.OrderID || !st.SellOrder.Price.GreaterThan(old.Price) {
		t.Fatalf("verwacht nieuwe winstorder met hogere prijs: oud %+v, nieuw %+v", old, st.SellOrder)
	}
	o, _ := s.paper.Order(s.ctx, old.OrderID)
	if o.Status != domain.OrderCancelled {
		t.Errorf("oude winstorder: %s, verwacht CANCELLED", o.Status)
	}
	if s.notes.count("Fees gewijzigd") != 1 || st.Halted {
		t.Errorf("verwacht één fee-melding en geen stilstand (halted=%v)", st.Halted)
	}
}

func TestScenarioPauseBlocksBuysOnly(t *testing.T) {
	s := newSim(t, risingMarket())
	s.boot()
	if err := s.trader.Pause(s.ctx, "test"); err != nil {
		t.Fatal(err)
	}
	s.run(120)
	if st := s.trader.State(); st.BuyOrder != nil || st.PositionID != 0 {
		t.Fatalf("gepauzeerde bot heeft gekocht: %+v", st)
	}

	if err := s.trader.Resume(s.ctx); err != nil {
		t.Fatal(err)
	}
	s.runUntil(30, "aankoop na hervatten", phaseIs(strategy.PhaseInPosition))

	// pauze tijdens een positie: de winstorders lopen gewoon door
	if err := s.trader.Pause(s.ctx, "test"); err != nil {
		t.Fatal(err)
	}
	s.runUntil(120, "winststap 1 tijdens pauze", phaseIs(strategy.PhasePart1Sold))
}

func TestScenarioNotificationsAndProgressLog(t *testing.T) {
	p := newPath(t0, "60000").flat(60).move("0.1", 200).move("-0.2", 120)
	s := newSim(t, p)
	s.boot()
	s.runUntil(700, "afronding", phaseIs(strategy.PhaseCooldown))

	for title, want := range map[string]int{
		"Kooporder geplaatst":         1,
		"Winstorder stap 1 geplaatst": 1,
		"Winstorder stap 2 geplaatst": 1,
		"Stopverkoop geplaatst":       1,
	} {
		if got := s.notes.count(title); got != want {
			t.Errorf("melding %q: %d keer, verwacht %d", title, got, want)
		}
	}
	// WACHT_OP_DATA → ZOEK_INSTAP → IN_POSITIE → DEEL1_VERKOCHT → TRAILING → COOLDOWN
	var phases []string
	for _, n := range s.notes.list {
		if n.title == "Toestand gewijzigd" {
			phases = append(phases, n.msg)
		}
	}
	if len(phases) != 5 || !strings.Contains(phases[4], "COOLDOWN (lang cooldown)") {
		t.Errorf("toestandsmeldingen = %q", phases)
	}

	// bij elke fill een logregel met de bijgewerkte winst
	var fillLines []string
	for _, line := range strings.Split(s.logs.String(), "\n") {
		if strings.Contains(line, `msg="fill verwerkt"`) {
			fillLines = append(fillLines, line)
		}
	}
	if len(fillLines) != 4 { // koop, winst 1, winst 2, stop
		t.Fatalf("fill-regels = %d, verwacht 4:\n%s", len(fillLines), strings.Join(fillLines, "\n"))
	}
	for _, field := range []string{"positie=1", "gerealiseerd=", "ongerealiseerd=", "totaal=", "restant="} {
		if !strings.Contains(fillLines[1], field) {
			t.Errorf("fill-regel mist %s: %s", field, fillLines[1])
		}
	}
	if !strings.Contains(fillLines[3], "restant=0 ") || !strings.Contains(fillLines[3], "ongerealiseerd=0.00") {
		t.Errorf("laatste fill-regel: %s", fillLines[3])
	}
}

func TestScenarioTimeoutIsNotified(t *testing.T) {
	p := newPath(t0, "60000").flat(60).move("0.1", 20).jump("1.01").move("0.5", 60).flat(100)
	s := newSim(t, p)
	s.boot()
	s.runUntil(90, "kooporder", func(st usecase.TraderState) bool { return st.BuyOrder != nil })
	s.run(11)
	if s.notes.count("Order geannuleerd") != 1 {
		t.Errorf("verwacht één melding over de geannuleerde kooporder")
	}
}
