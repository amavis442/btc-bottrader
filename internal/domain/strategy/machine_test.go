package strategy

import (
	"errors"
	"testing"

	"github.com/shopspring/decimal"

	"tradingbot/internal/domain/indicator"
)

// Kleine periodes zodat de tests overzichtelijk blijven: SMA2 tegen SMA4.
func testConfig() Config {
	return Config{
		Trend: indicator.TrendConfig{ShortPeriod: 2, LongPeriod: 4, FlatThreshold: d("0.005")},
		TakeProfits: [2]TakeProfit{
			{NetProfit: d("0.06"), SellFraction: d("0.5")},
			{NetProfit: d("0.12"), SellFraction: d("0.65")},
		},
		TrailingDistance: d("0.05"),
		CooldownReversal: 3,
		CooldownRising:   1,
	}
}

func newTestMachine(t *testing.T) *Machine {
	t.Helper()
	m, err := NewMachine(testConfig(), btcEur())
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func closes(values ...float64) []decimal.Decimal {
	out := make([]decimal.Decimal, len(values))
	for i, v := range values {
		out[i] = decimal.NewFromFloat(v)
	}
	return out
}

var (
	rising  = closes(99, 99, 101, 101)
	falling = closes(101, 101, 99, 99)
	flat    = closes(100, 100, 100, 100)
)

func assertPhase(t *testing.T, s State, want Phase) {
	t.Helper()
	if s.Phase != want {
		t.Fatalf("toestand = %s, verwacht %s", s.Phase, want)
	}
}

func assertActions(t *testing.T, actions []Action, want ...ActionKind) {
	t.Helper()
	if len(actions) != len(want) {
		t.Fatalf("acties = %+v, verwacht %v", actions, want)
	}
	for i, a := range actions {
		if a.Kind != want[i] {
			t.Fatalf("actie %d = %s, verwacht %s", i, a.Kind, want[i])
		}
	}
}

// inPosition brengt de machine via de normale weg in IN_POSITIE met 0,001 BTC tegen kostprijs 60.000.
func inPosition(t *testing.T, m *Machine) (State, Action) {
	t.Helper()
	s, _, err := m.OnCandleClosed(NewState(), rising)
	if err != nil {
		t.Fatal(err)
	}
	s, actions, err := m.OnBuyFilled(s, d("0.001"), d("60000"), testFees)
	if err != nil {
		t.Fatal(err)
	}
	assertActions(t, actions, ActionPlaceTakeProfit)
	return s, actions[0]
}

func TestFullCycle(t *testing.T) {
	m := newTestMachine(t)

	// WACHT_OP_DATA: te weinig candles
	s, actions, err := m.OnCandleClosed(NewState(), closes(99, 99, 101))
	if err != nil {
		t.Fatal(err)
	}
	assertPhase(t, s, PhaseWaitingForData)
	assertActions(t, actions)

	// genoeg data en stijgend → ZOEK_INSTAP + kopen
	s, actions, err = m.OnCandleClosed(s, rising)
	if err != nil {
		t.Fatal(err)
	}
	assertPhase(t, s, PhaseSeekingEntry)
	assertActions(t, actions, ActionBuy)

	// koop gevuld → IN_POSITIE + winststap 1
	s, actions, err = m.OnBuyFilled(s, d("0.001"), d("60000"), testFees)
	if err != nil {
		t.Fatal(err)
	}
	assertPhase(t, s, PhaseInPosition)
	assertActions(t, actions, ActionPlaceTakeProfit)
	tp1 := actions[0]
	if tp1.Step != 1 {
		t.Errorf("stap = %d, verwacht 1", tp1.Step)
	}
	assertDecimal(t, "prijs stap 1", tp1.Price, "63759.40")
	assertDecimal(t, "hoeveelheid stap 1", tp1.Quantity, "0.0005")

	// winststap 1 gevuld → DEEL1_VERKOCHT + winststap 2 (65% van het restant)
	s, actions, err = m.OnTakeProfitFilled(s, 1, tp1.Quantity, tp1.Price, testFees)
	if err != nil {
		t.Fatal(err)
	}
	assertPhase(t, s, PhasePart1Sold)
	assertActions(t, actions, ActionPlaceTakeProfit)
	tp2 := actions[0]
	assertDecimal(t, "prijs stap 2", tp2.Price, "67368.43")
	assertDecimal(t, "hoeveelheid stap 2", tp2.Quantity, "0.000325")

	// winststap 2 gevuld → TRAILING met 17,5% van de positie
	s, actions, err = m.OnTakeProfitFilled(s, 2, tp2.Quantity, tp2.Price, testFees)
	if err != nil {
		t.Fatal(err)
	}
	assertPhase(t, s, PhaseTrailing)
	assertActions(t, actions)
	assertDecimal(t, "restant", s.Position.Quantity, "0.000175")
	assertDecimal(t, "hoogste koers", s.TrailingHigh, "67368.43")

	// koers stijgt: stop schuift mee naar 70.000 × 0,95 = 66.500
	s, actions = m.OnPrice(s, d("70000"))
	assertActions(t, actions)
	assertDecimal(t, "stop", m.TrailingStop(s), "66500")

	// koers daalt maar blijft boven de stop: niets doen, stop gaat niet omlaag
	s, actions = m.OnPrice(s, d("66600"))
	assertActions(t, actions)
	assertDecimal(t, "stop", m.TrailingStop(s), "66500")

	// stop geraakt → restant verkopen, maar maar één keer
	s, actions = m.OnPrice(s, d("66500"))
	assertActions(t, actions, ActionSellRemaining)
	assertDecimal(t, "te verkopen", actions[0].Quantity, "0.000175")
	s, actions = m.OnPrice(s, d("66000"))
	assertActions(t, actions)

	// restant in twee delen verkocht → AFGEROND
	s, err = m.OnRemainingSold(s, d("0.0001"))
	if err != nil {
		t.Fatal(err)
	}
	assertPhase(t, s, PhaseTrailing)
	s, err = m.OnRemainingSold(s, d("0.000075"))
	if err != nil {
		t.Fatal(err)
	}
	assertPhase(t, s, PhaseCompleted)
	assertDecimal(t, "vorige instap", s.PreviousEntry, "60000")

	// koers gekeerd → lange cooldown
	s, err = m.StartCooldown(s, indicator.TrendFalling)
	if err != nil {
		t.Fatal(err)
	}
	assertPhase(t, s, PhaseCooldown)
	if s.Cooldown != CooldownReversal || s.Position != nil {
		t.Errorf("cooldown = %s, positie = %v", s.Cooldown, s.Position)
	}
}

func TestSeekingEntryOnlyBuysOnRisingTrend(t *testing.T) {
	m := newTestMachine(t)
	s := State{Phase: PhaseSeekingEntry}

	for name, c := range map[string][]decimal.Decimal{"vlak": flat, "dalend": falling} {
		got, actions, err := m.OnCandleClosed(s, c)
		if err != nil {
			t.Fatal(err)
		}
		assertPhase(t, got, PhaseSeekingEntry)
		if len(actions) != 0 {
			t.Errorf("%s: verwacht geen actie, kreeg %+v", name, actions)
		}
	}
}

func TestPriceDropInPositionDoesNothing(t *testing.T) {
	m := newTestMachine(t)
	s, _ := inPosition(t, m)

	for _, phase := range []Phase{PhaseInPosition, PhasePart1Sold} {
		s.Phase = phase
		got, actions := m.OnPrice(s, d("30000"))
		assertActions(t, actions)
		got, actions, err := m.OnCandleClosed(got, falling)
		if err != nil {
			t.Fatal(err)
		}
		assertActions(t, actions)
		assertPhase(t, got, phase)
		assertDecimal(t, "positie", got.Position.Quantity, "0.001")
	}
}

func TestCooldownRisingIsShort(t *testing.T) {
	m := newTestMachine(t)
	s := State{Phase: PhaseCompleted}
	s, err := m.StartCooldown(s, indicator.TrendRising)
	if err != nil {
		t.Fatal(err)
	}
	if s.Cooldown != CooldownRising {
		t.Fatalf("cooldown = %s, verwacht kort", s.Cooldown)
	}

	// na 1 candle (korte cooldown) en nog steeds stijgend → direct kopen
	s, actions, err := m.OnCandleClosed(s, rising)
	if err != nil {
		t.Fatal(err)
	}
	assertPhase(t, s, PhaseSeekingEntry)
	assertActions(t, actions, ActionBuy)
}

func TestCooldownReversal(t *testing.T) {
	for name, trend := range map[string]indicator.Trend{"dalend": indicator.TrendFalling, "vlak": indicator.TrendFlat} {
		t.Run(name, func(t *testing.T) {
			m := newTestMachine(t)
			s, err := m.StartCooldown(State{Phase: PhaseCompleted}, trend)
			if err != nil {
				t.Fatal(err)
			}
			if s.Cooldown != CooldownReversal {
				t.Fatalf("cooldown = %s, verwacht lang", s.Cooldown)
			}

			// eerste 2 candles: nog in cooldown, ook al is de trend stijgend
			for i := 0; i < 2; i++ {
				var actions []Action
				s, actions, err = m.OnCandleClosed(s, rising)
				if err != nil {
					t.Fatal(err)
				}
				assertPhase(t, s, PhaseCooldown)
				assertActions(t, actions)
			}

			// derde candle: cooldown voorbij → zoeken en direct kopen bij stijgende trend
			s, actions, err := m.OnCandleClosed(s, rising)
			if err != nil {
				t.Fatal(err)
			}
			assertPhase(t, s, PhaseSeekingEntry)
			assertActions(t, actions, ActionBuy)
		})
	}
}

func TestShortCooldownBecomesLongWhenTrendFalls(t *testing.T) {
	cfg := testConfig()
	cfg.CooldownRising = 2
	m, err := NewMachine(cfg, btcEur())
	if err != nil {
		t.Fatal(err)
	}
	s, _ := m.StartCooldown(State{Phase: PhaseCompleted}, indicator.TrendRising)

	s, _, _ = m.OnCandleClosed(s, falling)
	if s.Cooldown != CooldownReversal {
		t.Fatalf("cooldown = %s, verwacht lang na dalende trend", s.Cooldown)
	}
	// de lange cooldown telt vanaf de exit: na candle 2 nog niet klaar, na candle 3 wel
	s, _, _ = m.OnCandleClosed(s, rising)
	assertPhase(t, s, PhaseCooldown)
	s, actions, _ := m.OnCandleClosed(s, rising)
	assertPhase(t, s, PhaseSeekingEntry)
	assertActions(t, actions, ActionBuy)
}

func TestShortCooldownEndsWithoutBuyOnFlatTrend(t *testing.T) {
	m := newTestMachine(t)
	s, _ := m.StartCooldown(State{Phase: PhaseCompleted}, indicator.TrendRising)
	s, actions, _ := m.OnCandleClosed(s, flat)
	assertPhase(t, s, PhaseSeekingEntry)
	assertActions(t, actions)
}

func TestTakeProfitDustSellsEverythingAndCompletes(t *testing.T) {
	m := newTestMachine(t)
	s, _, _ := m.OnCandleClosed(NewState(), rising)
	// 0,00002 BTC: 50% is te klein om te verhandelen → winststap 1 verkoopt alles
	s, actions, err := m.OnBuyFilled(s, d("0.00002"), d("60000"), testFees)
	if err != nil {
		t.Fatal(err)
	}
	assertDecimal(t, "hoeveelheid", actions[0].Quantity, "0.00002")

	s, actions, err = m.OnTakeProfitFilled(s, 1, actions[0].Quantity, actions[0].Price, testFees)
	if err != nil {
		t.Fatal(err)
	}
	assertPhase(t, s, PhaseCompleted)
	assertActions(t, actions)
}

func TestTakeProfitActionAfterFeeChange(t *testing.T) {
	m := newTestMachine(t)
	s, original := inPosition(t, m)

	higher := Fees{Maker: d("0.004"), Taker: d("0.006")}
	action, ok := m.TakeProfitAction(s, higher)
	if !ok {
		t.Fatal("verwacht winstorder in IN_POSITIE")
	}
	if !action.Price.GreaterThan(original.Price) {
		t.Errorf("hogere fee moet hogere doelprijs geven: %s <= %s", action.Price, original.Price)
	}
	// 60.000 × 1,06 / 0,996 = 63.855,42…
	assertDecimal(t, "nieuwe prijs", action.Price, "63855.43")

	for _, phase := range []Phase{PhaseSeekingEntry, PhaseTrailing, PhaseCooldown} {
		s.Phase = phase
		if _, ok := m.TakeProfitAction(s, higher); ok {
			t.Errorf("geen winstorder verwacht in %s", phase)
		}
	}
}

func TestInvalidTransitions(t *testing.T) {
	m := newTestMachine(t)
	s, tp1 := inPosition(t, m)

	tests := []struct {
		name string
		run  func() error
	}{
		{"tweede koop in IN_POSITIE", func() error {
			_, _, err := m.OnBuyFilled(s, d("0.001"), d("60000"), testFees)
			return err
		}},
		{"winststap 2 in IN_POSITIE", func() error {
			_, _, err := m.OnTakeProfitFilled(s, 2, d("0.0001"), tp1.Price, testFees)
			return err
		}},
		{"onbekende winststap", func() error {
			_, _, err := m.OnTakeProfitFilled(s, 3, d("0.0001"), tp1.Price, testFees)
			return err
		}},
		{"restant verkocht zonder geraakte stop", func() error {
			_, err := m.OnRemainingSold(s, d("0.0001"))
			return err
		}},
		{"cooldown zonder afgeronde positie", func() error {
			_, err := m.StartCooldown(s, indicator.TrendRising)
			return err
		}},
	}
	for _, tt := range tests {
		if err := tt.run(); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("%s: verwacht ErrInvalidTransition, kreeg %v", tt.name, err)
		}
	}
}

func TestCannotSellMoreThanPosition(t *testing.T) {
	m := newTestMachine(t)
	s, tp1 := inPosition(t, m)
	if _, _, err := m.OnTakeProfitFilled(s, 1, d("0.002"), tp1.Price, testFees); err == nil {
		t.Error("verwacht fout bij verkoop van meer dan de positie")
	}
}

func TestInvalidBuyFill(t *testing.T) {
	m := newTestMachine(t)
	s := State{Phase: PhaseSeekingEntry}
	if _, _, err := m.OnBuyFilled(s, d("0"), d("60000"), testFees); err == nil {
		t.Error("verwacht fout bij lege vulling")
	}
}

func TestOnPriceOutsideTrailingDoesNothing(t *testing.T) {
	m := newTestMachine(t)
	s := State{Phase: PhaseSeekingEntry}
	got, actions := m.OnPrice(s, d("1"))
	assertActions(t, actions)
	assertPhase(t, got, PhaseSeekingEntry)
}

func TestSeekingFallsBackToWaitingWithoutData(t *testing.T) {
	m := newTestMachine(t)
	s, actions, err := m.OnCandleClosed(State{Phase: PhaseSeekingEntry}, closes(100))
	if err != nil {
		t.Fatal(err)
	}
	assertPhase(t, s, PhaseWaitingForData)
	assertActions(t, actions)
}

func TestConfigValidate(t *testing.T) {
	if err := testConfig().Validate(); err != nil {
		t.Fatalf("geldige config afgekeurd: %v", err)
	}
	mutations := map[string]func(*Config){
		"trend":              func(c *Config) { c.Trend.ShortPeriod = 0 },
		"winst nul":          func(c *Config) { c.TakeProfits[0].NetProfit = d("0") },
		"verkoop boven 100%": func(c *Config) { c.TakeProfits[0].SellFraction = d("1.1") },
		"stap 2 niet hoger":  func(c *Config) { c.TakeProfits[1].NetProfit = d("0.06") },
		"trailing nul":       func(c *Config) { c.TrailingDistance = d("0") },
		"trailing 100%":      func(c *Config) { c.TrailingDistance = d("1") },
		"negatieve cooldown": func(c *Config) { c.CooldownRising = -1 },
	}
	for name, mutate := range mutations {
		cfg := testConfig()
		mutate(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: ongeldige config goedgekeurd", name)
		}
	}
	if _, err := NewMachine(testConfig(), btcEur()); err != nil {
		t.Errorf("NewMachine: %v", err)
	}
}

func TestOnPartialTakeProfit(t *testing.T) {
	m := newTestMachine(t)
	s, _ := inPosition(t, m)

	s, err := m.OnPartialTakeProfit(s, d("0.0002"))
	if err != nil {
		t.Fatal(err)
	}
	assertPhase(t, s, PhaseInPosition)
	assertDecimal(t, "restant", s.Position.Quantity, "0.0008")

	// nieuwe winstorder: 50% van het restant
	action, _ := m.TakeProfitAction(s, testFees)
	assertDecimal(t, "nieuwe hoeveelheid", action.Quantity, "0.0004")

	s, err = m.OnPartialTakeProfit(s, d("0.0008"))
	if err != nil {
		t.Fatal(err)
	}
	assertPhase(t, s, PhaseCompleted)

	if _, err := m.OnPartialTakeProfit(State{Phase: PhaseTrailing}, d("0.1")); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("verwacht ErrInvalidTransition in TRAILING, kreeg %v", err)
	}
}
