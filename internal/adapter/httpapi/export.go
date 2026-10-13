package httpapi

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"tradingbot/internal/usecase"
)

// export levert alle fills (type=fills, standaard) of alle posities (type=positions) als CSV.
// Scheidingsteken is een komma, decimalen met een punt; tijden in UTC (RFC3339).
func (s *Server) export(w http.ResponseWriter, r *http.Request) {
	if f := r.URL.Query().Get("format"); f != "" && f != "csv" {
		s.fail(w, r, badRequest("alleen format=csv wordt ondersteund"))
		return
	}

	var header []string
	var rows [][]string
	kind := r.URL.Query().Get("type")
	switch kind {
	case "", "fills":
		kind = "fills"
		trades, err := s.queries.Trades(r.Context(), usecase.TradeFilter{Limit: -1})
		if err != nil {
			s.fail(w, r, err)
			return
		}
		header = []string{"trade_id", "tijd", "positie", "order_id", "doel", "kant", "prijs", "hoeveelheid", "waarde", "fee", "liquiditeit"}
		for i := len(trades) - 1; i >= 0; i-- { // oudste eerst
			t := trades[i]
			rows = append(rows, []string{t.TradeID, t.Time.Format(time.RFC3339), id(t.PositionID), t.OrderID,
				string(t.Purpose), string(t.Side), t.Price.String(), t.Size.String(), t.Price.Mul(t.Size).String(),
				t.Fee.String(), string(t.Liquidity)})
		}

	case "positions":
		positions, err := s.queries.AllPositions(r.Context())
		if err != nil {
			s.fail(w, r, err)
			return
		}
		header = []string{"positie", "geopend", "gesloten", "hoeveelheid", "kostprijs", "ingelegd", "fees",
			"gerealiseerd_eur", "gerealiseerd_pct", "ongerealiseerd_eur", "totaal_eur", "totaal_pct"}
		for _, p := range positions {
			closed := ""
			if p.ClosedAt != nil {
				closed = p.ClosedAt.Format(time.RFC3339)
			}
			rows = append(rows, []string{id(p.ID), p.OpenedAt.Format(time.RFC3339), closed, p.EntryQuantity.String(),
				p.CostPrice.String(), p.Invested.String(), p.Fees.String(), p.Realized.EUR.String(), p.Realized.Pct.String(),
				p.Unrealized.EUR.String(), p.Total.EUR.String(), p.Total.Pct.String()})
		}

	default:
		s.fail(w, r, badRequest("type moet fills of positions zijn"))
		return
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="tradingbot-%s-%s.csv"`, kind, s.now().UTC().Format("20060102")))
	cw := csv.NewWriter(w)
	_ = cw.Write(header)
	_ = cw.WriteAll(rows)
}

func id(v int64) string {
	if v == 0 {
		return ""
	}
	return strconv.FormatInt(v, 10)
}
