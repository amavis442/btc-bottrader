// Package usecase bevat de applicatielogica. Hij kent alleen de interfaces hieronder; de adapters
// (Coinbase, SQLite, HTTP, Telegram) implementeren ze.
package usecase

import (
	"context"
	"errors"
	"time"

	"github.com/shopspring/decimal"

	"tradingbot/internal/domain"
	"tradingbot/internal/domain/strategy"
)

// ErrProductUnavailable wordt teruggegeven als een product niet bestaat of niet verhandelbaar is.
var ErrProductUnavailable = errors.New("product niet beschikbaar")

// Clock levert de huidige tijd, zodat tijdsafhankelijke logica testbaar is.
type Clock interface {
	Now() time.Time
}

// MarketData levert publieke marktdata van de exchange.
type MarketData interface {
	// Product geeft de handelsregels van een product, of ErrProductUnavailable.
	Product(ctx context.Context, productID string) (domain.Product, error)
	// Candles geeft de 1-minuut candles met een start in [start, end), oudste eerst.
	// De candle van de lopende minuut kan erbij zitten.
	Candles(ctx context.Context, productID string, start, end time.Time) ([]domain.Candle, error)
}

// TickerSource levert de actuele prijs van een product.
type TickerSource interface {
	Ticker(ctx context.Context, productID string) (domain.Ticker, error)
}

// ErrInsufficientFunds wordt teruggegeven als het saldo te laag is voor een order.
var ErrInsufficientFunds = errors.New("onvoldoende saldo")

// OrderRequest is een verzoek om een limit-order te plaatsen.
type OrderRequest struct {
	ClientOrderID string
	ProductID     string
	Side          domain.Side
	Size          decimal.Decimal
	Price         decimal.Decimal
}

// Exchange voert orders uit. Zowel de paper-exchange als Coinbase (live) implementeren deze interface.
type Exchange interface {
	// PlaceLimitOrder plaatst een limit-order. Bestaat er al een order met hetzelfde ClientOrderID,
	// dan wordt die teruggegeven in plaats van een nieuwe te plaatsen (idempotent).
	PlaceLimitOrder(ctx context.Context, req OrderRequest) (domain.Order, error)
	// CancelOrder annuleert een open order. Een order die al klaar is, blijft ongewijzigd.
	CancelOrder(ctx context.Context, orderID string) error
	// Order geeft de actuele status van een order.
	Order(ctx context.Context, orderID string) (domain.Order, error)
	// Fills geeft alle fills van een order.
	Fills(ctx context.Context, orderID string) ([]domain.Fill, error)
	// Balances geeft de saldi per valuta.
	Balances(ctx context.Context) (map[string]domain.Balance, error)
}

// FeeSource levert de actuele fee-tarieven.
type FeeSource interface {
	Fees(ctx context.Context) (strategy.Fees, error)
}

// NotificationLevel is het belang van een melding.
type NotificationLevel string

const (
	NotifyInfo    NotificationLevel = "INFO"
	NotifyWarning NotificationLevel = "WAARSCHUWING"
	NotifyError   NotificationLevel = "FOUT"
)

// Notifier verstuurt meldingen (Telegram, log, ...). Een mislukte melding mag de bot niet stoppen.
type Notifier interface {
	Notify(ctx context.Context, level NotificationLevel, title, message string)
}

// KeyValueStore slaat kleine JSON-documenten op, zoals de toestand van de trader.
type KeyValueStore interface {
	GetValue(ctx context.Context, key string) ([]byte, bool, error)
	SetValue(ctx context.Context, key string, value []byte) error
}

// OrderRecord is een order zoals de bot hem administreert.
type OrderRecord struct {
	domain.Order
	PositionID int64 // 0 als de order (nog) niet bij een positie hoort
	Purpose    OrderPurpose
}

// OrderPurpose geeft aan waarvoor een order dient.
type OrderPurpose string

const (
	PurposeBuy         OrderPurpose = "KOOP"
	PurposeTakeProfit1 OrderPurpose = "WINST1"
	PurposeTakeProfit2 OrderPurpose = "WINST2"
	PurposeStop        OrderPurpose = "STOP"
)

// PositionRecord is een positie zoals de bot hem administreert.
type PositionRecord struct {
	ID            int64
	ProductID     string
	OpenedAt      time.Time
	ClosedAt      time.Time // nul zolang de positie open is
	EntryQuantity decimal.Decimal
	CostPrice     decimal.Decimal
}

// TradingStore is de administratie van de trader: orders, fills, posities en de audit log.
type TradingStore interface {
	SaveOrder(ctx context.Context, o OrderRecord) error
	SaveFills(ctx context.Context, fills []domain.Fill) error
	OrderFills(ctx context.Context, orderIDs []string) ([]domain.Fill, error)
	PositionOrders(ctx context.Context, positionID int64) ([]OrderRecord, error)
	OpenPosition(ctx context.Context, p PositionRecord) (int64, error)
	AssignOrderToPosition(ctx context.Context, orderID string, positionID int64) error
	ClosePosition(ctx context.Context, positionID int64, closedAt time.Time) error
	AddEvent(ctx context.Context, at time.Time, kind, message string) error
}

// OrderFilter beperkt een lijst orders. Lege velden filteren niet.
type OrderFilter struct {
	Status domain.OrderStatus
	From   time.Time // aangemaakt op of na
	To     time.Time // aangemaakt vóór
	Limit  int
}

// TradeFilter beperkt een lijst fills. Lege velden filteren niet.
type TradeFilter struct {
	From  time.Time
	To    time.Time
	Limit int
}

// TradeRecord is een fill met de order- en positiegegevens erbij.
type TradeRecord struct {
	domain.Fill
	PositionID int64
	Purpose    OrderPurpose
	Side       domain.Side
}

// PositionFilter beperkt een lijst posities.
type PositionFilter struct {
	Closed     bool      // alleen afgeronde posities
	ClosedFrom time.Time // afgerond op of na (alleen met Closed)
	Limit      int
}

// ReportStore levert de gegevens voor de rapportage en de API.
type ReportStore interface {
	ListOrders(ctx context.Context, f OrderFilter) ([]OrderRecord, error)
	ListTrades(ctx context.Context, f TradeFilter) ([]TradeRecord, error)
	ListPositions(ctx context.Context, f PositionFilter) ([]PositionRecord, error)
	GetPosition(ctx context.Context, id int64) (PositionRecord, bool, error)
	PositionOrders(ctx context.Context, positionID int64) ([]OrderRecord, error)
	OrderFills(ctx context.Context, orderIDs []string) ([]domain.Fill, error)
}

// CandleRepository slaat 1-minuut candles op.
type CandleRepository interface {
	// SaveCandles slaat candles op; een bestaande candle met dezelfde start wordt overschreven.
	SaveCandles(ctx context.Context, productID string, candles []domain.Candle) error
	// Candles geeft de candles met een start in [from, to), oudste eerst.
	Candles(ctx context.Context, productID string, from, to time.Time) ([]domain.Candle, error)
	// LatestCandleStart geeft de start van de nieuwste opgeslagen candle; ok is false als er geen is.
	LatestCandleStart(ctx context.Context, productID string) (start time.Time, ok bool, err error)
}
