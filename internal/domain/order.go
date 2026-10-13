package domain

import (
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// Side is de kant van een order.
type Side string

const (
	SideBuy  Side = "BUY"
	SideSell Side = "SELL"
)

// OrderStatus is de status van een order op de exchange.
type OrderStatus string

const (
	OrderOpen      OrderStatus = "OPEN"
	OrderFilled    OrderStatus = "FILLED"
	OrderCancelled OrderStatus = "CANCELLED"
)

// Done geeft aan of de order niet meer kan veranderen.
func (s OrderStatus) Done() bool { return s == OrderFilled || s == OrderCancelled }

// Liquidity geeft aan of een fill maker of taker was.
type Liquidity string

const (
	LiquidityMaker Liquidity = "MAKER"
	LiquidityTaker Liquidity = "TAKER"
)

// Order is een limit-order zoals de exchange hem kent.
type Order struct {
	ID            string
	ClientOrderID string
	ProductID     string
	Side          Side
	Price         decimal.Decimal // limietprijs
	Size          decimal.Decimal // gevraagde hoeveelheid BTC
	FilledSize    decimal.Decimal // gevulde hoeveelheid BTC
	FilledValue   decimal.Decimal // gevulde waarde in quote-valuta, exclusief fee
	Fee           decimal.Decimal // totaal betaalde fee in quote-valuta
	Status        OrderStatus
	CreatedAt     time.Time
	DoneAt        time.Time // moment van volledig vullen of annuleren
}

// AveragePrice geeft de gemiddelde vulprijs, of nul als er niets gevuld is.
func (o Order) AveragePrice() decimal.Decimal {
	if !o.FilledSize.IsPositive() {
		return decimal.Zero
	}
	return o.FilledValue.Div(o.FilledSize)
}

// Fill is een (deel)uitvoering van een order.
type Fill struct {
	TradeID   string
	OrderID   string
	Price     decimal.Decimal
	Size      decimal.Decimal
	Fee       decimal.Decimal
	Liquidity Liquidity
	Time      time.Time
}

// Balance is het saldo van één valuta.
type Balance struct {
	Currency  string
	Available decimal.Decimal // vrij te gebruiken
	Hold      decimal.Decimal // vastgezet in openstaande orders
}

// Total geeft het totale saldo (vrij + vast).
func (b Balance) Total() decimal.Decimal { return b.Available.Add(b.Hold) }

// Ticker is de actuele marktprijs.
type Ticker struct {
	Price   decimal.Decimal // laatste handelsprijs
	BestBid decimal.Decimal
	BestAsk decimal.Decimal
	Time    time.Time
}

// Currencies splitst een product-ID als "BTC-EUR" in base- en quote-valuta.
func Currencies(productID string) (base, quote string) {
	base, quote, _ = strings.Cut(productID, "-")
	return base, quote
}
