package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/shopspring/decimal"

	"tradingbot/internal/domain"
	"tradingbot/internal/usecase"
)

var _ usecase.CandleRepository = (*DB)(nil)

// SaveCandles slaat candles op in één transactie; bestaande candles worden overschreven.
func (d *DB) SaveCandles(ctx context.Context, productID string, candles []domain.Candle) error {
	if len(candles) == 0 {
		return nil
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO candles_1m (product_id, start, open, high, low, close, volume)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (product_id, start) DO UPDATE SET
			open = excluded.open, high = excluded.high, low = excluded.low,
			close = excluded.close, volume = excluded.volume`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, c := range candles {
		_, err := stmt.ExecContext(ctx, productID, c.Start.Unix(),
			c.Open.String(), c.High.String(), c.Low.String(), c.Close.String(), c.Volume.String())
		if err != nil {
			return fmt.Errorf("sqlite: candle %s opslaan: %w", c.Start, err)
		}
	}
	return tx.Commit()
}

// Candles geeft de candles met een start in [from, to), oudste eerst.
func (d *DB) Candles(ctx context.Context, productID string, from, to time.Time) ([]domain.Candle, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT start, open, high, low, close, volume FROM candles_1m
		WHERE product_id = ? AND start >= ? AND start < ?
		ORDER BY start`, productID, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var candles []domain.Candle
	for rows.Next() {
		var start int64
		var open, high, low, closePrice, volume string
		if err := rows.Scan(&start, &open, &high, &low, &closePrice, &volume); err != nil {
			return nil, err
		}
		c := domain.Candle{Start: time.Unix(start, 0).UTC()}
		for _, f := range []struct {
			dst *decimal.Decimal
			src string
		}{{&c.Open, open}, {&c.High, high}, {&c.Low, low}, {&c.Close, closePrice}, {&c.Volume, volume}} {
			if *f.dst, err = decimal.NewFromString(f.src); err != nil {
				return nil, fmt.Errorf("sqlite: ongeldige waarde %q in candle %d: %w", f.src, start, err)
			}
		}
		candles = append(candles, c)
	}
	return candles, rows.Err()
}

// LatestCandleStart geeft de start van de nieuwste candle van het product.
func (d *DB) LatestCandleStart(ctx context.Context, productID string) (time.Time, bool, error) {
	var start sql.NullInt64
	err := d.db.QueryRowContext(ctx, `SELECT MAX(start) FROM candles_1m WHERE product_id = ?`, productID).Scan(&start)
	if err != nil || !start.Valid {
		return time.Time{}, false, err
	}
	return time.Unix(start.Int64, 0).UTC(), true, nil
}
