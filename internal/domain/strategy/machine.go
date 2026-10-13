package strategy

import (
	"errors"
	"fmt"

	"github.com/shopspring/decimal"

	"tradingbot/internal/domain"
	"tradingbot/internal/domain/indicator"
)

// Phase is de toestand van de state machine. De waarden worden zo opgeslagen in de database.
type Phase string

const (
	PhaseWaitingForData Phase = "WACHT_OP_DATA"
	PhaseSeekingEntry   Phase = "ZOEK_INSTAP"
	PhaseInPosition     Phase = "IN_POSITIE"
	PhasePart1Sold      Phase = "DEEL1_VERKOCHT"
	PhaseTrailing       Phase = "TRAILING"
	PhaseCompleted      Phase = "AFGEROND"
	PhaseCooldown       Phase = "COOLDOWN"
)

// CooldownKind geeft aan welke cooldown loopt.
type CooldownKind string

const (
	CooldownNone     CooldownKind = ""
	CooldownReversal CooldownKind = "LANG" // koers gekeerd
	CooldownRising   CooldownKind = "KORT" // koers stijgt door
)

// ErrInvalidTransition wordt teruggegeven als een gebeurtenis niet past bij de huidige toestand.
var ErrInvalidTransition = errors.New("strategy: gebeurtenis past niet bij de toestand")

// Position is de open positie zoals de strategie die kent.
type Position struct {
	EntryQuantity decimal.Decimal // gekochte BTC
	Quantity      decimal.Decimal // nog aanwezige BTC
	CostPrice     decimal.Decimal // kostprijs per BTC inclusief koopfee
}

// State is de volledige, opslaanbare toestand van de strategie.
type State struct {
	Phase           Phase
	Position        *Position
	PreviousEntry   decimal.Decimal // kostprijs van de laatst afgeronde positie (rapportage)
	TrailingHigh    decimal.Decimal // hoogste koers sinds activatie van de trailing stop
	StopTriggered   bool            // trailing stop is geraakt, restant wordt verkocht
	Cooldown        CooldownKind
	CooldownElapsed int // aantal afgesloten candles sinds de exit
}

// NewState geeft de begintoestand terug.
func NewState() State {
	return State{Phase: PhaseWaitingForData}
}

// TakeProfit is een winststap: bij NetProfit netto winst wordt SellFraction van het restant verkocht.
type TakeProfit struct {
	NetProfit    decimal.Decimal // fractie, bijv. 0.06
	SellFraction decimal.Decimal // fractie, bijv. 0.5
}

// Config bevat de parameters van de strategie.
type Config struct {
	Trend            indicator.TrendConfig
	TakeProfits      [2]TakeProfit
	TrailingDistance decimal.Decimal // fractie, bijv. 0.05
	CooldownReversal int             // candles na exit als de koers gekeerd is
	CooldownRising   int             // candles na exit als de koers doorstijgt
}

// Validate controleert de strategieparameters.
func (c Config) Validate() error {
	if err := c.Trend.Validate(); err != nil {
		return err
	}
	for i, tp := range c.TakeProfits {
		if !tp.NetProfit.IsPositive() {
			return fmt.Errorf("strategy: winststap %d moet een positieve winst hebben", i+1)
		}
		if !tp.SellFraction.IsPositive() || tp.SellFraction.GreaterThan(one) {
			return fmt.Errorf("strategy: winststap %d moet tussen 0 en 100%% verkopen", i+1)
		}
	}
	if !c.TakeProfits[1].NetProfit.GreaterThan(c.TakeProfits[0].NetProfit) {
		return errors.New("strategy: tweede winststap moet hoger liggen dan de eerste")
	}
	if !c.TrailingDistance.IsPositive() || !c.TrailingDistance.LessThan(one) {
		return errors.New("strategy: afstand trailing stop moet tussen 0 en 100% liggen")
	}
	if c.CooldownReversal < 0 || c.CooldownRising < 0 {
		return errors.New("strategy: cooldown mag niet negatief zijn")
	}
	return nil
}

// ActionKind is het soort actie dat de strategie wil laten uitvoeren.
type ActionKind string

const (
	// ActionBuy: plaats een koop-limit met de vaste inleg. Prijs en grootte bepaalt de trader.
	ActionBuy ActionKind = "KOOP"
	// ActionPlaceTakeProfit: plaats (of vervang) de rustende sell-limit voor een winststap.
	ActionPlaceTakeProfit ActionKind = "WINSTNEMING"
	// ActionSellRemaining: de trailing stop is geraakt, verkoop het restant op de bid.
	ActionSellRemaining ActionKind = "VERKOOP_RESTANT"
)

// Action is een gewenste actie. De trader voert hem uit na de controles uit het plan (2.4a).
type Action struct {
	Kind     ActionKind
	Step     int             // winststap (1 of 2) bij ActionPlaceTakeProfit
	Quantity decimal.Decimal // te verkopen BTC
	Price    decimal.Decimal // limietprijs bij ActionPlaceTakeProfit
}

// Machine voert de state machine uit. Hij is stateless: de toestand gaat erin en komt er gewijzigd uit.
type Machine struct {
	cfg     Config
	product domain.Product
}

// NewMachine maakt een state machine na validatie van de parameters.
func NewMachine(cfg Config, product domain.Product) (*Machine, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := product.Validate(); err != nil {
		return nil, err
	}
	return &Machine{cfg: cfg, product: product}, nil
}

// Trend bepaalt de trend over de slotkoersen.
func (m *Machine) Trend(closes []decimal.Decimal) (indicator.Trend, error) {
	return indicator.DetermineTrend(closes, m.cfg.Trend)
}

// OnCandleClosed verwerkt een afgesloten strategie-candle. closes zijn de slotkoersen, oudste eerst,
// inclusief de zojuist afgesloten candle.
func (m *Machine) OnCandleClosed(s State, closes []decimal.Decimal) (State, []Action, error) {
	switch s.Phase {
	case PhaseWaitingForData:
		if len(closes) < m.cfg.Trend.LongPeriod {
			return s, nil, nil
		}
		s.Phase = PhaseSeekingEntry
		return m.seekEntry(s, closes)

	case PhaseSeekingEntry:
		return m.seekEntry(s, closes)

	case PhaseCooldown:
		s.CooldownElapsed++
		trend, err := m.Trend(closes)
		if err != nil {
			return s, nil, err
		}
		if s.Cooldown == CooldownRising && trend == indicator.TrendFalling {
			s.Cooldown = CooldownReversal
		}
		if s.CooldownElapsed < m.cooldownLength(s.Cooldown) {
			return s, nil, nil
		}
		s.Phase = PhaseSeekingEntry
		s.Cooldown = CooldownNone
		s.CooldownElapsed = 0
		return m.seekEntry(s, closes)

	default:
		// In een positie bepalen de rustende orders en de trailing stop wat er gebeurt, niet de candles.
		return s, nil, nil
	}
}

func (m *Machine) seekEntry(s State, closes []decimal.Decimal) (State, []Action, error) {
	trend, err := m.Trend(closes)
	if errors.Is(err, indicator.ErrNotEnoughData) {
		s.Phase = PhaseWaitingForData
		return s, nil, nil
	}
	if err != nil {
		return s, nil, err
	}
	if trend == indicator.TrendRising {
		return s, []Action{{Kind: ActionBuy}}, nil
	}
	return s, nil, nil
}

func (m *Machine) cooldownLength(kind CooldownKind) int {
	if kind == CooldownRising {
		return m.cfg.CooldownRising
	}
	return m.cfg.CooldownReversal
}

// OnBuyFilled verwerkt een (deels) gevulde kooporder. De positie gaat naar IN_POSITIE en de eerste
// winststap wordt als rustende order aangevraagd.
func (m *Machine) OnBuyFilled(s State, quantity, costPrice decimal.Decimal, fees Fees) (State, []Action, error) {
	if s.Phase != PhaseSeekingEntry {
		return s, nil, fmt.Errorf("%w: koop gevuld in %s", ErrInvalidTransition, s.Phase)
	}
	if !quantity.IsPositive() || !costPrice.IsPositive() {
		return s, nil, errors.New("strategy: ongeldige vulling")
	}
	s.Phase = PhaseInPosition
	s.Position = &Position{EntryQuantity: quantity, Quantity: quantity, CostPrice: costPrice}
	action, _ := m.TakeProfitAction(s, fees)
	return s, []Action{action}, nil
}

// TakeProfitAction geeft de rustende winstorder die bij de huidige toestand hoort. Bruikbaar na een
// fee-wijziging of bij herstel na een herstart. ok is false als er in deze toestand geen winstorder hoort.
func (m *Machine) TakeProfitAction(s State, fees Fees) (Action, bool) {
	var step int
	switch s.Phase {
	case PhaseInPosition:
		step = 1
	case PhasePart1Sold:
		step = 2
	default:
		return Action{}, false
	}
	tp := m.cfg.TakeProfits[step-1]
	price := TargetPrice(s.Position.CostPrice, tp.NetProfit, fees.Maker, m.product)
	qty := SellQuantity(s.Position.Quantity, tp.SellFraction, price, m.product)
	return Action{Kind: ActionPlaceTakeProfit, Step: step, Quantity: qty, Price: price}, true
}

// OnTakeProfitFilled verwerkt de volledig gevulde order van een winststap. fillPrice is de gemiddelde
// verkoopprijs. Wordt daarmee alles verkocht, dan gaat de positie direct naar AFGEROND.
func (m *Machine) OnTakeProfitFilled(s State, step int, sold, fillPrice decimal.Decimal, fees Fees) (State, []Action, error) {
	expected := map[int]Phase{1: PhaseInPosition, 2: PhasePart1Sold}[step]
	if expected == "" || s.Phase != expected {
		return s, nil, fmt.Errorf("%w: winststap %d gevuld in %s", ErrInvalidTransition, step, s.Phase)
	}
	if err := m.reduce(&s, sold); err != nil {
		return s, nil, err
	}
	if s.Position.Quantity.IsZero() {
		return m.complete(s), nil, nil
	}

	if step == 1 {
		s.Phase = PhasePart1Sold
		action, _ := m.TakeProfitAction(s, fees)
		return s, []Action{action}, nil
	}
	s.Phase = PhaseTrailing
	s.TrailingHigh = fillPrice
	s.StopTriggered = false
	return s, nil, nil
}

// TrailingStop geeft de huidige stopprijs in TRAILING.
func (m *Machine) TrailingStop(s State) decimal.Decimal {
	return s.TrailingHigh.Mul(one.Sub(m.cfg.TrailingDistance))
}

// OnPrice verwerkt een actuele koers (elke minuut). In TRAILING schuift de stop mee omhoog; raakt de
// koers de stop, dan wordt één keer gevraagd het restant te verkopen.
func (m *Machine) OnPrice(s State, price decimal.Decimal) (State, []Action) {
	if s.Phase != PhaseTrailing || s.StopTriggered {
		return s, nil
	}
	if price.GreaterThan(s.TrailingHigh) {
		s.TrailingHigh = price
		return s, nil
	}
	if price.LessThanOrEqual(m.TrailingStop(s)) {
		s.StopTriggered = true
		return s, []Action{{Kind: ActionSellRemaining, Quantity: s.Position.Quantity}}
	}
	return s, nil
}

// OnRemainingSold verwerkt een (deel)verkoop van het restant na het raken van de trailing stop.
// Is alles verkocht, dan gaat de positie naar AFGEROND.
func (m *Machine) OnRemainingSold(s State, sold decimal.Decimal) (State, error) {
	if s.Phase != PhaseTrailing || !s.StopTriggered {
		return s, fmt.Errorf("%w: restant verkocht in %s", ErrInvalidTransition, s.Phase)
	}
	if err := m.reduce(&s, sold); err != nil {
		return s, err
	}
	if s.Position.Quantity.IsZero() {
		return m.complete(s), nil
	}
	return s, nil
}

// OnPartialTakeProfit verwerkt een winstorder die gedeeltelijk gevuld is en daarna geannuleerd werd
// (bijv. om hem met een nieuwe fee te vervangen). De positie wordt kleiner; de toestand blijft gelijk,
// zodat er een nieuwe winstorder voor het restant geplaatst kan worden.
func (m *Machine) OnPartialTakeProfit(s State, sold decimal.Decimal) (State, error) {
	if s.Phase != PhaseInPosition && s.Phase != PhasePart1Sold {
		return s, fmt.Errorf("%w: gedeeltelijke winstneming in %s", ErrInvalidTransition, s.Phase)
	}
	if err := m.reduce(&s, sold); err != nil {
		return s, err
	}
	if s.Position.Quantity.IsZero() {
		return m.complete(s), nil
	}
	return s, nil
}

func (m *Machine) reduce(s *State, sold decimal.Decimal) error {
	if !sold.IsPositive() || sold.GreaterThan(s.Position.Quantity) {
		return fmt.Errorf("strategy: ongeldige verkochte hoeveelheid %s (positie %s)", sold, s.Position.Quantity)
	}
	s.Position.Quantity = s.Position.Quantity.Sub(sold)
	return nil
}

func (m *Machine) complete(s State) State {
	s.Phase = PhaseCompleted
	s.PreviousEntry = s.Position.CostPrice
	s.StopTriggered = false
	return s
}

// StartCooldown sluit een AFGEROND-positie af en start de cooldown. De lengte hangt af van de trend
// bij de exit: doorstijgend geeft een korte cooldown, gekeerd (vlak of dalend) een lange.
func (m *Machine) StartCooldown(s State, trendAtExit indicator.Trend) (State, error) {
	if s.Phase != PhaseCompleted {
		return s, fmt.Errorf("%w: cooldown starten in %s", ErrInvalidTransition, s.Phase)
	}
	s.Phase = PhaseCooldown
	s.Position = nil
	s.TrailingHigh = decimal.Zero
	s.CooldownElapsed = 0
	s.Cooldown = CooldownReversal
	if trendAtExit == indicator.TrendRising {
		s.Cooldown = CooldownRising
	}
	return s, nil
}
