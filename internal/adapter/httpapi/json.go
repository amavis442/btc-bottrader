package httpapi

import (
	"time"

	"github.com/shopspring/decimal"

	"tradingbot/internal/usecase"
)

// De JSON-vormen van de API. Bedragen staan als tekst, zodat er geen precisie verloren gaat.

type orderJSON struct {
	ID            string          `json:"id"`
	ClientOrderID string          `json:"client_order_id"`
	PositionID    int64           `json:"position_id,omitempty"`
	Purpose       string          `json:"purpose"`
	ProductID     string          `json:"product_id"`
	Side          string          `json:"side"`
	Price         decimal.Decimal `json:"price"`
	Size          decimal.Decimal `json:"size"`
	FilledSize    decimal.Decimal `json:"filled_size"`
	FilledValue   decimal.Decimal `json:"filled_value"`
	AveragePrice  decimal.Decimal `json:"average_price"`
	Fee           decimal.Decimal `json:"fee"`
	Status        string          `json:"status"`
	CreatedAt     time.Time       `json:"created_at"`
	DoneAt        *time.Time      `json:"done_at"`
}

func toOrderJSON(o usecase.OrderRecord) orderJSON {
	j := orderJSON{ID: o.ID, ClientOrderID: o.ClientOrderID, PositionID: o.PositionID, Purpose: string(o.Purpose),
		ProductID: o.ProductID, Side: string(o.Side), Price: o.Price, Size: o.Size, FilledSize: o.FilledSize,
		FilledValue: o.FilledValue, AveragePrice: o.AveragePrice().Round(2), Fee: o.Fee, Status: string(o.Status),
		CreatedAt: o.CreatedAt}
	if !o.DoneAt.IsZero() {
		j.DoneAt = &o.DoneAt
	}
	return j
}

type tradeJSON struct {
	TradeID    string          `json:"trade_id"`
	OrderID    string          `json:"order_id"`
	PositionID int64           `json:"position_id,omitempty"`
	Purpose    string          `json:"purpose"`
	Side       string          `json:"side"`
	Price      decimal.Decimal `json:"price"`
	Size       decimal.Decimal `json:"size"`
	Value      decimal.Decimal `json:"value"`
	Fee        decimal.Decimal `json:"fee"`
	Liquidity  string          `json:"liquidity"`
	Time       time.Time       `json:"time"`
}

func toTradeJSON(t usecase.TradeRecord) tradeJSON {
	return tradeJSON{TradeID: t.TradeID, OrderID: t.OrderID, PositionID: t.PositionID, Purpose: string(t.Purpose),
		Side: string(t.Side), Price: t.Price, Size: t.Size, Value: t.Price.Mul(t.Size), Fee: t.Fee,
		Liquidity: string(t.Liquidity), Time: t.Time}
}

type candleJSON struct {
	Start  time.Time       `json:"start"`
	Open   decimal.Decimal `json:"open"`
	High   decimal.Decimal `json:"high"`
	Low    decimal.Decimal `json:"low"`
	Close  decimal.Decimal `json:"close"`
	Volume decimal.Decimal `json:"volume"`
}
