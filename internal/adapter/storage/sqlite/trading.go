package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"tradingbot/internal/domain"
	"tradingbot/internal/usecase"
)

var (
	_ usecase.KeyValueStore = (*DB)(nil)
	_ usecase.TradingStore  = (*DB)(nil)
)

// GetValue leest een document uit de key-value store.
func (d *DB) GetValue(ctx context.Context, key string) ([]byte, bool, error) {
	var value string
	err := d.db.QueryRowContext(ctx, `SELECT value FROM kv_store WHERE key = ?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return []byte(value), true, nil
}

// SetValue schrijft een document naar de key-value store.
func (d *DB) SetValue(ctx context.Context, key string, value []byte) error {
	_, err := d.db.ExecContext(ctx, `
		INSERT INTO kv_store (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, string(value), time.Now().Unix())
	return err
}

// SaveOrder slaat een order op of werkt hem bij. Een bestaande koppeling aan een positie blijft behouden.
func (d *DB) SaveOrder(ctx context.Context, o usecase.OrderRecord) error {
	_, err := d.db.ExecContext(ctx, `
		INSERT INTO orders (id, client_order_id, position_id, purpose, product_id, side, price, size,
			filled_size, filled_value, fee, status, created_at, done_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			position_id = COALESCE(excluded.position_id, orders.position_id),
			filled_size = excluded.filled_size, filled_value = excluded.filled_value, fee = excluded.fee,
			status = excluded.status, done_at = excluded.done_at`,
		o.ID, o.ClientOrderID, nullInt(o.PositionID), string(o.Purpose), o.ProductID, string(o.Side),
		o.Price.String(), o.Size.String(), o.FilledSize.String(), o.FilledValue.String(), o.Fee.String(),
		string(o.Status), o.CreatedAt.Unix(), nullTime(o.DoneAt))
	if err != nil {
		return fmt.Errorf("sqlite: order %s opslaan: %w", o.ID, err)
	}
	return nil
}

// SaveFills slaat fills op; bestaande fills worden overgeslagen.
func (d *DB) SaveFills(ctx context.Context, fills []domain.Fill) error {
	if len(fills) == 0 {
		return nil
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, f := range fills {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO fills (trade_id, order_id, price, size, fee, liquidity, time)
			VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT (trade_id) DO NOTHING`,
			f.TradeID, f.OrderID, f.Price.String(), f.Size.String(), f.Fee.String(), string(f.Liquidity), f.Time.Unix())
		if err != nil {
			return fmt.Errorf("sqlite: fill %s opslaan: %w", f.TradeID, err)
		}
	}
	return tx.Commit()
}

// OrderFills geeft de fills van de opgegeven orders, oudste eerst.
func (d *DB) OrderFills(ctx context.Context, orderIDs []string) ([]domain.Fill, error) {
	if len(orderIDs) == 0 {
		return nil, nil
	}
	args := make([]any, len(orderIDs))
	for i, id := range orderIDs {
		args[i] = id
	}
	rows, err := d.db.QueryContext(ctx, `
		SELECT trade_id, order_id, price, size, fee, liquidity, time FROM fills
		WHERE order_id IN (?`+strings.Repeat(", ?", len(orderIDs)-1)+`) ORDER BY time, trade_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var fills []domain.Fill
	for rows.Next() {
		var f domain.Fill
		var price, size, fee, liquidity string
		var at int64
		if err := rows.Scan(&f.TradeID, &f.OrderID, &price, &size, &fee, &liquidity, &at); err != nil {
			return nil, err
		}
		if err := parseDecimals(map[*decimal.Decimal]string{&f.Price: price, &f.Size: size, &f.Fee: fee}); err != nil {
			return nil, err
		}
		f.Liquidity = domain.Liquidity(liquidity)
		f.Time = time.Unix(at, 0).UTC()
		fills = append(fills, f)
	}
	return fills, rows.Err()
}

// OpenPosition legt een nieuwe positie vast en geeft het ID terug.
func (d *DB) OpenPosition(ctx context.Context, p usecase.PositionRecord) (int64, error) {
	res, err := d.db.ExecContext(ctx, `
		INSERT INTO positions (product_id, opened_at, entry_quantity, cost_price) VALUES (?, ?, ?, ?)`,
		p.ProductID, p.OpenedAt.Unix(), p.EntryQuantity.String(), p.CostPrice.String())
	if err != nil {
		return 0, fmt.Errorf("sqlite: positie openen: %w", err)
	}
	return res.LastInsertId()
}

// AssignOrderToPosition koppelt een order aan een positie.
func (d *DB) AssignOrderToPosition(ctx context.Context, orderID string, positionID int64) error {
	_, err := d.db.ExecContext(ctx, `UPDATE orders SET position_id = ? WHERE id = ?`, positionID, orderID)
	return err
}

// ClosePosition markeert een positie als afgerond.
func (d *DB) ClosePosition(ctx context.Context, positionID int64, closedAt time.Time) error {
	_, err := d.db.ExecContext(ctx, `UPDATE positions SET closed_at = ? WHERE id = ?`, closedAt.Unix(), positionID)
	return err
}

// AddEvent voegt een regel toe aan de audit log.
func (d *DB) AddEvent(ctx context.Context, at time.Time, kind, message string) error {
	_, err := d.db.ExecContext(ctx, `INSERT INTO events (at, kind, message) VALUES (?, ?, ?)`, at.Unix(), kind, message)
	return err
}

func nullInt(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: v != 0} }

func nullTime(t time.Time) sql.NullInt64 {
	return sql.NullInt64{Int64: t.Unix(), Valid: !t.IsZero()}
}

func parseDecimals(fields map[*decimal.Decimal]string) error {
	for dst, src := range fields {
		v, err := decimal.NewFromString(src)
		if err != nil {
			return fmt.Errorf("sqlite: ongeldige waarde %q: %w", src, err)
		}
		*dst = v
	}
	return nil
}
