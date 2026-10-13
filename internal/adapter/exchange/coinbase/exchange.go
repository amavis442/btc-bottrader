package coinbase

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"tradingbot/internal/domain"
	"tradingbot/internal/domain/strategy"
	"tradingbot/internal/usecase"
)

// ErrOrderNotFound wordt teruggegeven als Coinbase een order niet kent.
var ErrOrderNotFound = errors.New("coinbase: order niet gevonden")

// Exchange is de live-exchange: echte orders op Coinbase. Vereist een client met een Signer.
type Exchange struct {
	client  *Client
	product domain.Product
	now     func() time.Time
}

var (
	_ usecase.Exchange  = (*Exchange)(nil)
	_ usecase.FeeSource = (*Client)(nil)
)

// NewExchange maakt de live-exchange voor één product.
func NewExchange(client *Client, product domain.Product) *Exchange {
	return &Exchange{client: client, product: product, now: time.Now}
}

// ---- orders plaatsen ----

type limitGTC struct {
	BaseSize   string `json:"base_size"`
	LimitPrice string `json:"limit_price"`
	PostOnly   bool   `json:"post_only"`
}

type createOrderRequest struct {
	ClientOrderID      string `json:"client_order_id"`
	ProductID          string `json:"product_id"`
	Side               string `json:"side"`
	OrderConfiguration struct {
		LimitGTC limitGTC `json:"limit_limit_gtc"`
	} `json:"order_configuration"`
}

type createOrderResponse struct {
	Success         bool `json:"success"`
	SuccessResponse struct {
		OrderID       string `json:"order_id"`
		ClientOrderID string `json:"client_order_id"`
	} `json:"success_response"`
	ErrorResponse struct {
		Error                 string `json:"error"`
		Message               string `json:"message"`
		ErrorDetails          string `json:"error_details"`
		NewOrderFailureReason string `json:"new_order_failure_reason"`
	} `json:"error_response"`
}

// PlaceLimitOrder plaatst een limit-order (good-til-cancelled). Coinbase maakt geen tweede order aan
// voor een bestaand client_order_id; in dat geval wordt de bestaande order opgezocht en teruggegeven.
func (e *Exchange) PlaceLimitOrder(ctx context.Context, req usecase.OrderRequest) (domain.Order, error) {
	if req.ProductID != e.product.ID {
		return domain.Order{}, fmt.Errorf("coinbase: onverwacht product %s", req.ProductID)
	}
	body := createOrderRequest{ClientOrderID: req.ClientOrderID, ProductID: req.ProductID, Side: string(req.Side)}
	body.OrderConfiguration.LimitGTC = limitGTC{BaseSize: req.Size.String(), LimitPrice: req.Price.String()}

	var resp createOrderResponse
	if err := e.client.privatePost(ctx, "/orders", body, &resp); err != nil {
		return domain.Order{}, err
	}
	if resp.Success {
		return e.Order(ctx, resp.SuccessResponse.OrderID)
	}

	er := resp.ErrorResponse
	reason := strings.Join(nonEmpty(er.NewOrderFailureReason, er.Error, er.Message, er.ErrorDetails), ": ")
	switch {
	case strings.Contains(er.NewOrderFailureReason, "DUPLICATE_CLIENT_ORDER_ID") || strings.Contains(er.Error, "DUPLICATE"):
		return e.findByClientOrderID(ctx, req.ClientOrderID)
	case strings.Contains(strings.ToUpper(reason), "INSUFFICIENT"):
		return domain.Order{}, fmt.Errorf("coinbase: %s: %w", reason, usecase.ErrInsufficientFunds)
	}
	return domain.Order{}, fmt.Errorf("coinbase: order geweigerd: %s", reason)
}

func nonEmpty(values ...string) []string {
	var out []string
	for _, v := range values {
		if v != "" && v != "UNKNOWN_FAILURE_REASON" {
			out = append(out, v)
		}
	}
	return out
}

// findByClientOrderID zoekt een order op client_order_id in de orders van de afgelopen 7 dagen.
// Coinbase heeft geen filter op client_order_id, dus er wordt door de lijst gebladerd.
func (e *Exchange) findByClientOrderID(ctx context.Context, clientOrderID string) (domain.Order, error) {
	query := url.Values{
		"product_ids": {e.product.ID},
		"start_date":  {e.now().UTC().Add(-7 * 24 * time.Hour).Format(time.RFC3339)},
		"limit":       {"250"},
	}
	for range 20 { // maximaal 5000 orders doorzoeken
		var resp listOrdersResponse
		if err := e.client.privateGet(ctx, "/orders/historical/batch", query, &resp); err != nil {
			return domain.Order{}, err
		}
		for _, o := range resp.Orders {
			if o.ClientOrderID == clientOrderID {
				return o.toDomain()
			}
		}
		if !resp.HasNext || resp.Cursor == "" {
			break
		}
		query.Set("cursor", resp.Cursor)
	}
	return domain.Order{}, fmt.Errorf("coinbase: order met client_order_id %s bestaat volgens Coinbase, maar is niet gevonden", clientOrderID)
}

// ---- orders opvragen ----

type orderResponse struct {
	OrderID            string `json:"order_id"`
	ClientOrderID      string `json:"client_order_id"`
	ProductID          string `json:"product_id"`
	Side               string `json:"side"`
	Status             string `json:"status"`
	OrderConfiguration struct {
		LimitGTC *struct {
			BaseSize   decimal.Decimal `json:"base_size"`
			LimitPrice decimal.Decimal `json:"limit_price"`
		} `json:"limit_limit_gtc"`
	} `json:"order_configuration"`
	FilledSize         decimal.Decimal `json:"filled_size"`
	AverageFilledPrice decimal.Decimal `json:"average_filled_price"`
	TotalFees          decimal.Decimal `json:"total_fees"`
	CreatedTime        time.Time       `json:"created_time"`
	LastFillTime       *time.Time      `json:"last_fill_time"`
	LastUpdateTime     *time.Time      `json:"last_update_time"`
}

type listOrdersResponse struct {
	Orders  []orderResponse `json:"orders"`
	HasNext bool            `json:"has_next"`
	Cursor  string          `json:"cursor"`
}

// toDomain zet een Coinbase-order om. Statussen waarin de order nog kan vullen (ook PENDING, QUEUED en
// CANCEL_QUEUED) gelden als open; EXPIRED en FAILED gelden als geannuleerd.
func (o orderResponse) toDomain() (domain.Order, error) {
	if o.OrderConfiguration.LimitGTC == nil {
		return domain.Order{}, fmt.Errorf("coinbase: order %s is geen limit-GTC-order", o.OrderID)
	}
	d := domain.Order{
		ID: o.OrderID, ClientOrderID: o.ClientOrderID, ProductID: o.ProductID, Side: domain.Side(o.Side),
		Price: o.OrderConfiguration.LimitGTC.LimitPrice, Size: o.OrderConfiguration.LimitGTC.BaseSize,
		FilledSize: o.FilledSize, FilledValue: o.FilledSize.Mul(o.AverageFilledPrice), Fee: o.TotalFees,
		CreatedAt: o.CreatedTime.UTC(),
	}
	switch o.Status {
	case "FILLED":
		d.Status = domain.OrderFilled
		d.DoneAt = firstTime(o.LastFillTime, o.LastUpdateTime)
	case "CANCELLED", "EXPIRED", "FAILED":
		d.Status = domain.OrderCancelled
		d.DoneAt = firstTime(o.LastUpdateTime, o.LastFillTime)
	default:
		d.Status = domain.OrderOpen
	}
	return d, nil
}

func firstTime(times ...*time.Time) time.Time {
	for _, t := range times {
		if t != nil && !t.IsZero() {
			return t.UTC()
		}
	}
	return time.Time{}
}

// Order geeft de actuele status van een order.
func (e *Exchange) Order(ctx context.Context, orderID string) (domain.Order, error) {
	var resp struct {
		Order orderResponse `json:"order"`
	}
	err := e.client.privateGet(ctx, "/orders/historical/"+url.PathEscape(orderID), nil, &resp)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
		return domain.Order{}, fmt.Errorf("%w: %s", ErrOrderNotFound, orderID)
	}
	if err != nil {
		return domain.Order{}, err
	}
	return resp.Order.toDomain()
}

// ---- annuleren ----

type cancelResponse struct {
	Results []struct {
		Success       bool   `json:"success"`
		FailureReason string `json:"failure_reason"`
		OrderID       string `json:"order_id"`
	} `json:"results"`
}

// CancelOrder annuleert een open order. Een order die al gevuld is of al geannuleerd wordt, is geen fout.
func (e *Exchange) CancelOrder(ctx context.Context, orderID string) error {
	var resp cancelResponse
	if err := e.client.privatePost(ctx, "/orders/batch_cancel", map[string][]string{"order_ids": {orderID}}, &resp); err != nil {
		return err
	}
	if len(resp.Results) != 1 {
		return fmt.Errorf("coinbase: onverwacht antwoord bij annuleren van %s", orderID)
	}
	r := resp.Results[0]
	switch {
	case r.Success, r.FailureReason == "ORDER_IS_FULLY_FILLED", r.FailureReason == "DUPLICATE_CANCEL_REQUEST":
		return nil
	case r.FailureReason == "UNKNOWN_CANCEL_ORDER":
		return fmt.Errorf("%w: %s", ErrOrderNotFound, orderID)
	}
	return fmt.Errorf("coinbase: annuleren van %s geweigerd: %s", orderID, r.FailureReason)
}

// ---- fills ----

type fillsResponse struct {
	Fills []struct {
		EntryID            string          `json:"entry_id"`
		TradeID            string          `json:"trade_id"`
		OrderID            string          `json:"order_id"`
		TradeTime          time.Time       `json:"trade_time"`
		TradeType          string          `json:"trade_type"`
		Price              decimal.Decimal `json:"price"`
		Size               decimal.Decimal `json:"size"`
		Commission         decimal.Decimal `json:"commission"`
		LiquidityIndicator string          `json:"liquidity_indicator"`
		SizeInQuote        bool            `json:"size_in_quote"`
	} `json:"fills"`
	Cursor string `json:"cursor"`
}

// Fills geeft alle fills van een order. Alleen gewone fills (trade_type FILL) tellen mee.
func (e *Exchange) Fills(ctx context.Context, orderID string) ([]domain.Fill, error) {
	query := url.Values{"order_ids": {orderID}, "limit": {"100"}}
	var fills []domain.Fill
	for range 50 {
		var resp fillsResponse
		if err := e.client.privateGet(ctx, "/orders/historical/fills", query, &resp); err != nil {
			return nil, err
		}
		for _, f := range resp.Fills {
			if f.TradeType != "" && f.TradeType != "FILL" {
				continue
			}
			if f.SizeInQuote {
				return nil, fmt.Errorf("coinbase: fill %s is in quote-valuta; de bot plaatst alleen orders in BTC", f.TradeID)
			}
			id := f.EntryID
			if id == "" {
				id = f.TradeID
			}
			liquidity := domain.LiquidityTaker
			if f.LiquidityIndicator == "MAKER" {
				liquidity = domain.LiquidityMaker
			}
			fills = append(fills, domain.Fill{TradeID: id, OrderID: f.OrderID, Price: f.Price, Size: f.Size,
				Fee: f.Commission, Liquidity: liquidity, Time: f.TradeTime.UTC()})
		}
		if resp.Cursor == "" || len(resp.Fills) == 0 {
			return fills, nil
		}
		query.Set("cursor", resp.Cursor)
	}
	return fills, nil
}

// ---- saldi ----

type amount struct {
	Value    decimal.Decimal `json:"value"`
	Currency string          `json:"currency"`
}

type accountsResponse struct {
	Accounts []struct {
		Currency         string `json:"currency"`
		AvailableBalance amount `json:"available_balance"`
		Hold             amount `json:"hold"`
		Active           bool   `json:"active"`
		Type             string `json:"type"`
	} `json:"accounts"`
	HasNext bool   `json:"has_next"`
	Cursor  string `json:"cursor"`
}

// Balances geeft de saldi per valuta van de gewone (crypto- en fiat-)rekeningen.
func (e *Exchange) Balances(ctx context.Context) (map[string]domain.Balance, error) {
	query := url.Values{"limit": {"250"}}
	out := map[string]domain.Balance{}
	for range 20 {
		var resp accountsResponse
		if err := e.client.privateGet(ctx, "/accounts", query, &resp); err != nil {
			return nil, err
		}
		for _, a := range resp.Accounts {
			if !a.Active || (a.Type != "ACCOUNT_TYPE_CRYPTO" && a.Type != "ACCOUNT_TYPE_FIAT") {
				continue // kluizen en futures-rekeningen zijn niet beschikbaar voor spot-orders
			}
			b := out[a.Currency]
			b.Currency = a.Currency
			b.Available = b.Available.Add(a.AvailableBalance.Value)
			b.Hold = b.Hold.Add(a.Hold.Value)
			out[a.Currency] = b
		}
		if !resp.HasNext || resp.Cursor == "" {
			break
		}
		query.Set("cursor", resp.Cursor)
	}
	return out, nil
}

// ---- fees ----

type transactionSummaryResponse struct {
	FeeTier struct {
		PricingTier  string          `json:"pricing_tier"`
		MakerFeeRate decimal.Decimal `json:"maker_fee_rate"`
		TakerFeeRate decimal.Decimal `json:"taker_fee_rate"`
	} `json:"fee_tier"`
}

// maxFeeRate is een veiligheidsgrens: een hoger tarief wijst op een verkeerd begrepen antwoord
// (bijv. een percentage in plaats van een fractie).
var maxFeeRate = decimal.RequireFromString("0.05")

// Fees haalt de actuele maker- en taker-tarieven van het account op (als fractie, 0.006 = 0,6%).
func (c *Client) Fees(ctx context.Context) (strategy.Fees, error) {
	var resp transactionSummaryResponse
	if err := c.privateGet(ctx, "/transaction_summary", url.Values{"product_type": {"SPOT"}}, &resp); err != nil {
		return strategy.Fees{}, err
	}
	fees := strategy.Fees{Maker: resp.FeeTier.MakerFeeRate, Taker: resp.FeeTier.TakerFeeRate}
	for _, rate := range []decimal.Decimal{fees.Maker, fees.Taker} {
		if rate.IsNegative() || rate.GreaterThan(maxFeeRate) {
			return strategy.Fees{}, fmt.Errorf("coinbase: onverwacht fee-tarief %s (tier %q)", rate, resp.FeeTier.PricingTier)
		}
	}
	return fees, nil
}
