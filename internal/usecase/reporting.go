package usecase

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/shopspring/decimal"

	"tradingbot/internal/domain"
	"tradingbot/internal/domain/strategy"
)

// ErrInvalidArgument wordt teruggegeven bij een ongeldige vraag (bijv. een onbekende periode).
var ErrInvalidArgument = errors.New("ongeldige invoer")

// TraderStateSource levert de actuele toestand van de trader.
type TraderStateSource interface {
	State() TraderState
}

// ReportingConfig bevat de vaste gegevens voor status en rapportage.
type ReportingConfig struct {
	Mode        string
	ProductID   string
	Budget      decimal.Decimal
	PaperStart  decimal.Decimal // alleen in paper-modus
	StartedAt   time.Time
	RunDuration time.Duration // 0 = onbepaald
	Location    *time.Location
	Cooldowns   map[strategy.CooldownKind]int
	MaxCandles  int // maximaal aantal candles per verzoek
}

// Reporting levert de gegevens voor de API en de meldingen. Hij leest alleen; acties lopen via de trader.
type Reporting struct {
	cfg      ReportingConfig
	store    ReportStore
	trader   TraderStateSource
	machine  *strategy.Machine
	market   *MarketDataService
	exchange Exchange
	clock    Clock
}

// NewReporting maakt de rapportage-service.
func NewReporting(cfg ReportingConfig, store ReportStore, trader TraderStateSource, machine *strategy.Machine,
	market *MarketDataService, exchange Exchange, clock Clock) *Reporting {
	if cfg.Location == nil {
		cfg.Location = time.UTC
	}
	if cfg.MaxCandles <= 0 {
		cfg.MaxCandles = 10000
	}
	return &Reporting{cfg: cfg, store: store, trader: trader, machine: machine, market: market, exchange: exchange, clock: clock}
}

var hundred = decimal.NewFromInt(100)

func percentOf(part, whole decimal.Decimal) decimal.Decimal {
	if !whole.IsPositive() {
		return decimal.Zero
	}
	return part.Div(whole).Mul(hundred).Round(2)
}

// ---- status ----

// StatusView is het antwoord van GET /status.
type StatusView struct {
	Mode             string          `json:"mode"`
	ProductID        string          `json:"product_id"`
	Phase            strategy.Phase  `json:"phase"`
	Trend            string          `json:"trend"`
	LastPrice        decimal.Decimal `json:"last_price"`
	LastBid          decimal.Decimal `json:"last_bid"`
	LastPriceTime    *time.Time      `json:"last_price_time"`
	LastCandle       *time.Time      `json:"last_candle"`
	StartedAt        time.Time       `json:"started_at"`
	UptimeSeconds    int64           `json:"uptime_seconds"`
	RunDuration      string          `json:"run_duration"`      // "onbepaald" of bijv. "72h0m0s"
	RemainingSeconds *int64          `json:"remaining_seconds"` // nil bij onbepaalde looptijd
	Paused           bool            `json:"paused"`
	Halted           bool            `json:"halted"`
	PauseReason      string          `json:"pause_reason,omitempty"`
	Budget           decimal.Decimal `json:"budget"`
	MakerFeePct      decimal.Decimal `json:"maker_fee_pct"`
	TakerFeePct      decimal.Decimal `json:"taker_fee_pct"`
	PositionID       int64           `json:"position_id,omitempty"`
	Cooldown         *CooldownView   `json:"cooldown,omitempty"`
	OpenOrders       []ActiveOrder   `json:"open_orders"`
}

// CooldownView is de voortgang van een lopende cooldown.
type CooldownView struct {
	Kind    strategy.CooldownKind `json:"kind"`
	Elapsed int                   `json:"elapsed_candles"`
	Length  int                   `json:"length_candles"`
}

// Status geeft de actuele status van de bot.
func (r *Reporting) Status(context.Context) StatusView {
	st := r.trader.State()
	now := r.clock.Now().UTC()
	v := StatusView{
		Mode: r.cfg.Mode, ProductID: r.cfg.ProductID, Phase: st.Strategy.Phase, Trend: string(st.LastTrend),
		LastPrice: st.LastPrice, LastBid: st.LastBid, LastPriceTime: timePtr(st.LastPriceTime),
		LastCandle: timePtr(st.LastCandle), StartedAt: r.cfg.StartedAt,
		UptimeSeconds: int64(now.Sub(r.cfg.StartedAt).Seconds()), RunDuration: "onbepaald",
		Paused: st.Paused, Halted: st.Halted, PauseReason: st.PauseReason, Budget: r.cfg.Budget,
		MakerFeePct: st.Fees.Maker.Mul(hundred), TakerFeePct: st.Fees.Taker.Mul(hundred),
		PositionID: st.PositionID, OpenOrders: []ActiveOrder{},
	}
	if r.cfg.RunDuration > 0 {
		v.RunDuration = r.cfg.RunDuration.String()
		remaining := max(0, int64(r.cfg.StartedAt.Add(r.cfg.RunDuration).Sub(now).Seconds()))
		v.RemainingSeconds = &remaining
	}
	if st.Strategy.Phase == strategy.PhaseCooldown {
		v.Cooldown = &CooldownView{Kind: st.Strategy.Cooldown, Elapsed: st.Strategy.CooldownElapsed,
			Length: r.cfg.Cooldowns[st.Strategy.Cooldown]}
	}
	for _, o := range []*ActiveOrder{st.BuyOrder, st.SellOrder} {
		if o != nil {
			v.OpenOrders = append(v.OpenOrders, *o)
		}
	}
	return v
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// ---- posities ----

// SaleView is één verkoopstap van een positie.
type SaleView struct {
	OrderID      string          `json:"order_id"`
	Purpose      OrderPurpose    `json:"purpose"`
	Time         time.Time       `json:"time"`
	Quantity     decimal.Decimal `json:"quantity"`
	AveragePrice decimal.Decimal `json:"average_price"`
	Proceeds     decimal.Decimal `json:"proceeds"` // na fee
	Fee          decimal.Decimal `json:"fee"`
}

// Amount is een bedrag met het bijbehorende percentage.
type Amount struct {
	EUR decimal.Decimal `json:"eur"`
	Pct decimal.Decimal `json:"pct"`
}

// PositionStatusView beschrijft wat er met een open positie gebeurt.
type PositionStatusView struct {
	Phase          strategy.Phase   `json:"phase"`
	OpenOrders     []ActiveOrder    `json:"open_orders"`
	NextTarget     *decimal.Decimal `json:"next_target,omitempty"` // prijs van de volgende winststap
	TrailingHigh   *decimal.Decimal `json:"trailing_high,omitempty"`
	TrailingStop   *decimal.Decimal `json:"trailing_stop,omitempty"`
	StopTriggered  bool             `json:"stop_triggered"`
	ValuationPrice decimal.Decimal  `json:"valuation_price"` // bid waartegen het restant gewaardeerd is
	ValuationAt    *time.Time       `json:"valuation_at"`
}

// PositionView is een positie met alle stappen en het resultaat (plan 4.7).
type PositionView struct {
	ID              int64               `json:"id"`
	ProductID       string              `json:"product_id"`
	Open            bool                `json:"open"`
	OpenedAt        time.Time           `json:"opened_at"`
	ClosedAt        *time.Time          `json:"closed_at"`
	DurationSeconds int64               `json:"duration_seconds"`
	EntryQuantity   decimal.Decimal     `json:"entry_quantity"`
	CostPrice       decimal.Decimal     `json:"cost_price"`
	Invested        decimal.Decimal     `json:"invested"` // inclusief koopfee
	Sales           []SaleView          `json:"sales"`
	Remaining       decimal.Decimal     `json:"remaining_quantity"`
	Realized        Amount              `json:"realized"`
	Unrealized      Amount              `json:"unrealized"`
	Total           Amount              `json:"total"`
	Fees            decimal.Decimal     `json:"fees"`
	Status          *PositionStatusView `json:"status,omitempty"`
}

// OpenPosition geeft de open positie, of nil als er geen is.
func (r *Reporting) OpenPosition(ctx context.Context) (*PositionView, error) {
	st := r.trader.State()
	if st.PositionID == 0 {
		return nil, nil
	}
	v, _, err := r.Position(ctx, st.PositionID)
	return v, err
}

// Position geeft één positie; ok is false als hij niet bestaat.
func (r *Reporting) Position(ctx context.Context, id int64) (*PositionView, bool, error) {
	rec, ok, err := r.store.GetPosition(ctx, id)
	if err != nil || !ok {
		return nil, ok, err
	}
	v, err := r.positionView(ctx, rec, r.trader.State())
	return v, err == nil, err
}

// PositionHistory geeft de afgeronde posities, nieuwste eerst.
func (r *Reporting) PositionHistory(ctx context.Context, limit int) ([]PositionView, error) {
	recs, err := r.store.ListPositions(ctx, PositionFilter{Closed: true, Limit: limit})
	if err != nil {
		return nil, err
	}
	st := r.trader.State()
	views := make([]PositionView, 0, len(recs))
	for _, rec := range recs {
		v, err := r.positionView(ctx, rec, st)
		if err != nil {
			return nil, err
		}
		views = append(views, *v)
	}
	return views, nil
}

func (r *Reporting) positionView(ctx context.Context, rec PositionRecord, st TraderState) (*PositionView, error) {
	orders, err := r.store.PositionOrders(ctx, rec.ID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(orders))
	byID := map[string]OrderRecord{}
	for i, o := range orders {
		ids[i] = o.ID
		byID[o.ID] = o
	}
	fills, err := r.store.OrderFills(ctx, ids)
	if err != nil {
		return nil, err
	}

	v := &PositionView{ID: rec.ID, ProductID: rec.ProductID, Open: rec.ClosedAt.IsZero(), OpenedAt: rec.OpenedAt,
		ClosedAt: timePtr(rec.ClosedAt), EntryQuantity: rec.EntryQuantity, CostPrice: rec.CostPrice, Sales: []SaleView{}}

	bought, sold, proceeds := decimal.Zero, decimal.Zero, decimal.Zero
	sales := map[string]*SaleView{}
	for _, f := range fills {
		value := f.Price.Mul(f.Size)
		v.Fees = v.Fees.Add(f.Fee)
		o := byID[f.OrderID]
		if o.Side == domain.SideBuy {
			bought = bought.Add(f.Size)
			v.Invested = v.Invested.Add(value).Add(f.Fee)
			continue
		}
		sold = sold.Add(f.Size)
		proceeds = proceeds.Add(value).Sub(f.Fee)
		s := sales[f.OrderID]
		if s == nil {
			s = &SaleView{OrderID: f.OrderID, Purpose: o.Purpose}
			sales[f.OrderID] = s
		}
		s.Quantity = s.Quantity.Add(f.Size)
		s.Proceeds = s.Proceeds.Add(value).Sub(f.Fee)
		s.Fee = s.Fee.Add(f.Fee)
		s.AveragePrice = s.AveragePrice.Add(value) // tijdelijk de som, hieronder gedeeld
		if f.Time.After(s.Time) {
			s.Time = f.Time
		}
	}
	for _, s := range sales {
		s.AveragePrice = s.AveragePrice.Div(s.Quantity).Round(2)
		v.Sales = append(v.Sales, *s)
	}
	sort.Slice(v.Sales, func(i, j int) bool { return v.Sales[i].Time.Before(v.Sales[j].Time) })

	v.Remaining = bought.Sub(sold)
	soldCost := decimal.Zero
	if bought.IsPositive() {
		soldCost = v.Invested.Mul(sold).Div(bought)
	}
	v.Realized = Amount{EUR: proceeds.Sub(soldCost).Round(2), Pct: percentOf(proceeds.Sub(soldCost), soldCost)}

	end := rec.ClosedAt
	if v.Open {
		end = r.clock.Now().UTC()
		if v.Remaining.IsPositive() {
			bid := st.LastBid
			if !bid.IsPositive() {
				bid = st.LastPrice
			}
			remainingCost := v.Invested.Sub(soldCost)
			value := v.Remaining.Mul(bid).Mul(decimal.NewFromInt(1).Sub(st.Fees.Maker))
			v.Unrealized = Amount{EUR: value.Sub(remainingCost).Round(2), Pct: percentOf(value.Sub(remainingCost), remainingCost)}
		}
		if rec.ID == st.PositionID {
			v.Status = r.positionStatus(st)
		}
	}
	total := v.Realized.EUR.Add(v.Unrealized.EUR)
	v.Total = Amount{EUR: total, Pct: percentOf(total, v.Invested)}
	v.DurationSeconds = int64(end.Sub(rec.OpenedAt).Seconds())
	return v, nil
}

func (r *Reporting) positionStatus(st TraderState) *PositionStatusView {
	ps := &PositionStatusView{Phase: st.Strategy.Phase, OpenOrders: []ActiveOrder{}, StopTriggered: st.Strategy.StopTriggered,
		ValuationPrice: st.LastBid, ValuationAt: timePtr(st.LastPriceTime)}
	if st.SellOrder != nil {
		ps.OpenOrders = append(ps.OpenOrders, *st.SellOrder)
	}
	switch st.Strategy.Phase {
	case strategy.PhaseInPosition, strategy.PhasePart1Sold:
		if a, ok := r.machine.TakeProfitAction(st.Strategy, st.Fees); ok {
			ps.NextTarget = &a.Price
		}
	case strategy.PhaseTrailing:
		high, stop := st.Strategy.TrailingHigh, r.machine.TrailingStop(st.Strategy).Round(2)
		ps.TrailingHigh, ps.TrailingStop = &high, &stop
	}
	return ps
}

// ---- orders, trades en candles ----

// Orders geeft orders volgens het filter.
func (r *Reporting) Orders(ctx context.Context, f OrderFilter) ([]OrderRecord, error) {
	return r.store.ListOrders(ctx, f)
}

// Trades geeft fills volgens het filter.
func (r *Reporting) Trades(ctx context.Context, f TradeFilter) ([]TradeRecord, error) {
	return r.store.ListTrades(ctx, f)
}

// Candles geeft de koershistorie in [from, to) met het opgegeven interval (veelvoud van 1 minuut).
func (r *Reporting) Candles(ctx context.Context, interval time.Duration, from, to time.Time) ([]domain.Candle, error) {
	if interval < time.Minute || interval%time.Minute != 0 {
		return nil, fmt.Errorf("%w: interval moet een veelvoud van 1m zijn", ErrInvalidArgument)
	}
	if !from.Before(to) {
		return nil, fmt.Errorf("%w: from moet vóór to liggen", ErrInvalidArgument)
	}
	if n := to.Sub(from) / interval; int(n) > r.cfg.MaxCandles {
		return nil, fmt.Errorf("%w: maximaal %d candles per verzoek", ErrInvalidArgument, r.cfg.MaxCandles)
	}
	minutes, err := r.market.MinuteCandles(ctx, from.UTC().Truncate(interval), to)
	if err != nil {
		return nil, err
	}
	if interval == time.Minute {
		return minutes, nil
	}
	end := r.clock.Now().UTC()
	if to.Before(end) {
		end = to
	}
	return domain.Aggregate(minutes, interval, end), nil
}

// ---- totaaloverzicht ----

// Period is een rapportageperiode.
type Period string

const (
	PeriodDay   Period = "day"
	PeriodWeek  Period = "week"
	PeriodMonth Period = "month"
	PeriodAll   Period = "all"
)

// ReportView is het totaaloverzicht (plan 4.7).
type ReportView struct {
	Period             Period          `json:"period"`
	From               *time.Time      `json:"from"`
	ClosedPositions    int             `json:"closed_positions"`
	Winners            int             `json:"winners"`
	Losers             int             `json:"losers"`
	Invested           decimal.Decimal `json:"invested"`
	Realized           Amount          `json:"realized"`
	Unrealized         Amount          `json:"unrealized"` // van de open positie
	Fees               decimal.Decimal `json:"fees"`
	AverageProfit      decimal.Decimal `json:"average_profit"`
	AverageDurationSec int64           `json:"average_duration_seconds"`
	Paper              *PaperView      `json:"paper,omitempty"`
}

// PaperView vergelijkt het paper-saldo met het startkapitaal.
type PaperView struct {
	StartBalance decimal.Decimal `json:"start_balance"`
	QuoteBalance decimal.Decimal `json:"quote_balance"`
	BaseBalance  decimal.Decimal `json:"base_balance"`
	Equity       decimal.Decimal `json:"equity"` // quote + base tegen de bid
	Change       Amount          `json:"change"`
}

// PeriodStart geeft het begin van de periode in de rapportagetijdzone; nul bij PeriodAll.
func PeriodStart(p Period, now time.Time, loc *time.Location) (time.Time, error) {
	local := now.In(loc)
	midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	switch p {
	case PeriodDay:
		return midnight, nil
	case PeriodWeek:
		offset := (int(local.Weekday()) + 6) % 7 // maandag = 0
		return midnight.AddDate(0, 0, -offset), nil
	case PeriodMonth:
		return time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, loc), nil
	case PeriodAll:
		return time.Time{}, nil
	}
	return time.Time{}, fmt.Errorf("%w: onbekende periode %q (day, week, month of all)", ErrInvalidArgument, p)
}

// PeriodSince is de periode van een overzicht vanaf een willekeurig tijdstip (ReportSince).
const PeriodSince Period = "since"

// Report geeft het totaaloverzicht over de afgeronde posities in de periode.
func (r *Reporting) Report(ctx context.Context, period Period) (ReportView, error) {
	from, err := PeriodStart(period, r.clock.Now(), r.cfg.Location)
	if err != nil {
		return ReportView{}, err
	}
	return r.report(ctx, period, from)
}

// ReportSince geeft het totaaloverzicht over de posities die vanaf from zijn afgerond.
func (r *Reporting) ReportSince(ctx context.Context, from time.Time) (ReportView, error) {
	return r.report(ctx, PeriodSince, from)
}

func (r *Reporting) report(ctx context.Context, period Period, from time.Time) (ReportView, error) {
	recs, err := r.store.ListPositions(ctx, PositionFilter{Closed: true, ClosedFrom: from, Limit: -1})
	if err != nil {
		return ReportView{}, err
	}

	st := r.trader.State()
	v := ReportView{Period: period, From: timePtr(from)}
	var duration int64
	for _, rec := range recs {
		p, err := r.positionView(ctx, rec, st)
		if err != nil {
			return ReportView{}, err
		}
		v.ClosedPositions++
		switch {
		case p.Realized.EUR.IsPositive():
			v.Winners++
		case p.Realized.EUR.IsNegative():
			v.Losers++
		}
		v.Invested = v.Invested.Add(p.Invested)
		v.Realized.EUR = v.Realized.EUR.Add(p.Realized.EUR)
		v.Fees = v.Fees.Add(p.Fees)
		duration += p.DurationSeconds
	}
	v.Realized.Pct = percentOf(v.Realized.EUR, v.Invested)
	if v.ClosedPositions > 0 {
		v.AverageProfit = v.Realized.EUR.Div(decimal.NewFromInt(int64(v.ClosedPositions))).Round(2)
		v.AverageDurationSec = duration / int64(v.ClosedPositions)
	}

	open, err := r.OpenPosition(ctx)
	if err != nil {
		return ReportView{}, err
	}
	if open != nil {
		v.Unrealized = open.Unrealized
	}

	if r.cfg.Mode == "paper" && r.exchange != nil {
		paper, err := r.paperView(ctx, st)
		if err != nil {
			return ReportView{}, err
		}
		v.Paper = paper
	}
	return v, nil
}

func (r *Reporting) paperView(ctx context.Context, st TraderState) (*PaperView, error) {
	balances, err := r.exchange.Balances(ctx)
	if err != nil {
		return nil, err
	}
	base, quote := domain.Currencies(r.cfg.ProductID)
	pv := &PaperView{StartBalance: r.cfg.PaperStart, QuoteBalance: balances[quote].Total(), BaseBalance: balances[base].Total()}
	pv.Equity = pv.QuoteBalance.Add(pv.BaseBalance.Mul(st.LastBid)).Round(2)
	change := pv.Equity.Sub(pv.StartBalance)
	pv.Change = Amount{EUR: change, Pct: percentOf(change, pv.StartBalance)}
	return pv, nil
}

// AllPositions geeft alle posities met resultaat, oudste eerst (voor de export).
func (r *Reporting) AllPositions(ctx context.Context) ([]PositionView, error) {
	recs, err := r.store.ListPositions(ctx, PositionFilter{Limit: -1})
	if err != nil {
		return nil, err
	}
	st := r.trader.State()
	views := make([]PositionView, 0, len(recs))
	for i := len(recs) - 1; i >= 0; i-- {
		v, err := r.positionView(ctx, recs[i], st)
		if err != nil {
			return nil, err
		}
		views = append(views, *v)
	}
	return views, nil
}
