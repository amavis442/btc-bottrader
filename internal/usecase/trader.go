package usecase

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"tradingbot/internal/domain"
	"tradingbot/internal/domain/indicator"
	"tradingbot/internal/domain/strategy"
)

const (
	traderStateKey = "trader"
	// maxBackfill is hoe ver de bot na downtime maximaal terug aanvult.
	maxBackfill = 7 * 24 * time.Hour
)

// TraderConfig bevat de instellingen van de trader.
type TraderConfig struct {
	Budget           decimal.Decimal // vaste inleg per aankoop, inclusief fee
	BuyLimitOffset   decimal.Decimal // fractie onder de laatste prijs voor de koop-limit
	BuyOrderTimeout  time.Duration
	StopOrderTimeout time.Duration
	FeeRefresh       time.Duration
	MaxCatchUp       int // maximaal aantal gemiste strategie-candles dat wordt nagelopen
}

// ActiveOrder is een order die de trader volgt. ClientOrderID wordt vastgelegd vóórdat de order
// geplaatst wordt; is OrderID nog leeg, dan wordt het plaatsen (idempotent) herhaald.
type ActiveOrder struct {
	ClientOrderID   string          `json:"client_order_id"`
	OrderID         string          `json:"order_id"`
	Purpose         OrderPurpose    `json:"purpose"`
	Side            domain.Side     `json:"side"`
	Size            decimal.Decimal `json:"size"`
	Price           decimal.Decimal `json:"price"`
	PlacedAt        time.Time       `json:"placed_at"`
	CancelRequested bool            `json:"cancel_requested"`
	Applied         decimal.Decimal `json:"applied"` // gevulde hoeveelheid die al verwerkt is
}

// TraderState is de volledige, opgeslagen toestand van de trader.
type TraderState struct {
	InstanceID    string          `json:"instance_id"`
	OrderSeq      int64           `json:"order_seq"`
	Strategy      strategy.State  `json:"strategy"`
	PositionID    int64           `json:"position_id"`
	BuyOrder      *ActiveOrder    `json:"buy_order"`
	SellOrder     *ActiveOrder    `json:"sell_order"`
	Paused        bool            `json:"paused"` // door de gebruiker: geen nieuwe aankopen
	Halted        bool            `json:"halted"` // door een afwijking: helemaal geen orders
	PauseReason   string          `json:"pause_reason"`
	LastCandle    time.Time       `json:"last_candle"`    // start van de laatst verwerkte strategie-candle
	LastPriceAt   time.Time       `json:"last_price_at"`  // minuut-candles vóór dit moment zijn verwerkt
	TrailingSince time.Time       `json:"trailing_since"` // begin van de trailing-fase
	Fees          strategy.Fees   `json:"fees"`
	FeesUpdatedAt time.Time       `json:"fees_updated_at"`
	LastPrice     decimal.Decimal `json:"last_price"`
	LastBid       decimal.Decimal `json:"last_bid"`
	LastPriceTime time.Time       `json:"last_price_time"`
	LastTrend     indicator.Trend `json:"last_trend"`
}

// Trader voert de strategie uit. Alle acties lopen via Tick, die nooit gelijktijdig draait.
type Trader struct {
	mu       sync.Mutex
	cfg      TraderConfig
	product  domain.Product
	machine  *strategy.Machine
	market   *MarketDataService
	tickers  TickerSource
	exchange Exchange
	fees     FeeSource
	store    TradingStore
	kv       KeyValueStore
	notifier Notifier
	clock    Clock
	log      *slog.Logger

	state     TraderState
	ticker    domain.Ticker // ticker van de lopende tick
	recovered bool          // gemiste periode na de start is aangevuld
}

// TraderDeps bundelt de afhankelijkheden van de trader.
type TraderDeps struct {
	Product  domain.Product
	Machine  *strategy.Machine
	Market   *MarketDataService
	Tickers  TickerSource
	Exchange Exchange
	Fees     FeeSource
	Store    TradingStore
	KV       KeyValueStore
	Notifier Notifier
	Clock    Clock
	Log      *slog.Logger
}

// NewTrader maakt een trader en laadt de opgeslagen toestand (of begint opnieuw).
func NewTrader(ctx context.Context, cfg TraderConfig, deps TraderDeps) (*Trader, error) {
	if !cfg.Budget.IsPositive() {
		return nil, errors.New("trader: budget moet positief zijn")
	}
	if cfg.MaxCatchUp <= 0 {
		cfg.MaxCatchUp = 200
	}
	t := &Trader{cfg: cfg, product: deps.Product, machine: deps.Machine, market: deps.Market,
		tickers: deps.Tickers, exchange: deps.Exchange, fees: deps.Fees, store: deps.Store, kv: deps.KV,
		notifier: deps.Notifier, clock: deps.Clock, log: deps.Log}

	data, ok, err := t.kv.GetValue(ctx, traderStateKey)
	if err != nil {
		return nil, fmt.Errorf("trader: toestand laden: %w", err)
	}
	if ok {
		if err := json.Unmarshal(data, &t.state); err != nil {
			return nil, fmt.Errorf("trader: toestand ontleden: %w", err)
		}
		t.log.Info("toestand hersteld", "fase", t.state.Strategy.Phase, "positie", t.state.PositionID,
			"gepauzeerd", t.state.Paused || t.state.Halted)
		return t, nil
	}

	id := make([]byte, 4)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	t.state = TraderState{InstanceID: hex.EncodeToString(id), Strategy: strategy.NewState()}
	return t, t.save(ctx)
}

// State geeft een kopie van de huidige toestand.
func (t *Trader) State() TraderState {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.state
	if s.Strategy.Position != nil {
		p := *s.Strategy.Position
		s.Strategy.Position = &p
	}
	if s.BuyOrder != nil {
		o := *s.BuyOrder
		s.BuyOrder = &o
	}
	if s.SellOrder != nil {
		o := *s.SellOrder
		s.SellOrder = &o
	}
	return s
}

// Pause stopt nieuwe aankopen. Een lopende positie wordt nog afgehandeld.
func (t *Trader) Pause(ctx context.Context, reason string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.state.Paused = true
	t.state.PauseReason = reason
	t.event(ctx, "PAUZE", reason)
	return t.save(ctx)
}

// Resume heft een pauze of een stop door een afwijking op.
func (t *Trader) Resume(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.state.Paused, t.state.Halted, t.state.PauseReason = false, false, ""
	t.event(ctx, "HERVAT", "bot hervat")
	return t.save(ctx)
}

// Tick voert één ronde uit: marktdata bijwerken, orders controleren, de strategie laten beslissen
// en de gevraagde acties uitvoeren. Wordt elke minuut aangeroepen.
func (t *Trader) Tick(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if err := t.backfill(ctx); err != nil {
		return err
	}
	if _, err := t.market.Sync(ctx); err != nil {
		return err
	}
	ticker, err := t.tickers.Ticker(ctx, t.product.ID)
	if err != nil {
		return fmt.Errorf("trader: ticker: %w", err)
	}
	t.ticker = ticker
	t.state.LastPrice, t.state.LastBid, t.state.LastPriceTime = ticker.Price, ticker.BestBid, t.now()
	startPhase := t.state.Strategy.Phase
	defer func() { t.notifyPhaseChange(ctx, startPhase) }()

	steps := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"fees", t.refreshFees},
		{"orders", t.reconcileOrders},
		{"prijzen", t.processPrices},
		{"candles", t.processCandles},
		{"afronden", t.finishPosition},
		{"winstorders", t.ensureSellOrders},
	}
	for _, step := range steps {
		if err := step.fn(ctx); err != nil {
			if saveErr := t.save(ctx); saveErr != nil {
				t.log.Error("toestand opslaan mislukt", "fout", saveErr)
			}
			return fmt.Errorf("trader: %s: %w", step.name, err)
		}
	}
	return t.save(ctx)
}

func (t *Trader) now() time.Time { return t.clock.Now().UTC() }

// backfill vult na een (her)start de candles aan vanaf de laatste ronde vóór de downtime. Die zijn
// nodig om de trailing stop te reconstrueren en om gemiste vullingen te bepalen.
func (t *Trader) backfill(ctx context.Context) error {
	if t.recovered {
		return nil
	}
	if from := t.state.LastPriceAt; !from.IsZero() {
		if limit := t.now().Add(-maxBackfill); from.Before(limit) {
			t.log.Warn("downtime langer dan het maximum, alleen de laatste periode wordt aangevuld", "maximum", maxBackfill)
			from = limit
		}
		n, err := t.market.SyncFrom(ctx, from)
		if err != nil {
			return fmt.Errorf("trader: aanvullen na herstart: %w", err)
		}
		t.event(ctx, "HERSTEL", fmt.Sprintf("%d candles aangevuld vanaf %s", n, from.Format(time.RFC3339)))
	}
	t.recovered = true
	return nil
}

func (t *Trader) save(ctx context.Context) error {
	data, err := json.Marshal(t.state)
	if err != nil {
		return err
	}
	return t.kv.SetValue(ctx, traderStateKey, data)
}

func (t *Trader) event(ctx context.Context, kind, message string) {
	t.log.Info(message, "soort", kind)
	if err := t.store.AddEvent(ctx, t.now(), kind, message); err != nil {
		t.log.Error("event opslaan mislukt", "fout", err)
	}
}

func (t *Trader) notify(ctx context.Context, level NotificationLevel, title, message string) {
	t.event(ctx, "MELDING_"+string(level), title+": "+message)
	t.notifier.Notify(ctx, level, title, message)
}

// notifyPhaseChange meldt een toestandsovergang die in deze ronde heeft plaatsgevonden.
func (t *Trader) notifyPhaseChange(ctx context.Context, from strategy.Phase) {
	st := t.state.Strategy
	if st.Phase == from {
		return
	}
	msg := fmt.Sprintf("%s → %s", from, st.Phase)
	if st.Phase == strategy.PhaseCooldown {
		msg += fmt.Sprintf(" (%s cooldown)", strings.ToLower(string(st.Cooldown)))
	}
	t.notify(ctx, NotifyInfo, "Toestand gewijzigd", msg)
}

// halt stopt alle orderacties na een afwijking, tot de gebruiker de bot hervat.
func (t *Trader) halt(ctx context.Context, reason string) {
	t.state.Halted = true
	t.state.PauseReason = reason
	t.notify(ctx, NotifyError, "Bot gepauzeerd door afwijking", reason+" Controleer het account en hervat de bot via de API.")
}

// refreshFees haalt de fee-tarieven op als dat nodig is. Bij een wijziging wordt een rustende
// winstorder zonder vullingen geannuleerd, zodat hij met de nieuwe doelprijs opnieuw geplaatst wordt.
func (t *Trader) refreshFees(ctx context.Context) error {
	now := t.now()
	if !t.state.FeesUpdatedAt.IsZero() && now.Sub(t.state.FeesUpdatedAt) < t.cfg.FeeRefresh {
		return nil
	}
	fees, err := t.fees.Fees(ctx)
	if err != nil {
		if t.state.FeesUpdatedAt.IsZero() {
			return err // zonder fees kan er niet gerekend worden
		}
		t.log.Warn("fees ophalen mislukt, vorige tarieven blijven gelden", "fout", err)
		return nil
	}
	old := t.state.Fees
	t.state.Fees, t.state.FeesUpdatedAt = fees, now
	if old.Maker.Equal(fees.Maker) && old.Taker.Equal(fees.Taker) {
		return nil
	}
	if old.Maker.IsZero() && old.Taker.IsZero() {
		t.event(ctx, "FEES", fmt.Sprintf("fee-tarieven: maker %s%%, taker %s%%", pct(fees.Maker), pct(fees.Taker)))
		return nil
	}
	t.notify(ctx, NotifyInfo, "Fees gewijzigd", fmt.Sprintf("maker %s%% → %s%%, taker %s%% → %s%%",
		pct(old.Maker), pct(fees.Maker), pct(old.Taker), pct(fees.Taker)))

	so := t.state.SellOrder
	if so == nil || so.OrderID == "" || so.Purpose == PurposeStop || t.state.Halted {
		return nil
	}
	action, ok := t.machine.TakeProfitAction(t.state.Strategy, fees)
	if !ok || action.Price.Equal(so.Price) {
		return nil
	}
	ord, err := t.exchange.Order(ctx, so.OrderID)
	if err != nil {
		return err
	}
	if ord.Status == domain.OrderOpen && ord.FilledSize.IsZero() {
		if err := t.exchange.CancelOrder(ctx, so.OrderID); err != nil {
			return err
		}
		so.CancelRequested = true
		t.notify(ctx, NotifyInfo, "Winstorder geannuleerd", fmt.Sprintf("order %s tegen %s wordt vervangen door een order tegen %s (nieuwe fees)",
			so.OrderID, so.Price, action.Price))
	}
	return nil
}

// reconcileOrders werkt de gevolgde orders bij met de status op de exchange.
func (t *Trader) reconcileOrders(ctx context.Context) error {
	if err := t.reconcile(ctx, &t.state.BuyOrder); err != nil {
		return err
	}
	return t.reconcile(ctx, &t.state.SellOrder)
}

func (t *Trader) reconcile(ctx context.Context, slot **ActiveOrder) error {
	a := *slot
	if a == nil {
		return nil
	}
	if a.OrderID == "" {
		// Het plaatsen is eerder niet bevestigd: idempotent opnieuw proberen.
		if t.state.Halted {
			return nil
		}
		if _, err := t.place(ctx, a); err != nil {
			if errors.Is(err, ErrInsufficientFunds) {
				*slot = nil
				t.notify(ctx, NotifyWarning, "Order niet geplaatst", err.Error())
				return nil
			}
			return err
		}
	}

	ord, err := t.exchange.Order(ctx, a.OrderID)
	if err != nil {
		return fmt.Errorf("order %s opvragen: %w", a.OrderID, err)
	}

	if ord.Status == domain.OrderOpen && !a.CancelRequested {
		timeout := time.Duration(0)
		switch a.Purpose {
		case PurposeBuy:
			timeout = t.cfg.BuyOrderTimeout
		case PurposeStop:
			timeout = t.cfg.StopOrderTimeout
		}
		if timeout > 0 && t.now().Sub(a.PlacedAt) >= timeout {
			if err := t.exchange.CancelOrder(ctx, a.OrderID); err != nil {
				return fmt.Errorf("order %s annuleren: %w", a.OrderID, err)
			}
			a.CancelRequested = true
			t.notify(ctx, NotifyInfo, "Order geannuleerd", fmt.Sprintf("%s-order %s niet binnen %s gevuld (gevuld: %s van %s BTC)",
				a.Purpose, a.OrderID, timeout, ord.FilledSize, ord.Size))
			if ord, err = t.exchange.Order(ctx, a.OrderID); err != nil {
				return fmt.Errorf("order %s opvragen: %w", a.OrderID, err)
			}
		}
	}

	if err := t.store.SaveOrder(ctx, OrderRecord{Order: ord, PositionID: t.state.PositionID, Purpose: a.Purpose}); err != nil {
		return err
	}
	if ord.FilledSize.GreaterThan(a.Applied) || ord.Status.Done() {
		fills, err := t.exchange.Fills(ctx, ord.ID)
		if err != nil {
			return fmt.Errorf("fills van %s opvragen: %w", ord.ID, err)
		}
		if err := t.store.SaveFills(ctx, fills); err != nil {
			return err
		}
		defer t.logProgress(ctx, ord, a.Purpose)
	}

	switch a.Purpose {
	case PurposeBuy:
		return t.onBuyUpdate(ctx, slot, ord)
	case PurposeTakeProfit1, PurposeTakeProfit2:
		return t.onTakeProfitUpdate(ctx, slot, ord)
	case PurposeStop:
		return t.onStopUpdate(ctx, slot, ord)
	}
	return fmt.Errorf("onbekend orderdoel %q", a.Purpose)
}

func (t *Trader) onBuyUpdate(ctx context.Context, slot **ActiveOrder, ord domain.Order) error {
	if !ord.Status.Done() {
		return nil
	}
	*slot = nil
	if ord.FilledSize.IsZero() {
		t.event(ctx, "KOOP_VERLOPEN", fmt.Sprintf("kooporder %s zonder vulling geannuleerd", ord.ID))
		return nil
	}

	fills, err := t.store.OrderFills(ctx, []string{ord.ID})
	if err != nil {
		return err
	}
	costPrice, err := strategy.CostPrice(toStrategyFills(fills))
	if err != nil {
		return err
	}
	s, _, err := t.machine.OnBuyFilled(t.state.Strategy, ord.FilledSize, costPrice, t.state.Fees)
	if err != nil {
		t.halt(ctx, fmt.Sprintf("Kooporder %s gevuld in onverwachte toestand: %v.", ord.ID, err))
		return nil
	}
	id, err := t.store.OpenPosition(ctx, PositionRecord{ProductID: t.product.ID, OpenedAt: ord.DoneAt,
		EntryQuantity: ord.FilledSize, CostPrice: costPrice})
	if err != nil {
		return err
	}
	if err := t.store.AssignOrderToPosition(ctx, ord.ID, id); err != nil {
		return err
	}
	t.state.Strategy, t.state.PositionID = s, id
	t.notify(ctx, NotifyInfo, "Gekocht", fmt.Sprintf("%s BTC voor %s (kostprijs %s per BTC incl. fee), positie %d",
		ord.FilledSize, ord.FilledValue.Add(ord.Fee).StringFixed(2), costPrice.StringFixed(2), id))
	return nil
}

func (t *Trader) onTakeProfitUpdate(ctx context.Context, slot **ActiveOrder, ord domain.Order) error {
	if !ord.Status.Done() {
		return nil
	}
	a := *slot
	*slot = nil
	step := 1
	if a.Purpose == PurposeTakeProfit2 {
		step = 2
	}

	if ord.Status == domain.OrderCancelled {
		if !a.CancelRequested {
			t.halt(ctx, fmt.Sprintf("Winstorder %s is buiten de bot om geannuleerd.", ord.ID))
		}
		if ord.FilledSize.IsPositive() {
			s, err := t.machine.OnPartialTakeProfit(t.state.Strategy, ord.FilledSize)
			if err != nil {
				return err
			}
			t.state.Strategy = s
		}
		return nil
	}

	s, _, err := t.machine.OnTakeProfitFilled(t.state.Strategy, step, ord.FilledSize, ord.AveragePrice(), t.state.Fees)
	if err != nil {
		t.halt(ctx, fmt.Sprintf("Winstorder %s gevuld in onverwachte toestand: %v.", ord.ID, err))
		return nil
	}
	t.state.Strategy = s
	if s.Phase == strategy.PhaseTrailing {
		t.state.TrailingSince = ord.DoneAt
		if t.state.TrailingSince.IsZero() {
			t.state.TrailingSince = t.now()
		}
	}
	t.notify(ctx, NotifyInfo, fmt.Sprintf("Winst genomen (stap %d)", step), fmt.Sprintf(
		"%s BTC verkocht voor %s per BTC, opbrengst %s na fee. Nog %s BTC over.",
		ord.FilledSize, ord.AveragePrice().StringFixed(2), ord.FilledValue.Sub(ord.Fee).StringFixed(2), remaining(s)))
	return nil
}

func (t *Trader) onStopUpdate(ctx context.Context, slot **ActiveOrder, ord domain.Order) error {
	a := *slot
	if newly := ord.FilledSize.Sub(a.Applied); newly.IsPositive() {
		s, err := t.machine.OnRemainingSold(t.state.Strategy, newly)
		if err != nil {
			t.halt(ctx, fmt.Sprintf("Stopverkoop %s verwerkt in onverwachte toestand: %v.", ord.ID, err))
			return nil
		}
		t.state.Strategy = s
		a.Applied = ord.FilledSize
	}
	if ord.Status.Done() {
		*slot = nil
		if ord.Status == domain.OrderCancelled && !a.CancelRequested {
			t.halt(ctx, fmt.Sprintf("Stopverkoop %s is buiten de bot om geannuleerd.", ord.ID))
		}
	}
	return nil
}

// logProgress schrijft na een (deel)vulling een logregel met de bijgewerkte winst van de positie.
func (t *Trader) logProgress(ctx context.Context, ord domain.Order, purpose OrderPurpose) {
	attrs := []any{"order", ord.ID, "doel", purpose, "status", ord.Status, "gevuld", ord.FilledSize,
		"gemiddelde_prijs", ord.AveragePrice().StringFixed(2), "fee", ord.Fee}
	if t.state.PositionID != 0 {
		r, err := t.positionResult(ctx, t.state.PositionID)
		if err != nil {
			t.log.Warn("winst van de positie berekenen mislukt", "fout", err)
			return
		}
		remaining := r.Bought.Sub(r.Sold)
		unrealized := decimal.Zero
		if remaining.IsPositive() && r.Bought.IsPositive() {
			value := remaining.Mul(t.ticker.BestBid).Mul(decimal.NewFromInt(1).Sub(t.state.Fees.Maker))
			unrealized = value.Sub(r.Invested.Mul(remaining).Div(r.Bought))
		}
		attrs = append(attrs, "positie", t.state.PositionID, "restant", remaining,
			"gerealiseerd", r.Realized.StringFixed(2), "ongerealiseerd", unrealized.StringFixed(2),
			"totaal", r.Realized.Add(unrealized).StringFixed(2), "fees", r.Fees.StringFixed(2))
	}
	t.log.Info("fill verwerkt", attrs...)
}

// processPrices verwerkt de koersen sinds de vorige ronde. In TRAILING schuift de stop mee met de
// slotkoers van elke minuut (ook die van een periode dat de bot uit stond) en de actuele koers.
func (t *Trader) processPrices(ctx context.Context) error {
	now := t.now()
	defer func() { t.state.LastPriceAt = now.Truncate(time.Minute) }()
	if t.state.Strategy.Phase != strategy.PhaseTrailing {
		return nil
	}

	from := t.state.LastPriceAt
	if t.state.TrailingSince.After(from) {
		from = t.state.TrailingSince
	}
	candles, err := t.market.MinuteCandles(ctx, from, now)
	if err != nil {
		return err
	}
	prices := domain.Closes(candles)
	prices = append(prices, t.ticker.Price)

	for _, p := range prices {
		before := t.state.Strategy
		s, actions := t.machine.OnPrice(t.state.Strategy, p)
		t.state.Strategy = s
		if len(actions) > 0 {
			t.notify(ctx, NotifyInfo, "Trailing stop geraakt", fmt.Sprintf("koers %s ≤ stop %s (hoogste koers %s); restant wordt verkocht",
				p.StringFixed(2), t.machine.TrailingStop(before).StringFixed(2), before.TrailingHigh.StringFixed(2)))
			break
		}
	}
	return nil
}

// processCandles laat de strategie elke nieuw afgesloten strategie-candle verwerken. Na downtime
// worden gemiste candles nagelopen (voor de cooldown), maar alleen de nieuwste candle mag tot een
// aankoop leiden.
func (t *Trader) processCandles(ctx context.Context) error {
	history := t.market.History()
	interval := t.market.Interval()
	missed := 1
	if !t.state.LastCandle.IsZero() {
		missed = int(t.now().Truncate(interval).Sub(t.state.LastCandle)/interval) - 1
	}
	missed = max(1, min(missed, t.cfg.MaxCatchUp))

	candles, err := t.market.StrategyCandles(ctx, history+missed)
	if err != nil {
		return err
	}
	closes := domain.Closes(candles)
	for i, c := range candles {
		if !c.Start.After(t.state.LastCandle) {
			continue
		}
		t.state.LastCandle = c.Start
		if i+1 < history {
			continue // te weinig historie voor deze candle
		}
		window := closes[:i+1]
		if trend, err := t.machine.Trend(window); err == nil {
			t.state.LastTrend = trend
		}

		s, actions, err := t.machine.OnCandleClosed(t.state.Strategy, window)
		if err != nil {
			return err
		}
		t.state.Strategy = s
		t.log.Info("candle afgesloten", "start", c.Start, "slot", c.Close, "trend", t.state.LastTrend, "fase", s.Phase)

		if i != len(candles)-1 {
			continue
		}
		for _, a := range actions {
			if a.Kind == strategy.ActionBuy {
				if err := t.placeBuy(ctx); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// placeBuy plaatst de kooporder na de controles uit het plan (2.4a).
func (t *Trader) placeBuy(ctx context.Context) error {
	switch {
	case t.state.Halted || t.state.Paused:
		t.log.Info("koopsignaal genegeerd: bot is gepauzeerd", "reden", t.state.PauseReason)
		return nil
	case t.state.Strategy.Phase != strategy.PhaseSeekingEntry:
		return nil
	case t.state.BuyOrder != nil || t.state.SellOrder != nil || t.state.PositionID != 0:
		return nil // er loopt al een order of positie
	}

	balances, err := t.exchange.Balances(ctx)
	if err != nil {
		return err
	}
	_, quote := domain.Currencies(t.product.ID)
	available := balances[quote].Available
	if available.LessThan(t.cfg.Budget) {
		t.notify(ctx, NotifyWarning, "Onvoldoende saldo", fmt.Sprintf("koopsignaal, maar %s %s beschikbaar en de inleg is %s",
			available.StringFixed(2), quote, t.cfg.Budget.StringFixed(2)))
		return nil
	}

	price := t.product.RoundPriceDown(t.ticker.Price.Mul(decimal.NewFromInt(1).Sub(t.cfg.BuyLimitOffset)))
	size := strategy.BuySize(t.cfg.Budget, price, t.state.Fees, t.product)
	if size.IsZero() {
		t.notify(ctx, NotifyWarning, "Order niet geplaatst", fmt.Sprintf("inleg %s is te klein voor een order tegen %s", t.cfg.Budget, price))
		return nil
	}

	a := t.newOrder(PurposeBuy, domain.SideBuy, size, price)
	t.state.BuyOrder = a
	if _, err := t.place(ctx, a); err != nil {
		if errors.Is(err, ErrInsufficientFunds) {
			t.state.BuyOrder = nil
			t.notify(ctx, NotifyWarning, "Order niet geplaatst", err.Error())
			return nil
		}
		return err // de order blijft vastgelegd en wordt de volgende ronde idempotent opnieuw geplaatst
	}
	t.notify(ctx, NotifyInfo, "Kooporder geplaatst", fmt.Sprintf("%s BTC tegen %s (inleg %s, trend %s)",
		size, price, t.cfg.Budget, t.state.LastTrend))
	return nil
}

// ensureSellOrders zorgt dat er in een positie altijd precies één passende verkooporder is.
func (t *Trader) ensureSellOrders(ctx context.Context) error {
	if t.state.Halted || t.state.SellOrder != nil {
		return nil
	}
	s := t.state.Strategy
	switch s.Phase {
	case strategy.PhaseInPosition, strategy.PhasePart1Sold:
		action, ok := t.machine.TakeProfitAction(s, t.state.Fees)
		if !ok {
			return nil
		}
		purpose := PurposeTakeProfit1
		if action.Step == 2 {
			purpose = PurposeTakeProfit2
		}
		return t.placeSell(ctx, purpose, action.Quantity, action.Price)

	case strategy.PhaseTrailing:
		if !s.StopTriggered {
			return nil
		}
		price := t.ticker.BestBid
		if !price.IsPositive() {
			price = t.ticker.Price
		}
		return t.placeSell(ctx, PurposeStop, s.Position.Quantity, t.product.RoundPriceDown(price))
	}
	return nil
}

// placeSell plaatst een verkooporder na controle van het werkelijke saldo (plan 2.4a).
func (t *Trader) placeSell(ctx context.Context, purpose OrderPurpose, size, price decimal.Decimal) error {
	position := t.state.Strategy.Position
	if position == nil || size.GreaterThan(position.Quantity) {
		t.halt(ctx, fmt.Sprintf("Verkoop van %s BTC past niet bij de positie.", size))
		return nil
	}
	balances, err := t.exchange.Balances(ctx)
	if err != nil {
		return err
	}
	base, _ := domain.Currencies(t.product.ID)
	if available := balances[base].Available; available.LessThan(position.Quantity) {
		t.halt(ctx, fmt.Sprintf("Er is %s %s vrij beschikbaar, maar de positie is %s %s.", available, base, position.Quantity, base))
		return nil
	}
	if !t.product.IsTradable(size, price) {
		t.halt(ctx, fmt.Sprintf("Verkoop van %s BTC tegen %s is te klein voor de exchange.", size, price))
		return nil
	}

	a := t.newOrder(purpose, domain.SideSell, size, price)
	t.state.SellOrder = a
	if _, err := t.place(ctx, a); err != nil {
		if errors.Is(err, ErrInsufficientFunds) {
			t.state.SellOrder = nil
			t.halt(ctx, "Verkooporder geweigerd wegens onvoldoende saldo: "+err.Error())
			return nil
		}
		return err
	}
	titles := map[OrderPurpose]string{PurposeTakeProfit1: "Winstorder stap 1 geplaatst",
		PurposeTakeProfit2: "Winstorder stap 2 geplaatst", PurposeStop: "Stopverkoop geplaatst"}
	t.notify(ctx, NotifyInfo, titles[purpose], fmt.Sprintf("%s BTC tegen %s", size, price))
	return nil
}

// newOrder legt een nieuwe order vast met een uniek, deterministisch ClientOrderID.
func (t *Trader) newOrder(purpose OrderPurpose, side domain.Side, size, price decimal.Decimal) *ActiveOrder {
	t.state.OrderSeq++
	return &ActiveOrder{
		ClientOrderID: fmt.Sprintf("tb-%s-%d-%s", t.state.InstanceID, t.state.OrderSeq, strings.ToLower(string(purpose))),
		Purpose:       purpose, Side: side, Size: size, Price: price, PlacedAt: t.now(),
	}
}

// place slaat eerst de toestand op (met het ClientOrderID) en plaatst daarna de order. Valt de bot
// tussendoor weg, dan levert opnieuw plaatsen met hetzelfde ClientOrderID geen tweede order op.
func (t *Trader) place(ctx context.Context, a *ActiveOrder) (domain.Order, error) {
	if err := t.save(ctx); err != nil {
		return domain.Order{}, err
	}
	ord, err := t.exchange.PlaceLimitOrder(ctx, OrderRequest{
		ClientOrderID: a.ClientOrderID, ProductID: t.product.ID, Side: a.Side, Size: a.Size, Price: a.Price,
	})
	if err != nil {
		return domain.Order{}, err
	}
	a.OrderID = ord.ID
	positionID := t.state.PositionID
	if err := t.store.SaveOrder(ctx, OrderRecord{Order: ord, PositionID: positionID, Purpose: a.Purpose}); err != nil {
		return ord, err
	}
	return ord, t.save(ctx)
}

// finishPosition rondt een volledig verkochte positie af en start de cooldown.
func (t *Trader) finishPosition(ctx context.Context) error {
	if t.state.Strategy.Phase != strategy.PhaseCompleted {
		return nil
	}
	if t.state.SellOrder != nil {
		return nil // eerst de laatste order afhandelen
	}

	if t.state.PositionID != 0 {
		result, err := t.positionResult(ctx, t.state.PositionID)
		if err != nil {
			return err
		}
		if err := t.store.ClosePosition(ctx, t.state.PositionID, t.now()); err != nil {
			return err
		}
		t.notify(ctx, NotifyInfo, "Positie afgerond", fmt.Sprintf("positie %d: ingelegd %s, opbrengst %s, resultaat %s (%s%%), fees %s",
			t.state.PositionID, result.Invested.StringFixed(2), result.Proceeds.StringFixed(2),
			result.Profit.StringFixed(2), result.ProfitPct.StringFixed(2), result.Fees.StringFixed(2)))
	}

	trend := indicator.TrendFlat
	if candles, err := t.market.StrategyCandles(ctx, t.market.History()); err == nil {
		if tr, err := t.machine.Trend(domain.Closes(candles)); err == nil {
			trend = tr
		}
	}
	s, err := t.machine.StartCooldown(t.state.Strategy, trend)
	if err != nil {
		return err
	}
	t.state.Strategy = s
	t.state.PositionID = 0
	t.state.TrailingSince = time.Time{}
	t.event(ctx, "COOLDOWN", fmt.Sprintf("cooldown %s gestart (trend bij exit: %s)", s.Cooldown, trend))
	return nil
}

// PositionResult is het resultaat van een positie, netto na alle fees.
type PositionResult struct {
	Bought    decimal.Decimal // gekochte BTC
	Sold      decimal.Decimal // verkochte BTC
	Invested  decimal.Decimal // betaald voor de aankoop, inclusief fee
	Proceeds  decimal.Decimal // ontvangen voor de verkopen, na fee
	Fees      decimal.Decimal
	Realized  decimal.Decimal // opbrengst − het aandeel van de verkochte BTC in de inleg
	Profit    decimal.Decimal // opbrengst − volledige inleg (gelijk aan Realized als alles verkocht is)
	ProfitPct decimal.Decimal
}

func (t *Trader) positionResult(ctx context.Context, positionID int64) (PositionResult, error) {
	orders, err := t.store.PositionOrders(ctx, positionID)
	if err != nil {
		return PositionResult{}, err
	}
	ids := make([]string, len(orders))
	side := map[string]domain.Side{}
	for i, o := range orders {
		ids[i] = o.ID
		side[o.ID] = o.Side
	}
	fills, err := t.store.OrderFills(ctx, ids)
	if err != nil {
		return PositionResult{}, err
	}
	return CalculateResult(fills, side), nil
}

// CalculateResult berekent het resultaat uit fills; side geeft per order-ID de kant.
func CalculateResult(fills []domain.Fill, side map[string]domain.Side) PositionResult {
	var r PositionResult
	for _, f := range fills {
		value := f.Price.Mul(f.Size)
		r.Fees = r.Fees.Add(f.Fee)
		if side[f.OrderID] == domain.SideBuy {
			r.Bought = r.Bought.Add(f.Size)
			r.Invested = r.Invested.Add(value).Add(f.Fee)
		} else {
			r.Sold = r.Sold.Add(f.Size)
			r.Proceeds = r.Proceeds.Add(value).Sub(f.Fee)
		}
	}
	r.Realized = r.Proceeds
	if r.Bought.IsPositive() {
		r.Realized = r.Proceeds.Sub(r.Invested.Mul(r.Sold).Div(r.Bought))
	}
	r.Profit = r.Proceeds.Sub(r.Invested)
	if r.Invested.IsPositive() {
		r.ProfitPct = r.Profit.Div(r.Invested).Mul(decimal.NewFromInt(100))
	}
	return r
}

func toStrategyFills(fills []domain.Fill) []strategy.Fill {
	out := make([]strategy.Fill, len(fills))
	for i, f := range fills {
		out[i] = strategy.Fill{Price: f.Price, Size: f.Size, Fee: f.Fee}
	}
	return out
}

func remaining(s strategy.State) decimal.Decimal {
	if s.Position == nil {
		return decimal.Zero
	}
	return s.Position.Quantity
}

func pct(fraction decimal.Decimal) string {
	return fraction.Mul(decimal.NewFromInt(100)).String()
}
