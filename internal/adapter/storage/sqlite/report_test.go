package sqlite

import (
	"context"
	"testing"
	"time"

	"tradingbot/internal/domain"
	"tradingbot/internal/usecase"
)

// seed maakt twee posities: één afgerond (met koop en verkoop) en één open.
func seed(t *testing.T, db *DB) (closedID, openID int64) {
	t.Helper()
	ctx := context.Background()
	var err error
	closedID, err = db.OpenPosition(ctx, usecase.PositionRecord{ProductID: "BTC-EUR", OpenedAt: t0, EntryQuantity: dec("0.001"), CostPrice: dec("60150")})
	if err != nil {
		t.Fatal(err)
	}
	openID, _ = db.OpenPosition(ctx, usecase.PositionRecord{ProductID: "BTC-EUR", OpenedAt: t0.Add(2 * time.Hour), EntryQuantity: dec("0.002"), CostPrice: dec("59000")})
	db.ClosePosition(ctx, closedID, t0.Add(time.Hour))

	orders := []usecase.OrderRecord{
		testOrder("koop", domain.SideBuy, domain.OrderFilled),
		testOrder("verkoop", domain.SideSell, domain.OrderFilled),
		testOrder("open", domain.SideSell, domain.OrderOpen),
	}
	orders[0].PositionID, orders[1].PositionID, orders[2].PositionID = closedID, closedID, openID
	orders[1].CreatedAt, orders[1].Purpose = t0.Add(30*time.Minute), usecase.PurposeStop
	orders[2].CreatedAt, orders[2].Purpose = t0.Add(3*time.Hour), usecase.PurposeTakeProfit1
	for _, o := range orders {
		if err := db.SaveOrder(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	db.SaveFills(ctx, []domain.Fill{
		{TradeID: "t1", OrderID: "koop", Price: dec("60000"), Size: dec("0.001"), Fee: dec("0.15"), Liquidity: domain.LiquidityMaker, Time: t0},
		{TradeID: "t2", OrderID: "verkoop", Price: dec("64000"), Size: dec("0.001"), Fee: dec("0.32"), Liquidity: domain.LiquidityTaker, Time: t0.Add(time.Hour)},
	})
	return closedID, openID
}

func TestListOrders(t *testing.T) {
	db := openTestDB(t)
	seed(t, db)
	ctx := context.Background()

	all, err := db.ListOrders(ctx, usecase.OrderFilter{})
	if err != nil || len(all) != 3 || all[0].ID != "open" {
		t.Fatalf("alle orders (nieuwste eerst): %v, %v", ids(all), err)
	}
	tests := []struct {
		name string
		f    usecase.OrderFilter
		want int
	}{
		{"status", usecase.OrderFilter{Status: domain.OrderOpen}, 1},
		{"vanaf", usecase.OrderFilter{From: t0.Add(time.Minute)}, 2},
		{"tot", usecase.OrderFilter{To: t0.Add(time.Minute)}, 1},
		{"limiet", usecase.OrderFilter{Limit: 2}, 2},
		{"alles", usecase.OrderFilter{Limit: -1}, 3},
	}
	for _, tt := range tests {
		got, err := db.ListOrders(ctx, tt.f)
		if err != nil || len(got) != tt.want {
			t.Errorf("%s: %d orders, verwacht %d (%v)", tt.name, len(got), tt.want, err)
		}
	}
}

func ids(orders []usecase.OrderRecord) []string {
	out := make([]string, len(orders))
	for i, o := range orders {
		out[i] = o.ID
	}
	return out
}

func TestListTrades(t *testing.T) {
	db := openTestDB(t)
	closedID, _ := seed(t, db)
	ctx := context.Background()

	trades, err := db.ListTrades(ctx, usecase.TradeFilter{})
	if err != nil || len(trades) != 2 {
		t.Fatalf("trades: %d, %v", len(trades), err)
	}
	first := trades[0] // nieuwste eerst
	if first.TradeID != "t2" || first.Side != domain.SideSell || first.Purpose != usecase.PurposeStop ||
		first.PositionID != closedID || first.Liquidity != domain.LiquidityTaker || !first.Price.Equal(dec("64000")) {
		t.Errorf("trade = %+v", first)
	}
	if got, _ := db.ListTrades(ctx, usecase.TradeFilter{From: t0.Add(time.Minute)}); len(got) != 1 {
		t.Errorf("vanaf: %d", len(got))
	}
	if got, _ := db.ListTrades(ctx, usecase.TradeFilter{To: t0.Add(time.Minute)}); len(got) != 1 {
		t.Errorf("tot: %d", len(got))
	}
}

func TestListAndGetPositions(t *testing.T) {
	db := openTestDB(t)
	closedID, openID := seed(t, db)
	ctx := context.Background()

	all, err := db.ListPositions(ctx, usecase.PositionFilter{})
	if err != nil || len(all) != 2 || all[0].ID != openID {
		t.Fatalf("alle posities: %+v, %v", all, err)
	}
	closed, _ := db.ListPositions(ctx, usecase.PositionFilter{Closed: true})
	if len(closed) != 1 || closed[0].ID != closedID || !closed[0].ClosedAt.Equal(t0.Add(time.Hour)) {
		t.Errorf("afgerond: %+v", closed)
	}
	if later, _ := db.ListPositions(ctx, usecase.PositionFilter{Closed: true, ClosedFrom: t0.Add(2 * time.Hour)}); len(later) != 0 {
		t.Errorf("afgerond na 2 uur: %d", len(later))
	}

	p, ok, err := db.GetPosition(ctx, openID)
	if err != nil || !ok || !p.CostPrice.Equal(dec("59000")) || !p.ClosedAt.IsZero() {
		t.Errorf("GetPosition: %+v, %v, %v", p, ok, err)
	}
	if _, ok, err := db.GetPosition(ctx, 999); ok || err != nil {
		t.Errorf("onbekende positie: ok = %v, fout = %v", ok, err)
	}
}
