package coinbase

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"tradingbot/internal/domain"
	"tradingbot/internal/usecase"
)

const keyName = "organizations/org-1/apiKeys/key-1"

func testKey(t *testing.T) (*ecdsa.PrivateKey, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return key, string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
}

// verifyJWT controleert de handtekening en geeft header en claims terug.
func verifyJWT(t *testing.T, token string, pub *ecdsa.PublicKey) (map[string]any, map[string]any) {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT heeft %d delen", len(parts))
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		t.Fatalf("handtekening: %d bytes, %v", len(sig), err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(pub, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("handtekening ongeldig")
	}
	var header, claims map[string]any
	for i, dst := range []*map[string]any{&header, &claims} {
		data, _ := base64.RawURLEncoding.DecodeString(parts[i])
		if err := json.Unmarshal(data, dst); err != nil {
			t.Fatal(err)
		}
	}
	return header, claims
}

func TestSignerToken(t *testing.T) {
	key, pemKey := testKey(t)
	s, err := NewSigner(keyName, pemKey)
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Unix(1_800_000_000, 0) }

	token, err := s.Token("GET", "api.coinbase.com", "/api/v3/brokerage/accounts")
	if err != nil {
		t.Fatal(err)
	}
	header, claims := verifyJWT(t, token, &key.PublicKey)
	if header["alg"] != "ES256" || header["typ"] != "JWT" || header["kid"] != keyName || len(header["nonce"].(string)) != 32 {
		t.Errorf("header = %v", header)
	}
	if claims["iss"] != "cdp" || claims["sub"] != keyName || claims["nbf"] != 1.8e9 || claims["exp"] != 1.8e9+120 ||
		claims["uri"] != "GET api.coinbase.com/api/v3/brokerage/accounts" {
		t.Errorf("claims = %v", claims)
	}

	// elke token heeft een eigen nonce
	other, _ := s.Token("GET", "api.coinbase.com", "/api/v3/brokerage/accounts")
	h2, _ := verifyJWT(t, other, &key.PublicKey)
	if h2["nonce"] == header["nonce"] {
		t.Error("nonce wordt hergebruikt")
	}
}

func TestNewSignerFormats(t *testing.T) {
	key, pemKey := testKey(t)

	// PEM op één regel met \n (zoals in JSON of een omgevingsvariabele)
	if _, err := NewSigner(keyName, strings.ReplaceAll(pemKey, "\n", `\n`)); err != nil {
		t.Errorf("PEM op één regel: %v", err)
	}
	// PKCS#8
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	if _, err := NewSigner(keyName, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))); err != nil {
		t.Errorf("PKCS#8: %v", err)
	}

	_, edKey, _ := ed25519.GenerateKey(rand.Reader)
	edDER, _ := x509.MarshalPKCS8PrivateKey(edKey)
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	p384DER, _ := x509.MarshalECPrivateKey(p384)
	bad := map[string][2]string{
		"verkeerde key name":        {"mijn-key", pemKey},
		"geen PEM (Ed25519 base64)": {keyName, base64.StdEncoding.EncodeToString(edKey)},
		"Ed25519 in PKCS#8":         {keyName, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: edDER}))},
		"P-384":                     {keyName, string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: p384DER}))},
		"onbekend PEM-type":         {keyName, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("x")}))},
		"kapotte key":               {keyName, string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: []byte("x")}))},
	}
	for name, args := range bad {
		if _, err := NewSigner(args[0], args[1]); err == nil {
			t.Errorf("%s: verwacht fout", name)
		}
	}
}

// fakeCoinbase speelt de private endpoints na met antwoorden volgens het schema van Coinbase.
type fakeCoinbase struct {
	t      *testing.T
	pub    *ecdsa.PublicKey
	mu     sync.Mutex
	routes map[string]func(w http.ResponseWriter, r *http.Request, body []byte)
	calls  []string
}

func (f *fakeCoinbase) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	route := r.Method + " " + r.URL.Path
	f.calls = append(f.calls, route)

	// elk privé-verzoek moet een geldig JWT hebben met de juiste uri (zonder query-string)
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	_, claims := verifyJWT(f.t, strings.TrimPrefix(auth, "Bearer "), f.pub)
	if want := r.Method + " " + r.Host + r.URL.Path; claims["uri"] != want {
		f.t.Errorf("uri-claim %q, verwacht %q", claims["uri"], want)
	}

	h, ok := f.routes[route]
	if !ok {
		f.t.Errorf("onverwacht verzoek %s", route)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	body, _ := io.ReadAll(r.Body)
	h(w, r, body)
}

func newLive(t *testing.T) (*Exchange, *fakeCoinbase) {
	t.Helper()
	key, pemKey := testKey(t)
	fake := &fakeCoinbase{t: t, pub: &key.PublicKey, routes: map[string]func(http.ResponseWriter, *http.Request, []byte){}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	signer, err := NewSigner(keyName, pemKey)
	if err != nil {
		t.Fatal(err)
	}
	client := New(WithBaseURL(srv.URL+"/api/v3/brokerage"), WithSigner(signer), WithRetry(2, time.Millisecond))
	product := domain.Product{ID: "BTC-EUR", BaseIncrement: dec("0.00000001"), QuoteIncrement: dec("0.01"),
		BaseMinSize: dec("0.00000001"), QuoteMinSize: dec("1")}
	ex := NewExchange(client, product)
	ex.now = func() time.Time { return time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC) }
	return ex, fake
}

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func respond(body string) func(http.ResponseWriter, *http.Request, []byte) {
	return func(w http.ResponseWriter, _ *http.Request, _ []byte) { w.Write([]byte(body)) }
}

// Een order zoals GET /orders/historical/{id} hem teruggeeft (velden volgens het schema).
const openOrderJSON = `{"order": {"order_id": "11111-00000-000000", "client_order_id": "tb-abc-1-koop",
	"product_id": "BTC-EUR", "user_id": "u", "side": "BUY", "status": "OPEN", "time_in_force": "GOOD_UNTIL_CANCELLED",
	"order_configuration": {"limit_limit_gtc": {"base_size": "0.00066", "limit_price": "74500.00", "post_only": false}},
	"created_time": "2026-10-13T12:00:01.123Z", "last_update_time": "2026-10-13T12:00:01.123Z",
	"filled_size": "0", "filled_value": "0", "average_filled_price": "0", "completion_percentage": "0",
	"number_of_fills": "0", "total_fees": "0", "total_value_after_fees": "0", "outstanding_hold_amount": "49.4415",
	"settled": false, "pending_cancel": false, "order_type": "LIMIT", "reject_reason": "", "cancel_message": ""}}`

const filledOrderJSON = `{"order": {"order_id": "11111-00000-000000", "client_order_id": "tb-abc-1-koop",
	"product_id": "BTC-EUR", "side": "BUY", "status": "FILLED",
	"order_configuration": {"limit_limit_gtc": {"base_size": "0.00066", "limit_price": "74500.00", "post_only": false}},
	"created_time": "2026-10-13T12:00:01Z", "last_fill_time": "2026-10-13T12:03:30Z", "last_update_time": "2026-10-13T12:03:31Z",
	"filled_size": "0.00066", "filled_value": "49.17", "average_filled_price": "74500", "total_fees": "0.196680",
	"total_value_after_fees": "49.36668", "settled": true}}`

func TestPlaceLimitOrder(t *testing.T) {
	ex, fake := newLive(t)
	var sent map[string]any
	fake.routes["POST /api/v3/brokerage/orders"] = func(w http.ResponseWriter, r *http.Request, body []byte) {
		json.Unmarshal(body, &sent)
		w.Write([]byte(`{"success": true, "success_response": {"order_id": "11111-00000-000000",
			"product_id": "BTC-EUR", "side": "BUY", "client_order_id": "tb-abc-1-koop"},
			"order_configuration": {"limit_limit_gtc": {"base_size": "0.00066", "limit_price": "74500.00"}}}`))
	}
	fake.routes["GET /api/v3/brokerage/orders/historical/11111-00000-000000"] = respond(openOrderJSON)

	o, err := ex.PlaceLimitOrder(context.Background(), usecase.OrderRequest{ClientOrderID: "tb-abc-1-koop",
		ProductID: "BTC-EUR", Side: domain.SideBuy, Size: dec("0.00066"), Price: dec("74500.00")})
	if err != nil {
		t.Fatal(err)
	}
	cfg := sent["order_configuration"].(map[string]any)["limit_limit_gtc"].(map[string]any)
	if sent["client_order_id"] != "tb-abc-1-koop" || sent["side"] != "BUY" || sent["product_id"] != "BTC-EUR" ||
		cfg["base_size"] != "0.00066" || cfg["limit_price"] != "74500" || cfg["post_only"] != false {
		t.Errorf("verzoek = %v", sent)
	}
	if o.ID != "11111-00000-000000" || o.Status != domain.OrderOpen || !o.Price.Equal(dec("74500")) ||
		!o.Size.Equal(dec("0.00066")) || !o.FilledSize.IsZero() || o.CreatedAt.IsZero() {
		t.Errorf("order = %+v", o)
	}
}

func TestPlaceLimitOrderDuplicateFindsExisting(t *testing.T) {
	ex, fake := newLive(t)
	fake.routes["POST /api/v3/brokerage/orders"] = respond(`{"success": false, "error_response": {
		"error": "UNKNOWN_FAILURE_REASON", "message": "duplicate client order id",
		"new_order_failure_reason": "DUPLICATE_CLIENT_ORDER_ID"}}`)
	pages := 0
	fake.routes["GET /api/v3/brokerage/orders/historical/batch"] = func(w http.ResponseWriter, r *http.Request, _ []byte) {
		q := r.URL.Query()
		if q.Get("product_ids") != "BTC-EUR" || q.Get("start_date") != "2026-10-06T12:00:00Z" {
			t.Errorf("query = %v", q)
		}
		pages++
		if q.Get("cursor") == "" {
			w.Write([]byte(`{"orders": [{"order_id": "x", "client_order_id": "ander", "side": "SELL", "status": "FILLED",
				"order_configuration": {"limit_limit_gtc": {"base_size": "1", "limit_price": "1"}}}],
				"has_next": true, "cursor": "pagina-2"}`))
			return
		}
		var o map[string]any
		json.Unmarshal([]byte(filledOrderJSON), &o)
		data, _ := json.Marshal(map[string]any{"orders": []any{o["order"]}, "has_next": false})
		w.Write(data)
	}

	o, err := ex.PlaceLimitOrder(context.Background(), usecase.OrderRequest{ClientOrderID: "tb-abc-1-koop",
		ProductID: "BTC-EUR", Side: domain.SideBuy, Size: dec("0.00066"), Price: dec("74500")})
	if err != nil {
		t.Fatal(err)
	}
	if o.ID != "11111-00000-000000" || o.Status != domain.OrderFilled || pages != 2 {
		t.Errorf("order = %+v, pagina's = %d", o, pages)
	}
}

func TestPlaceLimitOrderFailures(t *testing.T) {
	tests := map[string]struct {
		body     string
		wantFund bool
	}{
		"onvoldoende saldo": {`{"success": false, "error_response": {"error": "INSUFFICIENT_FUND",
			"message": "Insufficient balance in source account", "new_order_failure_reason": "INSUFFICIENT_FUND"}}`, true},
		"ongeldige prijs": {`{"success": false, "error_response": {"error": "INVALID_LIMIT_PRICE_PRECISION",
			"message": "Invalid limit price precision", "new_order_failure_reason": "INVALID_LIMIT_PRICE_PRECISION"}}`, false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			ex, fake := newLive(t)
			fake.routes["POST /api/v3/brokerage/orders"] = respond(tt.body)
			_, err := ex.PlaceLimitOrder(context.Background(), usecase.OrderRequest{ClientOrderID: "c", ProductID: "BTC-EUR",
				Side: domain.SideBuy, Size: dec("1"), Price: dec("1")})
			if err == nil || errors.Is(err, usecase.ErrInsufficientFunds) != tt.wantFund {
				t.Errorf("fout = %v", err)
			}
		})
	}

	ex, _ := newLive(t)
	if _, err := ex.PlaceLimitOrder(context.Background(), usecase.OrderRequest{ProductID: "ETH-EUR"}); err == nil {
		t.Error("verwacht fout bij ander product")
	}
}

func TestOrderStatusMapping(t *testing.T) {
	tests := map[string]domain.OrderStatus{
		"OPEN": domain.OrderOpen, "PENDING": domain.OrderOpen, "QUEUED": domain.OrderOpen,
		"CANCEL_QUEUED": domain.OrderOpen, "EDIT_QUEUED": domain.OrderOpen, "UNKNOWN_ORDER_STATUS": domain.OrderOpen,
		"FILLED": domain.OrderFilled, "CANCELLED": domain.OrderCancelled, "EXPIRED": domain.OrderCancelled,
		"FAILED": domain.OrderCancelled,
	}
	for status, want := range tests {
		ex, fake := newLive(t)
		fake.routes["GET /api/v3/brokerage/orders/historical/o1"] = respond(strings.Replace(
			strings.Replace(openOrderJSON, `"status": "OPEN"`, `"status": "`+status+`"`, 1), "11111-00000-000000", "o1", 1))
		o, err := ex.Order(context.Background(), "o1")
		if err != nil || o.Status != want {
			t.Errorf("%s → %s, verwacht %s (%v)", status, o.Status, want, err)
		}
		if want == domain.OrderCancelled && o.DoneAt.IsZero() {
			t.Errorf("%s: geen eindtijd", status)
		}
	}
}

func TestOrderFilled(t *testing.T) {
	ex, fake := newLive(t)
	fake.routes["GET /api/v3/brokerage/orders/historical/11111-00000-000000"] = respond(filledOrderJSON)
	o, err := ex.Order(context.Background(), "11111-00000-000000")
	if err != nil {
		t.Fatal(err)
	}
	if !o.FilledSize.Equal(dec("0.00066")) || !o.FilledValue.Equal(dec("49.17")) || !o.Fee.Equal(dec("0.19668")) ||
		!o.DoneAt.Equal(time.Date(2026, 10, 13, 12, 3, 30, 0, time.UTC)) || !o.AveragePrice().Equal(dec("74500")) {
		t.Errorf("order = %+v", o)
	}
}

func TestOrderNotFoundAndWrongType(t *testing.T) {
	ex, fake := newLive(t)
	fake.routes["GET /api/v3/brokerage/orders/historical/weg"] = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error": "NOT_FOUND", "message": "order not found"}`))
	}
	if _, err := ex.Order(context.Background(), "weg"); !errors.Is(err, ErrOrderNotFound) {
		t.Errorf("verwacht ErrOrderNotFound, kreeg %v", err)
	}
	fake.routes["GET /api/v3/brokerage/orders/historical/markt"] = respond(`{"order": {"order_id": "markt", "status": "FILLED",
		"order_configuration": {"market_market_ioc": {"quote_size": "10"}}}}`)
	if _, err := ex.Order(context.Background(), "markt"); err == nil {
		t.Error("verwacht fout bij een market-order")
	}
}

func TestCancelOrder(t *testing.T) {
	tests := map[string]struct {
		result  string
		wantErr bool
	}{
		"geannuleerd":     {`{"success": true, "failure_reason": "UNKNOWN_CANCEL_FAILURE_REASON", "order_id": "o1"}`, false},
		"al gevuld":       {`{"success": false, "failure_reason": "ORDER_IS_FULLY_FILLED", "order_id": "o1"}`, false},
		"al aangevraagd":  {`{"success": false, "failure_reason": "DUPLICATE_CANCEL_REQUEST", "order_id": "o1"}`, false},
		"onbekend":        {`{"success": false, "failure_reason": "UNKNOWN_CANCEL_ORDER", "order_id": "o1"}`, true},
		"niet toegestaan": {`{"success": false, "failure_reason": "NOT_ALLOWED_TO_CANCEL", "order_id": "o1"}`, true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			ex, fake := newLive(t)
			fake.routes["POST /api/v3/brokerage/orders/batch_cancel"] = func(w http.ResponseWriter, _ *http.Request, body []byte) {
				if string(body) != `{"order_ids":["o1"]}` {
					t.Errorf("verzoek = %s", body)
				}
				w.Write([]byte(`{"results": [` + tt.result + `]}`))
			}
			if err := ex.CancelOrder(context.Background(), "o1"); (err != nil) != tt.wantErr {
				t.Errorf("fout = %v", err)
			}
		})
	}
}

func TestFills(t *testing.T) {
	ex, fake := newLive(t)
	fake.routes["GET /api/v3/brokerage/orders/historical/fills"] = func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if r.URL.Query().Get("order_ids") != "o1" {
			t.Errorf("query = %v", r.URL.Query())
		}
		if r.URL.Query().Get("cursor") == "" {
			w.Write([]byte(`{"fills": [
				{"entry_id": "e1", "trade_id": "t1", "order_id": "o1", "trade_time": "2026-10-13T12:01:00Z", "trade_type": "FILL",
				 "price": "74500", "size": "0.0004", "commission": "0.1192", "product_id": "BTC-EUR",
				 "liquidity_indicator": "MAKER", "size_in_quote": false, "side": "BUY"},
				{"entry_id": "e0", "trade_id": "t0", "order_id": "o1", "trade_time": "2026-10-13T12:00:30Z", "trade_type": "CORRECTION",
				 "price": "1", "size": "1", "commission": "0", "liquidity_indicator": "MAKER", "size_in_quote": false}
			], "cursor": "volgende"}`))
			return
		}
		w.Write([]byte(`{"fills": [{"entry_id": "e2", "trade_id": "t2", "order_id": "o1", "trade_time": "2026-10-13T12:02:00Z",
			"trade_type": "FILL", "price": "74500", "size": "0.00026", "commission": "0.07748",
			"liquidity_indicator": "TAKER", "size_in_quote": false}], "cursor": ""}`))
	}
	fills, err := ex.Fills(context.Background(), "o1")
	if err != nil {
		t.Fatal(err)
	}
	if len(fills) != 2 || fills[0].TradeID != "e1" || fills[0].Liquidity != domain.LiquidityMaker ||
		!fills[0].Fee.Equal(dec("0.1192")) || fills[1].Liquidity != domain.LiquidityTaker || !fills[1].Size.Equal(dec("0.00026")) {
		t.Errorf("fills = %+v", fills)
	}
}

func TestFillsInQuoteRejected(t *testing.T) {
	ex, fake := newLive(t)
	fake.routes["GET /api/v3/brokerage/orders/historical/fills"] = respond(`{"fills": [{"entry_id": "e", "order_id": "o1",
		"trade_type": "FILL", "price": "1", "size": "50", "commission": "0", "size_in_quote": true}]}`)
	if _, err := ex.Fills(context.Background(), "o1"); err == nil {
		t.Error("verwacht fout bij fill in quote-valuta")
	}
}

func TestBalances(t *testing.T) {
	ex, fake := newLive(t)
	fake.routes["GET /api/v3/brokerage/accounts"] = func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if r.URL.Query().Get("cursor") == "" {
			w.Write([]byte(`{"accounts": [
				{"uuid": "1", "name": "EUR Wallet", "currency": "EUR", "available_balance": {"value": "120.50", "currency": "EUR"},
				 "hold": {"value": "49.44", "currency": "EUR"}, "active": true, "type": "ACCOUNT_TYPE_FIAT", "ready": true},
				{"uuid": "2", "name": "BTC Vault", "currency": "BTC", "available_balance": {"value": "5", "currency": "BTC"},
				 "hold": {"value": "0", "currency": "BTC"}, "active": true, "type": "ACCOUNT_TYPE_VAULT"}
			], "has_next": true, "cursor": "p2", "size": 2}`))
			return
		}
		w.Write([]byte(`{"accounts": [
			{"uuid": "3", "name": "BTC Wallet", "currency": "BTC", "available_balance": {"value": "0.0012", "currency": "BTC"},
			 "hold": {"value": "0.0003", "currency": "BTC"}, "active": true, "type": "ACCOUNT_TYPE_CRYPTO"}
		], "has_next": false, "size": 1}`))
	}
	b, err := ex.Balances(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !b["EUR"].Available.Equal(dec("120.5")) || !b["EUR"].Hold.Equal(dec("49.44")) {
		t.Errorf("EUR = %+v", b["EUR"])
	}
	// de kluis telt niet mee
	if !b["BTC"].Available.Equal(dec("0.0012")) || !b["BTC"].Hold.Equal(dec("0.0003")) {
		t.Errorf("BTC = %+v", b["BTC"])
	}
}

func TestFees(t *testing.T) {
	ex, fake := newLive(t)
	fake.routes["GET /api/v3/brokerage/transaction_summary"] = func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if r.URL.Query().Get("product_type") != "SPOT" {
			t.Errorf("query = %v", r.URL.Query())
		}
		w.Write([]byte(`{"total_volume": 0, "total_fees": 0, "fee_tier": {"pricing_tier": "<$10k", "usd_from": "0",
			"usd_to": "10000", "taker_fee_rate": "0.0060", "maker_fee_rate": "0.0040", "aop_from": "", "aop_to": ""},
			"advanced_trade_only_volume": 0, "advanced_trade_only_fees": 0}`))
	}
	fees, err := ex.client.Fees(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !fees.Maker.Equal(dec("0.004")) || !fees.Taker.Equal(dec("0.006")) {
		t.Errorf("fees = %+v", fees)
	}

	fake.routes["GET /api/v3/brokerage/transaction_summary"] = respond(`{"fee_tier": {"taker_fee_rate": "0.60", "maker_fee_rate": "0.40"}}`)
	if _, err := ex.client.Fees(context.Background()); err == nil {
		t.Error("verwacht fout bij een tarief dat een percentage lijkt")
	}
}

func TestPrivateRequestsNeedSigner(t *testing.T) {
	c := New(WithBaseURL("http://127.0.0.1:1"))
	if _, err := c.Fees(context.Background()); err == nil || !strings.Contains(err.Error(), "geen API-key") {
		t.Errorf("verwacht fout zonder API-key, kreeg %v", err)
	}
}

func TestUnauthorizedIsNotRetried(t *testing.T) {
	ex, fake := newLive(t)
	fake.routes["GET /api/v3/brokerage/accounts"] = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error": "unauthorized"}`))
	}
	_, err := ex.Balances(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnauthorized || len(fake.calls) != 1 {
		t.Errorf("fout = %v, aanroepen = %d", err, len(fake.calls))
	}
}

func TestQueryIsNotPartOfURIClaim(t *testing.T) {
	// fakeCoinbase controleert de uri-claim bij elk verzoek; dit verzoek heeft een query-string
	ex, fake := newLive(t)
	fake.routes["GET /api/v3/brokerage/accounts"] = respond(`{"accounts": [], "has_next": false}`)
	if _, err := ex.Balances(context.Background()); err != nil { // Balances stuurt ?limit=250 mee
		t.Fatal(err)
	}
}
