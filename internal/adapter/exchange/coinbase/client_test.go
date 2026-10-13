package coinbase

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"tradingbot/internal/usecase"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return New(WithBaseURL(srv.URL), WithRetry(2, time.Millisecond))
}

// Opgenomen antwoord van GET /market/products/BTC-EUR (ingekort).
const productBody = `{"product_id":"BTC-EUR", "price":"74333.96", "base_increment":"0.00000001",
	"quote_increment":"0.01", "quote_min_size":"1", "base_min_size":"0.00000001", "status":"online",
	"is_disabled":false, "cancel_only":false, "trading_disabled":false}`

// Opgenomen antwoord van GET /market/products/BTC-EUR/candles (nieuwste eerst, inclusief lopende minuut).
const candlesBody = `{"candles":[
	{"start":"1791896100", "low":"74333.04", "high":"74342", "open":"74333.04", "close":"74340.82", "volume":"0.14995326"},
	{"start":"1791896040", "low":"74323.76", "high":"74356.66", "open":"74343.35", "close":"74344.17", "volume":"0.00444068"},
	{"start":"1791895980", "low":"74345.15", "high":"74365.35", "open":"74347.23", "close":"74365.35", "volume":"0.01400362"}]}`

func TestProduct(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/market/products/BTC-EUR" {
			t.Errorf("pad = %s", r.URL.Path)
		}
		w.Write([]byte(productBody))
	})

	p, err := c.Product(context.Background(), "BTC-EUR")
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "BTC-EUR" || p.BaseIncrement.String() != "0.00000001" || p.QuoteIncrement.String() != "0.01" ||
		p.QuoteMinSize.String() != "1" || p.BaseMinSize.String() != "0.00000001" {
		t.Errorf("product onjuist: %+v", p)
	}
}

func TestProductUnavailable(t *testing.T) {
	tests := map[string]http.HandlerFunc{
		"404": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":"NOT_FOUND","message":"Product BTC-XYZ not supported"}`))
		},
		"offline": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"product_id":"BTC-EUR","base_increment":"0.00000001","quote_increment":"0.01","status":"offline"}`))
		},
		"handel geblokkeerd": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"product_id":"BTC-EUR","base_increment":"0.00000001","quote_increment":"0.01","status":"online","trading_disabled":true}`))
		},
	}
	for name, h := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := newTestClient(t, h).Product(context.Background(), "BTC-EUR")
			if !errors.Is(err, usecase.ErrProductUnavailable) {
				t.Errorf("verwacht ErrProductUnavailable, kreeg %v", err)
			}
		})
	}
}

func TestCandles(t *testing.T) {
	start := time.Unix(1791895980, 0).UTC()
	end := time.Unix(1791896100, 0).UTC() // de candle op end valt buiten [start, end)

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/market/products/BTC-EUR/candles" || q.Get("granularity") != "ONE_MINUTE" ||
			q.Get("start") != "1791895980" || q.Get("end") != "1791896100" {
			t.Errorf("onverwacht verzoek: %s", r.URL)
		}
		w.Write([]byte(candlesBody))
	})

	candles, err := c.Candles(context.Background(), "BTC-EUR", start, end)
	if err != nil {
		t.Fatal(err)
	}
	if len(candles) != 2 {
		t.Fatalf("candles = %d, verwacht 2", len(candles))
	}
	if !candles[0].Start.Equal(start) || candles[0].Close.String() != "74365.35" {
		t.Errorf("eerste candle onjuist (moet oudste zijn): %+v", candles[0])
	}
	if candles[1].High.String() != "74356.66" || candles[1].Volume.String() != "0.00444068" {
		t.Errorf("tweede candle onjuist: %+v", candles[1])
	}
}

func TestCandlesInvalidStart(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"candles":[{"start":"gisteren","open":"1","high":"1","low":"1","close":"1","volume":"1"}]}`))
	})
	if _, err := c.Candles(context.Background(), "BTC-EUR", time.Unix(0, 0), time.Now()); err == nil {
		t.Error("verwacht fout bij ongeldige start")
	}
}

func TestRetryOnRateLimitAndServerError(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.WriteHeader(http.StatusTooManyRequests)
		case 2:
			w.WriteHeader(http.StatusBadGateway)
		default:
			w.Write([]byte(productBody))
		}
	})
	if _, err := c.Product(context.Background(), "BTC-EUR"); err != nil {
		t.Fatalf("verwacht succes na retries, kreeg %v", err)
	}
	if calls.Load() != 3 {
		t.Errorf("aanroepen = %d, verwacht 3", calls.Load())
	}
}

func TestRetryGivesUp(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	if _, err := c.Product(context.Background(), "BTC-EUR"); err == nil {
		t.Error("verwacht fout")
	}
	if calls.Load() != 3 {
		t.Errorf("aanroepen = %d, verwacht 3 (1 + 2 retries)", calls.Load())
	}
}

func TestNoRetryOnClientError(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"INVALID_ARGUMENT"}`))
	})
	_, err := c.Candles(context.Background(), "BTC-EUR", time.Unix(0, 0), time.Unix(60, 0))
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
		t.Errorf("verwacht APIError 400, kreeg %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("aanroepen = %d, verwacht 1", calls.Load())
	}
}

func TestContextCancelStopsRetry(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	c.backoff = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := c.Product(ctx, "BTC-EUR"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("verwacht DeadlineExceeded, kreeg %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("retry wachtte niet op de context")
	}
}

func TestInvalidJSON(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{kapot`)) })
	if _, err := c.Product(context.Background(), "BTC-EUR"); err == nil {
		t.Error("verwacht fout bij ongeldige JSON")
	}
}

func TestTicker(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/market/products/BTC-EUR/ticker" || r.URL.Query().Get("limit") != "1" {
			t.Errorf("onverwacht verzoek: %s", r.URL)
		}
		// opgenomen antwoord
		w.Write([]byte(`{"trades":[{"trade_id":"110487562", "product_id":"BTC-EUR", "price":"74334.99",
			"size":"0.01201491", "time":"2026-10-13T12:55:48.969092Z", "side":"BUY", "bid":"", "ask":"",
			"exchange":"coinbase"}], "best_bid":"74334.99", "best_ask":"74335"}`))
	})
	tk, err := c.Ticker(context.Background(), "BTC-EUR")
	if err != nil {
		t.Fatal(err)
	}
	if tk.Price.String() != "74334.99" || tk.BestBid.String() != "74334.99" || tk.BestAsk.String() != "74335" ||
		tk.Time.Year() != 2026 {
		t.Errorf("ticker onjuist: %+v", tk)
	}
}

func TestTickerIncomplete(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"trades":[], "best_bid":"", "best_ask":""}`))
	})
	if _, err := c.Ticker(context.Background(), "BTC-EUR"); err == nil {
		t.Error("verwacht fout bij lege ticker")
	}
}
