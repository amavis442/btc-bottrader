package usecase_test

import (
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"tradingbot/internal/domain"
	"tradingbot/internal/domain/strategy"
	"tradingbot/internal/usecase"
)

func (s *sim) reporting(runDuration time.Duration) *usecase.Reporting {
	return usecase.NewReporting(usecase.ReportingConfig{
		Mode: "paper", ProductID: product.ID, Budget: d(s.budget), PaperStart: d(s.start),
		StartedAt: t0, RunDuration: runDuration, Location: time.UTC,
		Cooldowns: map[strategy.CooldownKind]int{strategy.CooldownReversal: 3, strategy.CooldownRising: 1},
	}, s.db, s.trader, s.machine, s.market, s.paper, s.clock)
}

// fullCycle laat een volledige, winstgevende positie doorlopen en opnieuw instappen.
func fullCycle(t *testing.T) *sim {
	t.Helper()
	p := newPath(t0, "60000").flat(60).move("0.1", 200).move("-0.2", 120).flat(30).move("0.1", 100)
	s := newSim(t, p)
	s.boot()
	s.runUntil(700, "eerste positie afgerond", phaseIs(strategy.PhaseCooldown))
	s.runUntil(200, "tweede aankoop", phaseIs(strategy.PhaseInPosition))
	return s
}

func TestReportingClosedPosition(t *testing.T) {
	s := fullCycle(t)
	r := s.reporting(0)

	history, err := r.PositionHistory(s.ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 {
		t.Fatalf("afgeronde posities = %d, verwacht 1", len(history))
	}
	p := history[0]
	if p.Open || p.ClosedAt == nil || !p.Remaining.IsZero() || p.Status != nil {
		t.Errorf("afgeronde positie onjuist: %+v", p)
	}
	if len(p.Sales) != 3 || p.Sales[0].Purpose != usecase.PurposeTakeProfit1 ||
		p.Sales[1].Purpose != usecase.PurposeTakeProfit2 || p.Sales[2].Purpose != usecase.PurposeStop {
		t.Fatalf("verkoopstappen = %+v", p.Sales)
	}
	sold := p.Sales[0].Quantity.Add(p.Sales[1].Quantity).Add(p.Sales[2].Quantity)
	if !sold.Equal(p.EntryQuantity) {
		t.Errorf("verkocht %s, gekocht %s", sold, p.EntryQuantity)
	}

	// netto winst: opbrengsten na fee − inleg incl. fee; de eerste stap haalt minstens +6% netto
	proceeds := p.Sales[0].Proceeds.Add(p.Sales[1].Proceeds).Add(p.Sales[2].Proceeds)
	if want := proceeds.Sub(p.Invested).Round(2); !p.Realized.EUR.Equal(want) {
		t.Errorf("gerealiseerd %s, verwacht %s", p.Realized.EUR, want)
	}
	if !p.Realized.EUR.IsPositive() || !p.Total.EUR.Equal(p.Realized.EUR) || !p.Unrealized.EUR.IsZero() {
		t.Errorf("resultaat onjuist: %+v / %+v / %+v", p.Realized, p.Unrealized, p.Total)
	}
	step1Cost := p.Invested.Mul(p.Sales[0].Quantity).Div(p.EntryQuantity)
	if p.Sales[0].Proceeds.LessThan(step1Cost.Mul(d("1.06"))) {
		t.Errorf("stap 1 haalt geen 6%% netto: %s op %s", p.Sales[0].Proceeds, step1Cost)
	}
	if p.Fees.LessThanOrEqual(decimal.Zero) || p.DurationSeconds <= 0 {
		t.Errorf("fees %s, looptijd %d", p.Fees, p.DurationSeconds)
	}

	got, ok, err := r.Position(s.ctx, p.ID)
	if err != nil || !ok || got.ID != p.ID {
		t.Errorf("Position(%d) = %v, %v, %v", p.ID, got, ok, err)
	}
	if _, ok, err := r.Position(s.ctx, 999); ok || err != nil {
		t.Errorf("onbekende positie: ok = %v, fout = %v", ok, err)
	}
}

func TestReportingOpenPosition(t *testing.T) {
	s := fullCycle(t)
	r := s.reporting(0)

	p, err := r.OpenPosition(s.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p == nil || !p.Open || p.Status == nil || p.Status.Phase != strategy.PhaseInPosition {
		t.Fatalf("open positie onjuist: %+v", p)
	}
	if p.Status.NextTarget == nil || len(p.Status.OpenOrders) != 1 {
		t.Errorf("verwacht volgend doel en één open order: %+v", p.Status)
	}
	if !p.Remaining.Equal(p.EntryQuantity) || len(p.Sales) != 0 || !p.Realized.EUR.IsZero() {
		t.Errorf("nog niets verkocht verwacht: %+v", p)
	}

	// ongerealiseerd = restant × bid × (1 − maker) − inleg
	st := s.trader.State()
	value := p.Remaining.Mul(st.LastBid).Mul(d("0.9975"))
	invested := p.CostPrice.Mul(p.EntryQuantity)
	if want := value.Sub(invested).Round(2); !p.Unrealized.EUR.Equal(want) {
		t.Errorf("ongerealiseerd %s, verwacht %s", p.Unrealized.EUR, want)
	}
	if !p.Total.EUR.Equal(p.Unrealized.EUR) {
		t.Errorf("totaal %s ≠ ongerealiseerd %s", p.Total.EUR, p.Unrealized.EUR)
	}
}

func TestReportingTrailingStatus(t *testing.T) {
	s := newSim(t, risingMarket())
	s.boot()
	s.runUntil(300, "trailing", phaseIs(strategy.PhaseTrailing))

	p, err := s.reporting(0).OpenPosition(s.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p.Status.TrailingHigh == nil || p.Status.TrailingStop == nil || p.Status.NextTarget != nil {
		t.Fatalf("trailing-status onjuist: %+v", p.Status)
	}
	if want := p.Status.TrailingHigh.Mul(d("0.95")).Round(2); !p.Status.TrailingStop.Equal(want) {
		t.Errorf("stop %s, verwacht %s", p.Status.TrailingStop, want)
	}
	if len(p.Sales) != 2 || !p.Realized.EUR.IsPositive() {
		t.Errorf("verwacht twee winststappen met winst: %+v", p.Sales)
	}
}

func TestReport(t *testing.T) {
	s := fullCycle(t)
	r := s.reporting(0)

	all, err := r.Report(s.ctx, usecase.PeriodAll)
	if err != nil {
		t.Fatal(err)
	}
	history, _ := r.PositionHistory(s.ctx, 10)
	if all.ClosedPositions != 1 || all.Winners != 1 || all.Losers != 0 || all.From != nil {
		t.Errorf("telling onjuist: %+v", all)
	}
	if !all.Realized.EUR.Equal(history[0].Realized.EUR) || !all.AverageProfit.Equal(history[0].Realized.EUR) {
		t.Errorf("gerealiseerd %s, positie %s", all.Realized.EUR, history[0].Realized.EUR)
	}
	open, _ := r.OpenPosition(s.ctx)
	if open == nil || !all.Unrealized.EUR.Equal(open.Unrealized.EUR) || !all.Unrealized.Pct.Equal(open.Unrealized.Pct) {
		t.Errorf("ongerealiseerd %+v hoort gelijk te zijn aan dat van de open positie", all.Unrealized)
	}

	// paper: saldo + BTC tegen de bid t.o.v. het startkapitaal
	if all.Paper == nil || !all.Paper.StartBalance.Equal(d("100")) || !all.Paper.BaseBalance.IsPositive() {
		t.Fatalf("paper-overzicht onjuist: %+v", all.Paper)
	}
	st := s.trader.State()
	want := all.Paper.QuoteBalance.Add(all.Paper.BaseBalance.Mul(st.LastBid)).Round(2)
	if !all.Paper.Equity.Equal(want) {
		t.Errorf("vermogen %s, verwacht %s", all.Paper.Equity, want)
	}

	// de positie is op de eerste dag afgerond; de klok staat nog op dezelfde dag
	day, err := r.Report(s.ctx, usecase.PeriodDay)
	if err != nil {
		t.Fatal(err)
	}
	if day.ClosedPositions != 1 {
		t.Errorf("dag: %d posities", day.ClosedPositions)
	}

	if _, err := r.Report(s.ctx, "jaar"); !errors.Is(err, usecase.ErrInvalidArgument) {
		t.Errorf("verwacht ErrInvalidArgument, kreeg %v", err)
	}
}

func TestReportingStatus(t *testing.T) {
	s := newSim(t, risingMarket())
	s.boot()
	s.runUntil(90, "aankoop", phaseIs(strategy.PhaseInPosition))

	v := s.reporting(48 * time.Hour).Status(s.ctx)
	if v.Mode != "paper" || v.Phase != strategy.PhaseInPosition || v.PositionID == 0 || len(v.OpenOrders) != 1 {
		t.Errorf("status onjuist: %+v", v)
	}
	if v.RemainingSeconds == nil || *v.RemainingSeconds <= 0 || v.RunDuration != "48h0m0s" || v.UptimeSeconds <= 0 {
		t.Errorf("looptijd onjuist: %+v", v)
	}
	if !v.MakerFeePct.Equal(d("0.25")) || v.LastPriceTime == nil || v.Cooldown != nil {
		t.Errorf("fees/prijs onjuist: %+v", v)
	}
	if unbounded := s.reporting(0).Status(s.ctx); unbounded.RemainingSeconds != nil || unbounded.RunDuration != "onbepaald" {
		t.Errorf("onbepaalde looptijd: %+v", unbounded)
	}
}

func TestReportingCandles(t *testing.T) {
	s := newSim(t, risingMarket())
	s.boot()
	s.run(60)
	r := s.reporting(0)
	now := s.clock.Now()

	// de laatst afgesloten minuut wordt pas bij de volgende ronde opgeslagen: 29 van de 30
	minutes, err := r.Candles(s.ctx, time.Minute, now.Add(-30*time.Minute), now)
	if err != nil || len(minutes) != 29 {
		t.Fatalf("1m-candles: %d, %v", len(minutes), err)
	}
	tens, err := r.Candles(s.ctx, 10*time.Minute, now.Add(-30*time.Minute), now)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range tens {
		if c.Start.Add(10 * time.Minute).After(now) {
			t.Errorf("niet-afgesloten candle teruggegeven: %s", c.Start)
		}
	}
	if len(tens) < 2 {
		t.Errorf("10m-candles: %d", len(tens))
	}

	bad := []struct {
		interval time.Duration
		from, to time.Time
	}{
		{90 * time.Second, now.Add(-time.Hour), now},
		{time.Minute, now, now.Add(-time.Hour)},
		{time.Minute, now.Add(-30 * 24 * time.Hour), now},
	}
	for _, b := range bad {
		if _, err := r.Candles(s.ctx, b.interval, b.from, b.to); !errors.Is(err, usecase.ErrInvalidArgument) {
			t.Errorf("Candles(%s, %s, %s): verwacht ErrInvalidArgument, kreeg %v", b.interval, b.from, b.to, err)
		}
	}
}

func TestOrdersAndTrades(t *testing.T) {
	s := fullCycle(t)
	r := s.reporting(0)

	filled, err := r.Orders(s.ctx, usecase.OrderFilter{Status: domain.OrderFilled})
	if err != nil {
		t.Fatal(err)
	}
	if len(filled) != 5 { // koop, winst 1, winst 2, stop, tweede koop
		t.Errorf("gevulde orders = %d, verwacht 5", len(filled))
	}
	trades, err := r.Trades(s.ctx, usecase.TradeFilter{Limit: 2})
	if err != nil || len(trades) != 2 || trades[0].Time.Before(trades[1].Time) {
		t.Errorf("trades (nieuwste eerst, limiet 2): %+v, %v", trades, err)
	}
}

func TestPeriodStart(t *testing.T) {
	ams, _ := time.LoadLocation("Europe/Amsterdam")
	now := time.Date(2026, 10, 15, 0, 30, 0, 0, time.UTC) // donderdag 02:30 in Amsterdam
	tests := []struct {
		period usecase.Period
		want   time.Time
	}{
		{usecase.PeriodDay, time.Date(2026, 10, 15, 0, 0, 0, 0, ams)},
		{usecase.PeriodWeek, time.Date(2026, 10, 12, 0, 0, 0, 0, ams)}, // maandag
		{usecase.PeriodMonth, time.Date(2026, 10, 1, 0, 0, 0, 0, ams)},
		{usecase.PeriodAll, time.Time{}},
	}
	for _, tt := range tests {
		got, err := usecase.PeriodStart(tt.period, now, ams)
		if err != nil || !got.Equal(tt.want) {
			t.Errorf("%s: %s, %v; verwacht %s", tt.period, got, err, tt.want)
		}
	}
	sunday := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	if got, _ := usecase.PeriodStart(usecase.PeriodWeek, sunday, ams); !got.Equal(time.Date(2026, 10, 12, 0, 0, 0, 0, ams)) {
		t.Errorf("zondag hoort bij de week vanaf maandag 12 oktober: %s", got)
	}
}
