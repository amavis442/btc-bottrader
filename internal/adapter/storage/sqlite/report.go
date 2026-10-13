package sqlite

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"tradingbot/internal/domain"
	"tradingbot/internal/usecase"
)

var _ usecase.ReportStore = (*DB)(nil)

const maxListLimit = 1000

// limit begrenst een lijst: 0 geeft het maximum, een negatieve waarde betekent alles (LIMIT -1).
func limit(n int) int {
	switch {
	case n < 0:
		return -1
	case n == 0 || n > maxListLimit:
		return maxListLimit
	}
	return n
}

// where bouwt een WHERE-clausule uit losse voorwaarden.
type where struct {
	conds []string
	args  []any
}

func (w *where) add(cond string, arg any) {
	w.conds = append(w.conds, cond)
	w.args = append(w.args, arg)
}

func (w *where) sql() string {
	if len(w.conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(w.conds, " AND ")
}

const orderColumns = `id, client_order_id, COALESCE(position_id, 0), purpose, product_id, side, price, size,
	filled_size, filled_value, fee, status, created_at, done_at`

// ListOrders geeft orders, nieuwste eerst.
func (d *DB) ListOrders(ctx context.Context, f usecase.OrderFilter) ([]usecase.OrderRecord, error) {
	var w where
	if f.Status != "" {
		w.add("status = ?", string(f.Status))
	}
	if !f.From.IsZero() {
		w.add("created_at >= ?", f.From.Unix())
	}
	if !f.To.IsZero() {
		w.add("created_at < ?", f.To.Unix())
	}
	rows, err := d.db.QueryContext(ctx, `SELECT `+orderColumns+` FROM orders`+w.sql()+
		` ORDER BY created_at DESC, id DESC LIMIT ?`, append(w.args, limit(f.Limit))...)
	if err != nil {
		return nil, err
	}
	return scanOrders(rows)
}

// PositionOrders geeft de orders van een positie, oudste eerst.
func (d *DB) PositionOrders(ctx context.Context, positionID int64) ([]usecase.OrderRecord, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT `+orderColumns+` FROM orders
		WHERE position_id = ? ORDER BY created_at, id`, positionID)
	if err != nil {
		return nil, err
	}
	return scanOrders(rows)
}

func scanOrders(rows *sql.Rows) ([]usecase.OrderRecord, error) {
	defer rows.Close()
	var orders []usecase.OrderRecord
	for rows.Next() {
		var o usecase.OrderRecord
		var purpose, side, status, price, size, filledSize, filledValue, fee string
		var created int64
		var done sql.NullInt64
		if err := rows.Scan(&o.ID, &o.ClientOrderID, &o.PositionID, &purpose, &o.ProductID, &side, &price, &size,
			&filledSize, &filledValue, &fee, &status, &created, &done); err != nil {
			return nil, err
		}
		if err := parseDecimals(map[*decimal.Decimal]string{&o.Price: price, &o.Size: size,
			&o.FilledSize: filledSize, &o.FilledValue: filledValue, &o.Fee: fee}); err != nil {
			return nil, err
		}
		o.Purpose = usecase.OrderPurpose(purpose)
		o.Side = domain.Side(side)
		o.Status = domain.OrderStatus(status)
		o.CreatedAt = time.Unix(created, 0).UTC()
		if done.Valid {
			o.DoneAt = time.Unix(done.Int64, 0).UTC()
		}
		orders = append(orders, o)
	}
	return orders, rows.Err()
}

// ListTrades geeft fills met order- en positiegegevens, nieuwste eerst.
func (d *DB) ListTrades(ctx context.Context, f usecase.TradeFilter) ([]usecase.TradeRecord, error) {
	var w where
	if !f.From.IsZero() {
		w.add("f.time >= ?", f.From.Unix())
	}
	if !f.To.IsZero() {
		w.add("f.time < ?", f.To.Unix())
	}
	rows, err := d.db.QueryContext(ctx, `
		SELECT f.trade_id, f.order_id, f.price, f.size, f.fee, f.liquidity, f.time,
			COALESCE(o.position_id, 0), o.purpose, o.side
		FROM fills f JOIN orders o ON o.id = f.order_id`+w.sql()+`
		ORDER BY f.time DESC, f.trade_id DESC LIMIT ?`, append(w.args, limit(f.Limit))...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var trades []usecase.TradeRecord
	for rows.Next() {
		var t usecase.TradeRecord
		var price, size, fee, liquidity, purpose, side string
		var at int64
		if err := rows.Scan(&t.TradeID, &t.OrderID, &price, &size, &fee, &liquidity, &at,
			&t.PositionID, &purpose, &side); err != nil {
			return nil, err
		}
		if err := parseDecimals(map[*decimal.Decimal]string{&t.Price: price, &t.Size: size, &t.Fee: fee}); err != nil {
			return nil, err
		}
		t.Liquidity = domain.Liquidity(liquidity)
		t.Time = time.Unix(at, 0).UTC()
		t.Purpose = usecase.OrderPurpose(purpose)
		t.Side = domain.Side(side)
		trades = append(trades, t)
	}
	return trades, rows.Err()
}

const positionColumns = `id, product_id, opened_at, closed_at, entry_quantity, cost_price`

// ListPositions geeft posities, nieuwste eerst.
func (d *DB) ListPositions(ctx context.Context, f usecase.PositionFilter) ([]usecase.PositionRecord, error) {
	var w where
	if f.Closed {
		w.conds = append(w.conds, "closed_at IS NOT NULL")
		if !f.ClosedFrom.IsZero() {
			w.add("closed_at >= ?", f.ClosedFrom.Unix())
		}
	}
	rows, err := d.db.QueryContext(ctx, `SELECT `+positionColumns+` FROM positions`+w.sql()+
		` ORDER BY opened_at DESC, id DESC LIMIT ?`, append(w.args, limit(f.Limit))...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var positions []usecase.PositionRecord
	for rows.Next() {
		p, err := scanPosition(rows)
		if err != nil {
			return nil, err
		}
		positions = append(positions, p)
	}
	return positions, rows.Err()
}

// GetPosition geeft één positie.
func (d *DB) GetPosition(ctx context.Context, id int64) (usecase.PositionRecord, bool, error) {
	p, err := scanPosition(d.db.QueryRowContext(ctx, `SELECT `+positionColumns+` FROM positions WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return usecase.PositionRecord{}, false, nil
	}
	if err != nil {
		return usecase.PositionRecord{}, false, err
	}
	return p, true, nil
}

type scanner interface{ Scan(dest ...any) error }

func scanPosition(row scanner) (usecase.PositionRecord, error) {
	var p usecase.PositionRecord
	var opened int64
	var closed sql.NullInt64
	var qty, cost string
	if err := row.Scan(&p.ID, &p.ProductID, &opened, &closed, &qty, &cost); err != nil {
		return p, err
	}
	if err := parseDecimals(map[*decimal.Decimal]string{&p.EntryQuantity: qty, &p.CostPrice: cost}); err != nil {
		return p, err
	}
	p.OpenedAt = time.Unix(opened, 0).UTC()
	if closed.Valid {
		p.ClosedAt = time.Unix(closed.Int64, 0).UTC()
	}
	return p, nil
}
