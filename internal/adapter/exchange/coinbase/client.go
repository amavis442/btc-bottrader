// Package coinbase is de adapter voor de Coinbase Advanced Trade API.
package coinbase

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/shopspring/decimal"

	"tradingbot/internal/domain"
	"tradingbot/internal/usecase"
)

// DefaultBaseURL is de basis-URL van de Advanced Trade API.
const DefaultBaseURL = "https://api.coinbase.com/api/v3/brokerage"

// Client praat met de Coinbase Advanced Trade API: publieke marktdata zonder key, en met een Signer
// ook de private endpoints (saldi, orders, fills en fees).
type Client struct {
	baseURL    string
	http       *http.Client
	maxRetries int
	backoff    time.Duration
	signer     *Signer
}

// Option past de client aan.
type Option func(*Client)

// WithBaseURL zet een andere basis-URL (voor tests).
func WithBaseURL(u string) Option { return func(c *Client) { c.baseURL = u } }

// WithSigner zet de API-key voor de private endpoints.
func WithSigner(s *Signer) Option { return func(c *Client) { c.signer = s } }

// WithHTTPClient zet een eigen HTTP-client.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// WithRetry zet het aantal herhalingen en de start-wachttijd (die per poging verdubbelt).
func WithRetry(maxRetries int, backoff time.Duration) Option {
	return func(c *Client) { c.maxRetries, c.backoff = maxRetries, backoff }
}

// New maakt een client.
func New(opts ...Option) *Client {
	c := &Client{
		baseURL:    DefaultBaseURL,
		http:       &http.Client{Timeout: 15 * time.Second},
		maxRetries: 4,
		backoff:    500 * time.Millisecond,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

var (
	_ usecase.MarketData   = (*Client)(nil)
	_ usecase.TickerSource = (*Client)(nil)
)

type productResponse struct {
	ProductID       string          `json:"product_id"`
	BaseIncrement   decimal.Decimal `json:"base_increment"`
	QuoteIncrement  decimal.Decimal `json:"quote_increment"`
	BaseMinSize     decimal.Decimal `json:"base_min_size"`
	QuoteMinSize    decimal.Decimal `json:"quote_min_size"`
	Status          string          `json:"status"`
	IsDisabled      bool            `json:"is_disabled"`
	TradingDisabled bool            `json:"trading_disabled"`
	CancelOnly      bool            `json:"cancel_only"`
}

// Product haalt de handelsregels van een product op. Een onbekend, offline of geblokkeerd product
// geeft usecase.ErrProductUnavailable.
func (c *Client) Product(ctx context.Context, productID string) (domain.Product, error) {
	var resp productResponse
	err := c.get(ctx, "/market/products/"+url.PathEscape(productID), nil, &resp)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
		return domain.Product{}, fmt.Errorf("%s: %w", productID, usecase.ErrProductUnavailable)
	}
	if err != nil {
		return domain.Product{}, err
	}
	if resp.Status != "online" || resp.IsDisabled || resp.TradingDisabled || resp.CancelOnly {
		return domain.Product{}, fmt.Errorf("%s (status %q): %w", productID, resp.Status, usecase.ErrProductUnavailable)
	}

	product := domain.Product{
		ID:             resp.ProductID,
		BaseIncrement:  resp.BaseIncrement,
		QuoteIncrement: resp.QuoteIncrement,
		BaseMinSize:    resp.BaseMinSize,
		QuoteMinSize:   resp.QuoteMinSize,
	}
	if err := product.Validate(); err != nil {
		return domain.Product{}, fmt.Errorf("coinbase: onverwachte productregels: %w", err)
	}
	return product, nil
}

type candlesResponse struct {
	Candles []struct {
		Start  string          `json:"start"`
		Open   decimal.Decimal `json:"open"`
		High   decimal.Decimal `json:"high"`
		Low    decimal.Decimal `json:"low"`
		Close  decimal.Decimal `json:"close"`
		Volume decimal.Decimal `json:"volume"`
	} `json:"candles"`
}

// Candles haalt 1-minuut candles op met een start in [start, end), oudste eerst.
// Coinbase staat maximaal 350 candles per verzoek toe.
func (c *Client) Candles(ctx context.Context, productID string, start, end time.Time) ([]domain.Candle, error) {
	query := url.Values{
		"start":       {strconv.FormatInt(start.Unix(), 10)},
		"end":         {strconv.FormatInt(end.Unix(), 10)},
		"granularity": {"ONE_MINUTE"},
	}
	var resp candlesResponse
	if err := c.get(ctx, "/market/products/"+url.PathEscape(productID)+"/candles", query, &resp); err != nil {
		return nil, err
	}

	candles := make([]domain.Candle, 0, len(resp.Candles))
	for _, rc := range resp.Candles {
		unix, err := strconv.ParseInt(rc.Start, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("coinbase: ongeldige candle-start %q: %w", rc.Start, err)
		}
		ts := time.Unix(unix, 0).UTC()
		if ts.Before(start) || !ts.Before(end) {
			continue
		}
		candles = append(candles, domain.Candle{
			Start: ts, Open: rc.Open, High: rc.High, Low: rc.Low, Close: rc.Close, Volume: rc.Volume,
		})
	}
	sort.Slice(candles, func(i, j int) bool { return candles[i].Start.Before(candles[j].Start) })
	return candles, nil
}

type tickerResponse struct {
	Trades []struct {
		Price decimal.Decimal `json:"price"`
		Time  time.Time       `json:"time"`
	} `json:"trades"`
	BestBid decimal.Decimal `json:"best_bid"`
	BestAsk decimal.Decimal `json:"best_ask"`
}

// Ticker haalt de laatste handelsprijs en de beste bied- en laatprijs op.
func (c *Client) Ticker(ctx context.Context, productID string) (domain.Ticker, error) {
	var resp tickerResponse
	query := url.Values{"limit": {"1"}}
	if err := c.get(ctx, "/market/products/"+url.PathEscape(productID)+"/ticker", query, &resp); err != nil {
		return domain.Ticker{}, err
	}
	if len(resp.Trades) == 0 || !resp.BestBid.IsPositive() || !resp.BestAsk.IsPositive() {
		return domain.Ticker{}, fmt.Errorf("coinbase: onvolledige ticker voor %s", productID)
	}
	return domain.Ticker{
		Price:   resp.Trades[0].Price,
		BestBid: resp.BestBid,
		BestAsk: resp.BestAsk,
		Time:    resp.Trades[0].Time.UTC(),
	}, nil
}

// APIError is een foutantwoord van Coinbase.
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("coinbase: HTTP %d: %s", e.StatusCode, e.Body)
}

func (e *APIError) retryable() bool {
	return e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500
}

// get voert een publieke GET uit.
func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	return c.request(ctx, http.MethodGet, path, query, nil, out, false)
}

// privateGet voert een ondertekende GET uit.
func (c *Client) privateGet(ctx context.Context, path string, query url.Values, out any) error {
	return c.request(ctx, http.MethodGet, path, query, nil, out, true)
}

// privatePost voert een ondertekende POST uit. Alleen gebruiken voor verzoeken die veilig herhaald
// kunnen worden (orders met een vast client_order_id, annuleren).
func (c *Client) privatePost(ctx context.Context, path string, body, out any) error {
	return c.request(ctx, http.MethodPost, path, nil, body, out, true)
}

// request voert een verzoek uit met retries (exponential backoff) bij netwerkfouten, 429 en 5xx.
// Bij een ondertekend verzoek wordt per poging een nieuw JWT gemaakt.
func (c *Client) request(ctx context.Context, method, path string, query url.Values, body, out any, private bool) error {
	if private && c.signer == nil {
		return errors.New("coinbase: geen API-key ingesteld")
	}
	base, err := url.Parse(c.baseURL)
	if err != nil {
		return fmt.Errorf("coinbase: ongeldige basis-URL: %w", err)
	}
	u := *base
	u.Path = base.Path + path
	u.RawQuery = query.Encode()

	var payload []byte
	if body != nil {
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}

	wait := c.backoff
	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			wait *= 2
		}

		token := ""
		if private {
			if token, err = c.signer.Token(method, u.Host, u.Path); err != nil {
				return fmt.Errorf("coinbase: JWT maken: %w", err)
			}
		}
		lastErr = c.do(ctx, method, u.String(), payload, token, out)
		if lastErr == nil {
			return nil
		}
		var apiErr *APIError
		if errors.As(lastErr, &apiErr) && !apiErr.retryable() {
			return lastErr
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return fmt.Errorf("coinbase: na %d pogingen: %w", c.maxRetries+1, lastErr)
}

func (c *Client) do(ctx context.Context, method, u string, payload []byte, token string, out any) error {
	var reqBody io.Reader
	if payload != nil {
		reqBody = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, reqBody)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("coinbase: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return fmt.Errorf("coinbase: antwoord lezen: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return &APIError{StatusCode: resp.StatusCode, Body: string(data)}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("coinbase: antwoord ontleden: %w", err)
	}
	return nil
}
