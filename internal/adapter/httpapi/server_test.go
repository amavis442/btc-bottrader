package httpapi

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"tradingbot/internal/domain"
	"tradingbot/internal/domain/strategy"
	"tradingbot/internal/usecase"
)

const token = "test-token-1234567890"

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

var now = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

type fakeQueries struct {
	orderFilter usecase.OrderFilter
	tradeFilter usecase.TradeFilter
	candleArgs  []any
	period      usecase.Period
	open        *usecase.PositionView
	fail        error
}

func (f *fakeQueries) Status(context.Context) usecase.StatusView {
	return usecase.StatusView{Mode: "paper", Phase: strategy.PhaseSeekingEntry, LastPrice: d("74000.12")}
}

func (f *fakeQueries) OpenPosition(context.Context) (*usecase.PositionView, error) {
	return f.open, f.fail
}

func (f *fakeQueries) Position(_ context.Context, id int64) (*usecase.PositionView, bool, error) {
	if id == 7 {
		return &usecase.PositionView{ID: 7, EntryQuantity: d("0.001")}, true, nil
	}
	return nil, false, nil
}

func (f *fakeQueries) PositionHistory(_ context.Context, limit int) ([]usecase.PositionView, error) {
	return []usecase.PositionView{{ID: 1}}, f.fail
}

func (f *fakeQueries) AllPositions(context.Context) ([]usecase.PositionView, error) {
	closed := now
	return []usecase.PositionView{{ID: 1, OpenedAt: now.Add(-time.Hour), ClosedAt: &closed, EntryQuantity: d("0.001"),
		CostPrice: d("60150"), Invested: d("60.15"), Fees: d("0.4"),
		Realized: usecase.Amount{EUR: d("4.56"), Pct: d("7.58")}, Total: usecase.Amount{EUR: d("4.56"), Pct: d("7.58")}}}, nil
}

func (f *fakeQueries) Orders(_ context.Context, filter usecase.OrderFilter) ([]usecase.OrderRecord, error) {
	f.orderFilter = filter
	return []usecase.OrderRecord{{Order: domain.Order{ID: "o1", Side: domain.SideBuy, Price: d("60000"), Size: d("0.001"),
		FilledSize: d("0.001"), FilledValue: d("60"), Fee: d("0.15"), Status: domain.OrderFilled, CreatedAt: now, DoneAt: now},
		PositionID: 1, Purpose: usecase.PurposeBuy}}, nil
}

func (f *fakeQueries) Trades(_ context.Context, filter usecase.TradeFilter) ([]usecase.TradeRecord, error) {
	f.tradeFilter = filter
	return []usecase.TradeRecord{
		{Fill: domain.Fill{TradeID: "t2", OrderID: "o2", Price: d("63759.40"), Size: d("0.0005"), Fee: d("0.08"),
			Liquidity: domain.LiquidityMaker, Time: now}, PositionID: 1, Purpose: usecase.PurposeTakeProfit1, Side: domain.SideSell},
		{Fill: domain.Fill{TradeID: "t1", OrderID: "o1", Price: d("60000"), Size: d("0.001"), Fee: d("0.15"),
			Liquidity: domain.LiquidityMaker, Time: now.Add(-time.Hour)}, PositionID: 1, Purpose: usecase.PurposeBuy, Side: domain.SideBuy},
	}, nil
}

func (f *fakeQueries) Candles(_ context.Context, interval time.Duration, from, to time.Time) ([]domain.Candle, error) {
	f.candleArgs = []any{interval, from, to}
	if interval == 90*time.Second {
		return nil, errors.Join(usecase.ErrInvalidArgument, errors.New("interval"))
	}
	return []domain.Candle{{Start: from, Open: d("1"), High: d("2"), Low: d("0.5"), Close: d("1.5"), Volume: d("3")}}, nil
}

func (f *fakeQueries) Report(_ context.Context, p usecase.Period) (usecase.ReportView, error) {
	f.period = p
	if p == "jaar" {
		return usecase.ReportView{}, errors.Join(usecase.ErrInvalidArgument, errors.New("periode"))
	}
	return usecase.ReportView{Period: p, ClosedPositions: 3}, nil
}

type fakeControl struct {
	paused  string
	resumed bool
	stopped bool
}

func (c *fakeControl) Pause(_ context.Context, reason string) error { c.paused = reason; return nil }
func (c *fakeControl) Resume(context.Context) error                 { c.resumed = true; return nil }
func (c *fakeControl) Stop()                                        { c.stopped = true }

func newTestServer() (*Server, *fakeQueries, *fakeControl) {
	q, c := &fakeQueries{}, &fakeControl{}
	s := New(Config{Listen: "127.0.0.1:0", Token: token, CORSOrigins: []string{"http://localhost:5173"}}, q, c,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.now = func() time.Time { return now }
	return s, q, c
}

func do(t *testing.T, s *Server, method, target, body string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, r)
	req.Header.Set("Authorization", "Bearer "+token)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("geen JSON: %s", rec.Body)
	}
	return v
}

func TestAuth(t *testing.T) {
	s, _, _ := newTestServer()
	for name, header := range map[string]string{"geen token": "", "fout token": "Bearer verkeerd", "zonder Bearer": token} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s: status %d, verwacht 401", name, rec.Code)
		}
	}
	if rec := do(t, s, http.MethodGet, "/api/v1/status", ""); rec.Code != http.StatusOK {
		t.Errorf("geldig token: status %d", rec.Code)
	}
}

func TestEmptyTokenRejectsEverything(t *testing.T) {
	s, _, _ := newTestServer()
	s.cfg.Token = ""
	req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	req.Header.Set("Authorization", "Bearer ")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("leeg token moet alles weigeren, status %d", rec.Code)
	}
}

func TestCORS(t *testing.T) {
	s, _, _ := newTestServer()

	// preflight zonder token van een toegestane origin
	req := httptest.NewRequest(http.MethodOptions, "/api/v1/status", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || rec.Header().Get("Access-Control-Allow-Origin") != "http://localhost:5173" {
		t.Errorf("preflight: %d, %v", rec.Code, rec.Header())
	}

	// onbekende origin krijgt geen CORS-headers
	rec = do(t, s, http.MethodGet, "/api/v1/status", "", "Origin", "https://kwaadwillend.example")
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("CORS-header voor onbekende origin")
	}
	rec = do(t, s, http.MethodGet, "/api/v1/status", "", "Origin", "http://localhost:5173")
	if rec.Header().Get("Access-Control-Allow-Origin") != "http://localhost:5173" {
		t.Error("geen CORS-header voor toegestane origin")
	}
}

func TestStatus(t *testing.T) {
	s, _, _ := newTestServer()
	v := decode(t, do(t, s, http.MethodGet, "/api/v1/status", ""))
	if v["mode"] != "paper" || v["phase"] != "ZOEK_INSTAP" || v["last_price"] != "74000.12" {
		t.Errorf("status = %v", v)
	}
}

func TestPositions(t *testing.T) {
	s, q, _ := newTestServer()

	if v := decode(t, do(t, s, http.MethodGet, "/api/v1/position", "")); v["position"] != nil {
		t.Errorf("geen open positie verwacht: %v", v)
	}
	q.open = &usecase.PositionView{ID: 3}
	if v := decode(t, do(t, s, http.MethodGet, "/api/v1/position", "")); v["position"] == nil {
		t.Error("open positie ontbreekt")
	}

	if rec := do(t, s, http.MethodGet, "/api/v1/positions/7", ""); rec.Code != http.StatusOK || decode(t, rec)["entry_quantity"] != "0.001" {
		t.Errorf("positie 7: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, s, http.MethodGet, "/api/v1/positions/8", ""); rec.Code != http.StatusNotFound {
		t.Errorf("onbekende positie: %d", rec.Code)
	}
	if rec := do(t, s, http.MethodGet, "/api/v1/positions/abc", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("ongeldig ID: %d", rec.Code)
	}
	if rec := do(t, s, http.MethodGet, "/api/v1/positions/history?limit=5", ""); rec.Code != http.StatusOK {
		t.Errorf("historie: %d", rec.Code)
	}
	if rec := do(t, s, http.MethodGet, "/api/v1/positions/history?limit=5000", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("te grote limiet: %d", rec.Code)
	}
}

func TestOrders(t *testing.T) {
	s, q, _ := newTestServer()
	rec := do(t, s, http.MethodGet, "/api/v1/orders?status=filled&from=2026-10-01&to=2026-10-13T12:00:00Z&limit=10", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	want := usecase.OrderFilter{Status: domain.OrderFilled, From: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), To: now, Limit: 10}
	if q.orderFilter != want {
		t.Errorf("filter = %+v, verwacht %+v", q.orderFilter, want)
	}
	orders := decode(t, rec)["orders"].([]any)
	o := orders[0].(map[string]any)
	if o["average_price"] != "60000" || o["purpose"] != "KOOP" || o["done_at"] == nil {
		t.Errorf("order = %v", o)
	}

	for _, bad := range []string{"?status=WEG", "?from=gisteren", "?limit=0"} {
		if rec := do(t, s, http.MethodGet, "/api/v1/orders"+bad, ""); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, verwacht 400", bad, rec.Code)
		}
	}
}

func TestTrades(t *testing.T) {
	s, q, _ := newTestServer()
	rec := do(t, s, http.MethodGet, "/api/v1/trades?limit=2", "")
	trades := decode(t, rec)["trades"].([]any)
	if len(trades) != 2 || trades[0].(map[string]any)["value"] != "31.8797" || q.tradeFilter.Limit != 2 {
		t.Errorf("trades = %v, filter %+v", trades, q.tradeFilter)
	}
}

func TestCandles(t *testing.T) {
	s, q, _ := newTestServer()
	rec := do(t, s, http.MethodGet, "/api/v1/candles", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	// standaard: 10m over de laatste 24 uur
	if q.candleArgs[0] != 10*time.Minute || q.candleArgs[1] != now.Add(-24*time.Hour) || q.candleArgs[2] != now {
		t.Errorf("standaardwaarden = %v", q.candleArgs)
	}
	v := decode(t, rec)
	if v["interval"] != "10m0s" || len(v["candles"].([]any)) != 1 {
		t.Errorf("antwoord = %v", v)
	}

	if rec := do(t, s, http.MethodGet, "/api/v1/candles?interval=1h&from=2026-10-12", ""); rec.Code != http.StatusOK || q.candleArgs[0] != time.Hour {
		t.Errorf("1h: %d %v", rec.Code, q.candleArgs)
	}
	for _, bad := range []string{"?interval=soms", "?interval=90s", "?to=morgen"} {
		if rec := do(t, s, http.MethodGet, "/api/v1/candles"+bad, ""); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, verwacht 400", bad, rec.Code)
		}
	}
}

func TestReport(t *testing.T) {
	s, q, _ := newTestServer()
	if v := decode(t, do(t, s, http.MethodGet, "/api/v1/report", "")); v["period"] != "all" || q.period != usecase.PeriodAll {
		t.Errorf("standaardperiode: %v", v)
	}
	if rec := do(t, s, http.MethodGet, "/api/v1/report?period=week", ""); rec.Code != http.StatusOK || q.period != usecase.PeriodWeek {
		t.Errorf("week: %d", rec.Code)
	}
	if rec := do(t, s, http.MethodGet, "/api/v1/report?period=jaar", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("onbekende periode: %d", rec.Code)
	}
}

func readCSV(t *testing.T, rec *httptest.ResponseRecorder) [][]string {
	t.Helper()
	rows, err := csv.NewReader(rec.Body).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestExportFills(t *testing.T) {
	s, q, _ := newTestServer()
	rec := do(t, s, http.MethodGet, "/api/v1/report/export?format=csv", "")
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/csv") ||
		!strings.Contains(rec.Header().Get("Content-Disposition"), "tradingbot-fills-20261013.csv") {
		t.Fatalf("export: %d %v", rec.Code, rec.Header())
	}
	rows := readCSV(t, rec)
	if len(rows) != 3 || rows[0][0] != "trade_id" {
		t.Fatalf("rijen = %v", rows)
	}
	// oudste eerst
	if rows[1][0] != "t1" || rows[1][5] != "BUY" || rows[1][8] != "60" || rows[2][0] != "t2" {
		t.Errorf("rijen = %v", rows)
	}
	if q.tradeFilter.Limit != -1 {
		t.Errorf("export moet alle fills ophalen, limiet %d", q.tradeFilter.Limit)
	}
}

func TestExportPositions(t *testing.T) {
	s, _, _ := newTestServer()
	rows := readCSV(t, do(t, s, http.MethodGet, "/api/v1/report/export?type=positions", ""))
	if len(rows) != 2 || rows[0][0] != "positie" || rows[1][0] != "1" || rows[1][7] != "4.56" || rows[1][2] == "" {
		t.Errorf("rijen = %v", rows)
	}
	for _, bad := range []string{"?format=xlsx", "?type=alles"} {
		if rec := do(t, s, http.MethodGet, "/api/v1/report/export"+bad, ""); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d", bad, rec.Code)
		}
	}
}

func TestControl(t *testing.T) {
	s, _, c := newTestServer()

	if rec := do(t, s, http.MethodPost, "/api/v1/control/pause", `{"reason":"onderhoud"}`); rec.Code != http.StatusOK || c.paused != "onderhoud" {
		t.Errorf("pauze: %d, reden %q", rec.Code, c.paused)
	}
	if rec := do(t, s, http.MethodPost, "/api/v1/control/pause", ""); rec.Code != http.StatusOK || c.paused != "gepauzeerd via de API" {
		t.Errorf("pauze zonder reden: %d, reden %q", rec.Code, c.paused)
	}
	if rec := do(t, s, http.MethodPost, "/api/v1/control/pause", `{kapot`); rec.Code != http.StatusBadRequest {
		t.Errorf("ongeldige JSON: %d", rec.Code)
	}
	if rec := do(t, s, http.MethodPost, "/api/v1/control/resume", ""); rec.Code != http.StatusOK || !c.resumed {
		t.Errorf("hervatten: %d", rec.Code)
	}
	if rec := do(t, s, http.MethodPost, "/api/v1/control/stop", ""); rec.Code != http.StatusAccepted || !c.stopped {
		t.Errorf("stoppen: %d", rec.Code)
	}
	if rec := do(t, s, http.MethodGet, "/api/v1/control/stop", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET op stop: %d, verwacht 405", rec.Code)
	}
}

func TestErrors(t *testing.T) {
	s, q, _ := newTestServer()
	if rec := do(t, s, http.MethodGet, "/api/v1/bestaat-niet", ""); rec.Code != http.StatusNotFound {
		t.Errorf("onbekend endpoint: %d", rec.Code)
	}
	q.fail = errors.New("database weg")
	rec := do(t, s, http.MethodGet, "/api/v1/position", "")
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "database") {
		t.Errorf("interne fout lekt details of verkeerde status: %d %s", rec.Code, rec.Body)
	}
}

func TestRunAndShutdown(t *testing.T) {
	s, _, _ := newTestServer()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("afsluiten: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server stopte niet")
	}

	s.cfg.Listen = "ongeldig-adres"
	if err := s.Run(context.Background()); err == nil {
		t.Error("verwacht fout bij ongeldig adres")
	}
}
