// Package paper is een gesimuleerde exchange voor papertrading. Hij gebruikt echte marktdata en
// rekent dezelfde fees, minimumgroottes en afrondingen als de echte exchange.
//
// Vulregels:
//   - Een order die bij plaatsing direct matcht (koop ≥ best ask, verkoop ≤ best bid) wordt meteen
//     gevuld tegen de best ask/bid, met het taker-tarief.
//   - Een rustende order wordt gevuld tegen zijn limietprijs, met het maker-tarief, zodra een
//     1-minuut candle die ná de plaatsing begint er doorheen gaat: low < limiet (koop) of
//     high > limiet (verkoop). Alleen raken is niet genoeg, want dan staan er vaak nog andere
//     orders vóór in de rij.
//   - Orders worden in één keer volledig gevuld.
//
// De status wordt opgeslagen. Na downtime worden de gemiste candles afgespeeld, zodat orders die
// intussen gevuld zouden zijn, alsnog gevuld worden.
package paper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"tradingbot/internal/domain"
	"tradingbot/internal/usecase"
)

// ErrOrderNotFound wordt teruggegeven voor een onbekend order-ID.
var ErrOrderNotFound = errors.New("paper: order niet gevonden")

const (
	stateKey       = "paper_exchange"
	keepDoneOrders = 200
)

type order struct {
	domain.Order
	Hold decimal.Decimal `json:"hold"` // vastgezet bedrag (quote bij koop, base bij verkoop)
}

type balance struct {
	Available decimal.Decimal `json:"available"`
	Hold      decimal.Decimal `json:"hold"`
}

type state struct {
	Balances      map[string]*balance `json:"balances"`
	Orders        []*order            `json:"orders"`
	Fills         []domain.Fill       `json:"fills"`
	LastProcessed time.Time           `json:"last_processed"` // candles vóór dit moment zijn verwerkt
	Seq           int64               `json:"seq"`
}

// Exchange is de gesimuleerde exchange.
type Exchange struct {
	mu      sync.Mutex
	kv      usecase.KeyValueStore
	candles usecase.CandleRepository
	ticker  usecase.TickerSource
	fees    usecase.FeeSource
	clock   usecase.Clock
	product domain.Product
	start   decimal.Decimal
	st      *state
}

var _ usecase.Exchange = (*Exchange)(nil)

// New maakt een paper-exchange met startSaldo in de quote-valuta van het product.
func New(kv usecase.KeyValueStore, candles usecase.CandleRepository, ticker usecase.TickerSource,
	fees usecase.FeeSource, clock usecase.Clock, product domain.Product, startSaldo decimal.Decimal) *Exchange {
	return &Exchange{kv: kv, candles: candles, ticker: ticker, fees: fees, clock: clock, product: product, start: startSaldo}
}

// load laadt de status (eenmalig) en speelt gemiste candles af.
func (e *Exchange) load(ctx context.Context) error {
	if e.st == nil {
		data, ok, err := e.kv.GetValue(ctx, stateKey)
		if err != nil {
			return fmt.Errorf("paper: status laden: %w", err)
		}
		if ok {
			var st state
			if err := json.Unmarshal(data, &st); err != nil {
				return fmt.Errorf("paper: status ontleden: %w", err)
			}
			e.st = &st
		} else {
			base, quote := domain.Currencies(e.product.ID)
			e.st = &state{
				Balances: map[string]*balance{
					base:  {Available: decimal.Zero, Hold: decimal.Zero},
					quote: {Available: e.start, Hold: decimal.Zero},
				},
				LastProcessed: e.clock.Now().UTC().Truncate(time.Minute),
			}
		}
	}
	return e.catchUp(ctx)
}

func (e *Exchange) save(ctx context.Context) error {
	e.prune()
	data, err := json.Marshal(e.st)
	if err != nil {
		return err
	}
	return e.kv.SetValue(ctx, stateKey, data)
}

// catchUp vult rustende orders op basis van de 1-minuut candles sinds de laatste verwerking.
func (e *Exchange) catchUp(ctx context.Context) error {
	now := e.clock.Now().UTC().Truncate(time.Minute)
	if !e.st.LastProcessed.Before(now) {
		return nil
	}
	candles, err := e.candles.Candles(ctx, e.product.ID, e.st.LastProcessed, now)
	if err != nil {
		return fmt.Errorf("paper: candles laden: %w", err)
	}
	if len(candles) == 0 {
		return nil
	}
	fees, err := e.fees.Fees(ctx)
	if err != nil {
		return fmt.Errorf("paper: fees: %w", err)
	}

	for _, c := range candles {
		for _, o := range e.st.Orders {
			if o.Status != domain.OrderOpen || c.Start.Before(o.CreatedAt.Truncate(time.Minute).Add(time.Minute)) {
				continue
			}
			through := (o.Side == domain.SideBuy && c.Low.LessThan(o.Price)) ||
				(o.Side == domain.SideSell && c.High.GreaterThan(o.Price))
			if through {
				e.fill(o, o.Price, domain.LiquidityMaker, fees.Maker, c.Start.Add(time.Minute))
			}
		}
		e.st.LastProcessed = c.Start.Add(time.Minute)
	}
	return nil
}

// fill vult een order volledig tegen price.
func (e *Exchange) fill(o *order, price decimal.Decimal, liquidity domain.Liquidity, rate decimal.Decimal, at time.Time) {
	base, quote := domain.Currencies(e.product.ID)
	value := o.Size.Mul(price)
	fee := value.Mul(rate)

	if o.Side == domain.SideBuy {
		q := e.st.Balances[quote]
		q.Hold = q.Hold.Sub(o.Hold)
		q.Available = q.Available.Add(o.Hold).Sub(value).Sub(fee)
		e.st.Balances[base].Available = e.st.Balances[base].Available.Add(o.Size)
	} else {
		b := e.st.Balances[base]
		b.Hold = b.Hold.Sub(o.Hold)
		e.st.Balances[quote].Available = e.st.Balances[quote].Available.Add(value).Sub(fee)
	}

	o.Hold = decimal.Zero
	o.FilledSize = o.Size
	o.FilledValue = value
	o.Fee = fee
	o.Status = domain.OrderFilled
	o.DoneAt = at

	e.st.Seq++
	e.st.Fills = append(e.st.Fills, domain.Fill{
		TradeID: fmt.Sprintf("paper-trade-%d", e.st.Seq), OrderID: o.ID,
		Price: price, Size: o.Size, Fee: fee, Liquidity: liquidity, Time: at,
	})
}

// PlaceLimitOrder plaatst een limit-order (idempotent op ClientOrderID).
func (e *Exchange) PlaceLimitOrder(ctx context.Context, req usecase.OrderRequest) (domain.Order, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.load(ctx); err != nil {
		return domain.Order{}, err
	}
	for _, o := range e.st.Orders {
		if o.ClientOrderID == req.ClientOrderID {
			return o.Order, nil
		}
	}

	if req.ProductID != e.product.ID {
		return domain.Order{}, fmt.Errorf("paper: onbekend product %s", req.ProductID)
	}
	if !e.product.IsTradable(req.Size, req.Price) {
		return domain.Order{}, fmt.Errorf("paper: order %s @ %s voldoet niet aan de minimumeisen", req.Size, req.Price)
	}
	if !req.Size.Equal(e.product.RoundSizeDown(req.Size)) || !req.Price.Equal(e.product.RoundPriceDown(req.Price)) {
		return domain.Order{}, errors.New("paper: grootte of prijs niet afgerond op de increments")
	}

	fees, err := e.fees.Fees(ctx)
	if err != nil {
		return domain.Order{}, fmt.Errorf("paper: fees: %w", err)
	}
	ticker, err := e.ticker.Ticker(ctx, e.product.ID)
	if err != nil {
		return domain.Order{}, fmt.Errorf("paper: ticker: %w", err)
	}

	base, quote := domain.Currencies(e.product.ID)
	o := &order{Order: domain.Order{
		ClientOrderID: req.ClientOrderID, ProductID: req.ProductID, Side: req.Side,
		Price: req.Price, Size: req.Size, Status: domain.OrderOpen, CreatedAt: e.clock.Now().UTC(),
	}}
	switch req.Side {
	case domain.SideBuy:
		o.Hold = req.Size.Mul(req.Price).Mul(decimal.NewFromInt(1).Add(fees.Taker))
		b := e.st.Balances[quote]
		if b.Available.LessThan(o.Hold) {
			return domain.Order{}, fmt.Errorf("paper: %s nodig, %s beschikbaar: %w", o.Hold, b.Available, usecase.ErrInsufficientFunds)
		}
		b.Available, b.Hold = b.Available.Sub(o.Hold), b.Hold.Add(o.Hold)
	case domain.SideSell:
		o.Hold = req.Size
		b := e.st.Balances[base]
		if b.Available.LessThan(o.Hold) {
			return domain.Order{}, fmt.Errorf("paper: %s %s nodig, %s beschikbaar: %w", o.Hold, base, b.Available, usecase.ErrInsufficientFunds)
		}
		b.Available, b.Hold = b.Available.Sub(o.Hold), b.Hold.Add(o.Hold)
	default:
		return domain.Order{}, fmt.Errorf("paper: onbekende kant %q", req.Side)
	}

	e.st.Seq++
	o.ID = fmt.Sprintf("paper-order-%d", e.st.Seq)
	e.st.Orders = append(e.st.Orders, o)

	switch {
	case req.Side == domain.SideBuy && req.Price.GreaterThanOrEqual(ticker.BestAsk):
		e.fill(o, ticker.BestAsk, domain.LiquidityTaker, fees.Taker, o.CreatedAt)
	case req.Side == domain.SideSell && req.Price.LessThanOrEqual(ticker.BestBid):
		e.fill(o, ticker.BestBid, domain.LiquidityTaker, fees.Taker, o.CreatedAt)
	}

	if err := e.save(ctx); err != nil {
		return domain.Order{}, err
	}
	return o.Order, nil
}

// CancelOrder annuleert een open order. Een order die al klaar is, blijft ongewijzigd.
func (e *Exchange) CancelOrder(ctx context.Context, orderID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.load(ctx); err != nil {
		return err
	}
	o := e.find(orderID)
	if o == nil {
		return ErrOrderNotFound
	}
	if o.Status == domain.OrderOpen {
		base, quote := domain.Currencies(e.product.ID)
		currency := quote
		if o.Side == domain.SideSell {
			currency = base
		}
		b := e.st.Balances[currency]
		b.Hold, b.Available = b.Hold.Sub(o.Hold), b.Available.Add(o.Hold)
		o.Hold = decimal.Zero
		o.Status = domain.OrderCancelled
		o.DoneAt = e.clock.Now().UTC()
	}
	return e.save(ctx)
}

// Order geeft de actuele status van een order.
func (e *Exchange) Order(ctx context.Context, orderID string) (domain.Order, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.load(ctx); err != nil {
		return domain.Order{}, err
	}
	o := e.find(orderID)
	if o == nil {
		return domain.Order{}, ErrOrderNotFound
	}
	if err := e.save(ctx); err != nil {
		return domain.Order{}, err
	}
	return o.Order, nil
}

// Fills geeft de fills van een order.
func (e *Exchange) Fills(ctx context.Context, orderID string) ([]domain.Fill, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.load(ctx); err != nil {
		return nil, err
	}
	var fills []domain.Fill
	for _, f := range e.st.Fills {
		if f.OrderID == orderID {
			fills = append(fills, f)
		}
	}
	return fills, nil
}

// Balances geeft de saldi per valuta.
func (e *Exchange) Balances(ctx context.Context) (map[string]domain.Balance, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.load(ctx); err != nil {
		return nil, err
	}
	if err := e.save(ctx); err != nil {
		return nil, err
	}
	out := make(map[string]domain.Balance, len(e.st.Balances))
	for currency, b := range e.st.Balances {
		out[currency] = domain.Balance{Currency: currency, Available: b.Available, Hold: b.Hold}
	}
	return out, nil
}

func (e *Exchange) find(orderID string) *order {
	for _, o := range e.st.Orders {
		if o.ID == orderID {
			return o
		}
	}
	return nil
}

// prune houdt alleen open orders en de laatste afgeronde orders (met hun fills) bij.
func (e *Exchange) prune() {
	done := 0
	for _, o := range e.st.Orders {
		if o.Status.Done() {
			done++
		}
	}
	if done <= keepDoneOrders {
		return
	}
	drop := done - keepDoneOrders
	removed := map[string]bool{}
	kept := e.st.Orders[:0]
	for _, o := range e.st.Orders {
		if drop > 0 && o.Status.Done() {
			removed[o.ID] = true
			drop--
			continue
		}
		kept = append(kept, o)
	}
	e.st.Orders = kept
	fills := e.st.Fills[:0]
	for _, f := range e.st.Fills {
		if !removed[f.OrderID] {
			fills = append(fills, f)
		}
	}
	e.st.Fills = fills
}
