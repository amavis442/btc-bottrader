// Package httpapi is de REST/JSON-API van de bot (plan 4.5). Alle endpoints vragen een bearer-token.
package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"tradingbot/internal/domain"
	"tradingbot/internal/usecase"
)

// Queries levert de gegevens voor de API.
type Queries interface {
	Status(ctx context.Context) usecase.StatusView
	OpenPosition(ctx context.Context) (*usecase.PositionView, error)
	Position(ctx context.Context, id int64) (*usecase.PositionView, bool, error)
	PositionHistory(ctx context.Context, limit int) ([]usecase.PositionView, error)
	AllPositions(ctx context.Context) ([]usecase.PositionView, error)
	Orders(ctx context.Context, f usecase.OrderFilter) ([]usecase.OrderRecord, error)
	Trades(ctx context.Context, f usecase.TradeFilter) ([]usecase.TradeRecord, error)
	Candles(ctx context.Context, interval time.Duration, from, to time.Time) ([]domain.Candle, error)
	Report(ctx context.Context, period usecase.Period) (usecase.ReportView, error)
}

// Control bestuurt de bot.
type Control interface {
	Pause(ctx context.Context, reason string) error
	Resume(ctx context.Context) error
	Stop()
}

// Config bevat de instellingen van de API.
type Config struct {
	Listen      string
	Token       string
	CORSOrigins []string // "*" staat alle origins toe
}

// Server is de HTTP-server van de API.
type Server struct {
	cfg     Config
	queries Queries
	control Control
	log     *slog.Logger
	now     func() time.Time
}

// New maakt de server.
func New(cfg Config, queries Queries, control Control, log *slog.Logger) *Server {
	return &Server{cfg: cfg, queries: queries, control: control, log: log, now: time.Now}
}

// Handler geeft de volledige handler, inclusief CORS, authenticatie en foutafhandeling.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/status", s.status)
	mux.HandleFunc("GET /api/v1/position", s.openPosition)
	mux.HandleFunc("GET /api/v1/positions/history", s.positionHistory)
	mux.HandleFunc("GET /api/v1/positions/{id}", s.position)
	mux.HandleFunc("GET /api/v1/orders", s.orders)
	mux.HandleFunc("GET /api/v1/trades", s.trades)
	mux.HandleFunc("GET /api/v1/candles", s.candles)
	mux.HandleFunc("GET /api/v1/report", s.report)
	mux.HandleFunc("GET /api/v1/report/export", s.export)
	mux.HandleFunc("POST /api/v1/control/pause", s.pause)
	mux.HandleFunc("POST /api/v1/control/resume", s.resume)
	mux.HandleFunc("POST /api/v1/control/stop", s.stop)
	return s.recover(s.cors(s.auth(mux)))
}

// Run start de server en stopt hem netjes zodra ctx afloopt.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.cfg.Listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	s.log.Info("API gestart", "adres", s.cfg.Listen)

	select {
	case err := <-errc:
		return fmt.Errorf("API: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// ---- middleware ----

func (s *Server) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.log.Error("API: paniek", "pad", r.URL.Path, "fout", v)
				writeError(w, http.StatusInternalServerError, "interne fout")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && (slices.Contains(s.cfg.CORSOrigins, origin) || slices.Contains(s.cfg.CORSOrigins, "*")) {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			h.Set("Access-Control-Max-Age", "600")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) auth(next http.Handler) http.Handler {
	expected := []byte("Bearer " + s.cfg.Token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if s.cfg.Token == "" || subtle.ConstantTimeCompare(got, expected) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="tradingbot"`)
			writeError(w, http.StatusUnauthorized, "ongeldig of ontbrekend token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---- hulpfuncties ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// fail vertaalt een fout naar een HTTP-antwoord.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, usecase.ErrInvalidArgument) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.log.Error("API: verzoek mislukt", "pad", r.URL.Path, "fout", err)
	writeError(w, http.StatusInternalServerError, "interne fout")
}

func badRequest(format string, args ...any) error {
	return fmt.Errorf("%w: %s", usecase.ErrInvalidArgument, fmt.Sprintf(format, args...))
}

// parseTime accepteert RFC3339 ("2026-10-13T12:00:00Z") of een datum ("2026-10-13", UTC).
func parseTime(r *http.Request, name string) (time.Time, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse(time.DateOnly, v); err == nil {
		return t, nil
	}
	return time.Time{}, badRequest("%s: gebruik RFC3339 of JJJJ-MM-DD", name)
}

func parseLimit(r *http.Request) (int, error) {
	v := r.URL.Query().Get("limit")
	if v == "" {
		return 100, nil
	}
	var n int
	if _, err := fmt.Sscan(v, &n); err != nil || n <= 0 || n > 1000 {
		return 0, badRequest("limit moet tussen 1 en 1000 liggen")
	}
	return n, nil
}

// ---- endpoints ----

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.queries.Status(r.Context()))
}

func (s *Server) openPosition(w http.ResponseWriter, r *http.Request) {
	p, err := s.queries.OpenPosition(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"position": p})
}

func (s *Server) position(w http.ResponseWriter, r *http.Request) {
	var id int64
	if _, err := fmt.Sscan(r.PathValue("id"), &id); err != nil || id <= 0 {
		s.fail(w, r, badRequest("ongeldig positie-ID"))
		return
	}
	p, ok, err := s.queries.Position(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "positie niet gevonden")
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) positionHistory(w http.ResponseWriter, r *http.Request) {
	limit, err := parseLimit(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	positions, err := s.queries.PositionHistory(r.Context(), limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"positions": positions})
}

func (s *Server) orders(w http.ResponseWriter, r *http.Request) {
	var f usecase.OrderFilter
	var err error
	if f.From, err = parseTime(r, "from"); err == nil {
		if f.To, err = parseTime(r, "to"); err == nil {
			f.Limit, err = parseLimit(r)
		}
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	switch status := domain.OrderStatus(strings.ToUpper(r.URL.Query().Get("status"))); status {
	case "", domain.OrderOpen, domain.OrderFilled, domain.OrderCancelled:
		f.Status = status
	default:
		s.fail(w, r, badRequest("status moet OPEN, FILLED of CANCELLED zijn"))
		return
	}

	orders, err := s.queries.Orders(r.Context(), f)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := make([]orderJSON, len(orders))
	for i, o := range orders {
		out[i] = toOrderJSON(o)
	}
	writeJSON(w, http.StatusOK, map[string]any{"orders": out})
}

func (s *Server) trades(w http.ResponseWriter, r *http.Request) {
	var f usecase.TradeFilter
	var err error
	if f.From, err = parseTime(r, "from"); err == nil {
		if f.To, err = parseTime(r, "to"); err == nil {
			f.Limit, err = parseLimit(r)
		}
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	trades, err := s.queries.Trades(r.Context(), f)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := make([]tradeJSON, len(trades))
	for i, t := range trades {
		out[i] = toTradeJSON(t)
	}
	writeJSON(w, http.StatusOK, map[string]any{"trades": out})
}

func (s *Server) candles(w http.ResponseWriter, r *http.Request) {
	interval := 10 * time.Minute
	if v := r.URL.Query().Get("interval"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			s.fail(w, r, badRequest("interval: gebruik bijv. 1m, 10m of 1h"))
			return
		}
		interval = d
	}
	from, err := parseTime(r, "from")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	to, err := parseTime(r, "to")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if to.IsZero() {
		to = s.now().UTC()
	}
	if from.IsZero() {
		from = to.Add(-24 * time.Hour)
	}

	candles, err := s.queries.Candles(r.Context(), interval, from, to)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := make([]candleJSON, len(candles))
	for i, c := range candles {
		out[i] = candleJSON{Start: c.Start, Open: c.Open, High: c.High, Low: c.Low, Close: c.Close, Volume: c.Volume}
	}
	writeJSON(w, http.StatusOK, map[string]any{"interval": interval.String(), "candles": out})
}

func (s *Server) report(w http.ResponseWriter, r *http.Request) {
	period := usecase.Period(r.URL.Query().Get("period"))
	if period == "" {
		period = usecase.PeriodAll
	}
	report, err := s.queries.Report(r.Context(), period)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

type pauseRequest struct {
	Reason string `json:"reason"`
}

func (s *Server) pause(w http.ResponseWriter, r *http.Request) {
	var req pauseRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			s.fail(w, r, badRequest("ongeldige JSON"))
			return
		}
	}
	if req.Reason == "" {
		req.Reason = "gepauzeerd via de API"
	}
	if err := s.control.Pause(r.Context(), req.Reason); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.queries.Status(r.Context()))
}

func (s *Server) resume(w http.ResponseWriter, r *http.Request) {
	if err := s.control.Resume(r.Context()); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.queries.Status(r.Context()))
}

func (s *Server) stop(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusAccepted, map[string]string{"message": "bot stopt; open posities en orders blijven staan"})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	s.log.Info("stop aangevraagd via de API")
	s.control.Stop()
}
