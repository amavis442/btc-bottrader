package usecase_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"tradingbot/internal/adapter/exchange/coinbase"
	"tradingbot/internal/adapter/storage/sqlite"
	"tradingbot/internal/domain/indicator"
	"tradingbot/internal/domain/strategy"
	"tradingbot/internal/usecase"
)

// fakeCoinbase houdt orders, saldi en fills bij zoals de private API van Coinbase dat doet.
// De test bepaalt zelf wanneer een order gevuld wordt.
type fakeCoinbase struct {
	mu        sync.Mutex
	orders    map[string]map[string]any
	fills     map[string][]map[string]any
	eur, btc  decimal.Decimal
	seq       int
	dropNext  bool // het volgende POST /orders wel uitvoeren, maar met een 502 beantwoorden
	postCount int
}

func newFakeCoinbase() *fakeCoinbase {
	return &fakeCoinbase{orders: map[string]map[string]any{}, fills: map[string][]map[string]any{}, eur: d("100"), btc: d("0")}
}

func (f *fakeCoinbase) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v3/brokerage")
	write := func(v any) { json.NewEncoder(w).Encode(v) }

	switch {
	case r.Method == http.MethodPost && path == "/orders":
		f.postCount++
		var req struct {
			ClientOrderID      string `json:"client_order_id"`
			ProductID          string `json:"product_id"`
			Side               string `json:"side"`
			OrderConfiguration struct {
				LimitGTC map[string]any `json:"limit_limit_gtc"`
			} `json:"order_configuration"`
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &req)
		for _, o := range f.orders {
			if o["client_order_id"] == req.ClientOrderID {
				write(map[string]any{"success": false, "error_response": map[string]any{
					"error": "UNKNOWN_FAILURE_REASON", "new_order_failure_reason": "DUPLICATE_CLIENT_ORDER_ID"}})
				return
			}
		}
		f.seq++
		id := fmt.Sprintf("cb-%d", f.seq)
		f.orders[id] = map[string]any{"order_id": id, "client_order_id": req.ClientOrderID, "product_id": req.ProductID,
			"side": req.Side, "status": "OPEN", "order_configuration": map[string]any{"limit_limit_gtc": req.OrderConfiguration.LimitGTC},
			"created_time": time.Now().UTC().Format(time.RFC3339), "filled_size": "0", "average_filled_price": "0", "total_fees": "0"}
		if f.dropNext {
			f.dropNext = false
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		write(map[string]any{"success": true, "success_response": map[string]any{"order_id": id, "client_order_id": req.ClientOrderID}})

	case r.Method == http.MethodGet && path == "/orders/historical/batch":
		var list []any
		for _, o := range f.orders {
			list = append(list, o)
		}
		write(map[string]any{"orders": list, "has_next": false})

	case r.Method == http.MethodGet && path == "/orders/historical/fills":
		write(map[string]any{"fills": f.fills[r.URL.Query().Get("order_ids")], "cursor": ""})

	case r.Method == http.MethodGet && strings.HasPrefix(path, "/orders/historical/"):
		o, ok := f.orders[strings.TrimPrefix(path, "/orders/historical/")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		write(map[string]any{"order": o})

	case r.Method == http.MethodPost && path == "/orders/batch_cancel":
		var req struct {
			OrderIDs []string `json:"order_ids"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		o := f.orders[req.OrderIDs[0]]
		if o["status"] == "FILLED" {
			write(map[string]any{"results": []any{map[string]any{"success": false, "failure_reason": "ORDER_IS_FULLY_FILLED", "order_id": o["order_id"]}}})
			return
		}
		o["status"] = "CANCELLED"
		o["last_update_time"] = time.Now().UTC().Format(time.RFC3339)
		write(map[string]any{"results": []any{map[string]any{"success": true, "order_id": o["order_id"]}}})

	case r.Method == http.MethodGet && path == "/accounts":
		write(map[string]any{"has_next": false, "accounts": []any{
			map[string]any{"currency": "EUR", "active": true, "type": "ACCOUNT_TYPE_FIAT",
				"available_balance": map[string]any{"value": f.eur.String()}, "hold": map[string]any{"value": "0"}},
			map[string]any{"currency": "BTC", "active": true, "type": "ACCOUNT_TYPE_CRYPTO",
				"available_balance": map[string]any{"value": f.btc.String()}, "hold": map[string]any{"value": "0"}},
		}})

	case r.Method == http.MethodGet && path == "/transaction_summary":
		write(map[string]any{"fee_tier": map[string]any{"pricing_tier": "<$10k", "maker_fee_rate": "0.0040", "taker_fee_rate": "0.0060"}})

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// fill vult een order volledig tegen zijn limietprijs (maker, 0,4%) en past de saldi aan.
func (f *fakeCoinbase) fill(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.orders[id]
	cfg := o["order_configuration"].(map[string]any)["limit_limit_gtc"].(map[string]any)
	size, price := d(cfg["base_size"].(string)), d(cfg["limit_price"].(string))
	value := size.Mul(price)
	fee := value.Mul(d("0.004"))
	if o["side"] == "BUY" {
		f.eur, f.btc = f.eur.Sub(value).Sub(fee), f.btc.Add(size)
	} else {
		f.eur, f.btc = f.eur.Add(value).Sub(fee), f.btc.Sub(size)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	o["status"], o["filled_size"], o["average_filled_price"], o["total_fees"] = "FILLED", size.String(), price.String(), fee.String()
	o["last_fill_time"] = now
	f.fills[id] = append(f.fills[id], map[string]any{"entry_id": id + "-f", "trade_id": id + "-t", "order_id": id,
		"trade_time": now, "trade_type": "FILL", "price": price.String(), "size": size.String(), "commission": fee.String(),
		"liquidity_indicator": "MAKER", "size_in_quote": false})
}

// openOrders geeft de open orders per kant.
func (f *fakeCoinbase) openOrders(side string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for _, o := range f.orders {
		if o["status"] == "OPEN" && o["side"] == side {
			out = append(out, o)
		}
	}
	return out
}

func TestLiveTradingAgainstCoinbaseAPI(t *testing.T) {
	cb := newFakeCoinbase()
	srv := httptest.NewServer(cb)
	defer srv.Close()

	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalECPrivateKey(key)
	signer, err := coinbase.NewSigner("organizations/o/apiKeys/k", string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})))
	if err != nil {
		t.Fatal(err)
	}
	client := coinbase.New(coinbase.WithBaseURL(srv.URL+"/api/v3/brokerage"), coinbase.WithSigner(signer), coinbase.WithRetry(2, time.Millisecond))
	exchange := coinbase.NewExchange(client, product)

	db, _ := sqlite.Open(":memory:")
	defer db.Close()
	p := newPath(t0, "60000").flat(60).move("0.1", 30).flat(300)
	c := &clock{now: t0.Add(5 * time.Second)}
	f := &feed{path: p, clock: c}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	machine, _ := strategy.NewMachine(strategy.Config{
		Trend: indicator.TrendConfig{ShortPeriod: 2, LongPeriod: 4, FlatThreshold: d("0.005")},
		TakeProfits: [2]strategy.TakeProfit{
			{NetProfit: d("0.06"), SellFraction: d("0.5")},
			{NetProfit: d("0.12"), SellFraction: d("0.65")},
		},
		TrailingDistance: d("0.05"), CooldownReversal: 3, CooldownRising: 1,
	}, product)
	market, _ := usecase.NewMarketDataService(f, db, c, log, product.ID, 10*time.Minute, 4)
	fees := usecase.NewCachedFees(client, time.Hour, c)
	notes := &recorder{}
	trader, err := usecase.NewTrader(context.Background(), usecase.TraderConfig{
		Budget: d("50"), BuyOrderTimeout: time.Hour, StopOrderTimeout: time.Minute, FeeRefresh: time.Hour,
	}, usecase.TraderDeps{Product: product, Machine: machine, Market: market, Tickers: f, Exchange: exchange,
		Fees: fees, Store: db, KV: db, Notifier: notes, Clock: c, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	tick := func() {
		t.Helper()
		if err := trader.Tick(context.Background()); err != nil {
			t.Fatalf("ronde: %v", err)
		}
		c.set(c.Now().Add(time.Minute))
	}

	// 1. koopsignaal → kooporder op Coinbase
	for range 120 {
		tick()
		if len(cb.openOrders("BUY")) > 0 {
			break
		}
	}
	buys := cb.openOrders("BUY")
	if len(buys) != 1 {
		t.Fatalf("verwacht één kooporder, kreeg %d", len(buys))
	}
	if st := trader.State(); !st.Fees.Maker.Equal(d("0.004")) || !st.Fees.Taker.Equal(d("0.006")) {
		t.Errorf("fees niet van Coinbase: %+v", st.Fees)
	}

	// 2. Coinbase vult de koop; het antwoord op de winstorder gaat verloren (502), maar de order bestaat
	cb.fill(buys[0]["order_id"].(string))
	cb.mu.Lock()
	cb.dropNext = true
	cb.mu.Unlock()
	tick()

	st := trader.State()
	if st.Strategy.Phase != strategy.PhaseInPosition || st.SellOrder == nil || st.SellOrder.OrderID == "" {
		t.Fatalf("na de koop verwacht IN_POSITIE met winstorder: %s %+v", st.Strategy.Phase, st.SellOrder)
	}
	sells := cb.openOrders("SELL")
	if len(sells) != 1 || sells[0]["order_id"] != st.SellOrder.OrderID {
		t.Fatalf("verwacht precies één verkooporder op Coinbase (ondanks de 502), kreeg %d", len(sells))
	}
	// doelprijs met de echte maker-fee van 0,4%
	want := product.RoundPriceUp(st.Strategy.Position.CostPrice.Mul(d("1.06")).Div(d("0.996")))
	if !st.SellOrder.Price.Equal(want) {
		t.Errorf("doelprijs %s, verwacht %s", st.SellOrder.Price, want)
	}

	// 3. winststap 1 gevuld → winststap 2 geplaatst
	cb.fill(st.SellOrder.OrderID)
	tick()
	st = trader.State()
	if st.Strategy.Phase != strategy.PhasePart1Sold || st.SellOrder == nil || st.SellOrder.Purpose != usecase.PurposeTakeProfit2 {
		t.Fatalf("verwacht DEEL1_VERKOCHT met winstorder 2: %s %+v", st.Strategy.Phase, st.SellOrder)
	}
	if n := len(cb.openOrders("SELL")); n != 1 {
		t.Errorf("open verkooporders op Coinbase: %d", n)
	}

	// 4. winststap 2 gevuld. De vullingen in deze test zijn kunstmatig: de koers zelf ligt ver onder de
	// +12%, dus de trailing stop wordt meteen geraakt en de bot verkoopt het restant op de bid.
	cb.fill(st.SellOrder.OrderID)
	tick()
	st = trader.State()
	if st.Strategy.Phase != strategy.PhaseTrailing || st.SellOrder == nil || st.SellOrder.Purpose != usecase.PurposeStop ||
		!st.SellOrder.Size.Equal(st.Strategy.Position.Quantity) {
		t.Fatalf("verwacht stopverkoop van het restant: %s %+v", st.Strategy.Phase, st.SellOrder)
	}

	// 5. stopverkoop gevuld → positie afgerond, cooldown, geen BTC meer
	cb.fill(st.SellOrder.OrderID)
	tick()
	st = trader.State()
	if st.Strategy.Phase != strategy.PhaseCooldown || st.SellOrder != nil || st.PositionID != 0 {
		t.Fatalf("verwacht COOLDOWN na de stopverkoop: %s %+v", st.Strategy.Phase, st.SellOrder)
	}
	cb.mu.Lock()
	btc, posts := cb.btc, cb.postCount
	cb.mu.Unlock()
	if !btc.IsZero() {
		t.Errorf("nog %s BTC op Coinbase", btc)
	}
	// koop + winst 1 (twee pogingen door de 502) + winst 2 + stop
	if posts != 5 {
		t.Errorf("aantal orderverzoeken = %d, verwacht 5", posts)
	}
	if notes.count("Positie afgerond") != 1 {
		t.Error("verwacht melding over de afgeronde positie")
	}
}

func TestCachedFees(t *testing.T) {
	src := &countingFees{fees: strategy.Fees{Maker: d("0.004"), Taker: d("0.006")}}
	c := &clock{now: t0}
	cached := usecase.NewCachedFees(src, time.Hour, c)
	ctx := context.Background()

	for range 3 {
		if f, err := cached.Fees(ctx); err != nil || !f.Maker.Equal(d("0.004")) {
			t.Fatalf("fees = %+v, %v", f, err)
		}
	}
	if src.calls != 1 {
		t.Errorf("bron %d keer aangeroepen binnen de TTL, verwacht 1", src.calls)
	}

	// na de TTL: opnieuw ophalen; mislukt dat, dan blijven de vorige tarieven gelden
	c.set(t0.Add(2 * time.Hour))
	src.err = fmt.Errorf("Coinbase onbereikbaar")
	if f, err := cached.Fees(ctx); err != nil || !f.Taker.Equal(d("0.006")) {
		t.Errorf("verwacht vorige tarieven bij een fout: %+v, %v", f, err)
	}

	// zonder eerder resultaat wordt de fout doorgegeven
	if _, err := usecase.NewCachedFees(src, time.Hour, c).Fees(ctx); err == nil {
		t.Error("verwacht fout zonder eerder resultaat")
	}
}

type countingFees struct {
	fees  strategy.Fees
	err   error
	calls int
}

func (c *countingFees) Fees(context.Context) (strategy.Fees, error) {
	c.calls++
	return c.fees, c.err
}
