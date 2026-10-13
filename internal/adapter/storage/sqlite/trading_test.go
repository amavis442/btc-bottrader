package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"tradingbot/internal/domain"
	"tradingbot/internal/usecase"
)

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func TestKeyValue(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if _, ok, err := db.GetValue(ctx, "x"); ok || err != nil {
		t.Fatalf("lege store: ok = %v, fout = %v", ok, err)
	}
	for _, v := range []string{`{"a":1}`, `{"a":2}`} {
		if err := db.SetValue(ctx, "x", []byte(v)); err != nil {
			t.Fatal(err)
		}
	}
	got, ok, err := db.GetValue(ctx, "x")
	if err != nil || !ok || string(got) != `{"a":2}` {
		t.Errorf("waarde = %s, ok = %v, fout = %v", got, ok, err)
	}
}

func testOrder(id string, side domain.Side, status domain.OrderStatus) usecase.OrderRecord {
	return usecase.OrderRecord{
		Order: domain.Order{ID: id, ClientOrderID: "c-" + id, ProductID: "BTC-EUR", Side: side,
			Price: dec("60000"), Size: dec("0.001"), FilledSize: dec("0"), FilledValue: dec("0"), Fee: dec("0"),
			Status: status, CreatedAt: t0},
		Purpose: usecase.PurposeBuy,
	}
}

func TestOrdersPositionsAndFills(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// kooporder zonder positie, daarna gevuld en aan een positie gekoppeld
	buy := testOrder("koop", domain.SideBuy, domain.OrderOpen)
	if err := db.SaveOrder(ctx, buy); err != nil {
		t.Fatal(err)
	}
	buy.Status, buy.FilledSize, buy.FilledValue, buy.Fee = domain.OrderFilled, dec("0.001"), dec("60"), dec("0.15")
	buy.DoneAt = t0.Add(time.Minute)
	if err := db.SaveOrder(ctx, buy); err != nil {
		t.Fatal(err)
	}
	fill := domain.Fill{TradeID: "t1", OrderID: "koop", Price: dec("60000"), Size: dec("0.001"), Fee: dec("0.15"),
		Liquidity: domain.LiquidityMaker, Time: t0.Add(time.Minute)}
	for range 2 { // dubbel opslaan mag geen dubbele fill geven
		if err := db.SaveFills(ctx, []domain.Fill{fill}); err != nil {
			t.Fatal(err)
		}
	}

	id, err := db.OpenPosition(ctx, usecase.PositionRecord{ProductID: "BTC-EUR", OpenedAt: t0,
		EntryQuantity: dec("0.001"), CostPrice: dec("60150")})
	if err != nil || id == 0 {
		t.Fatalf("positie openen: id = %d, fout = %v", id, err)
	}
	if err := db.AssignOrderToPosition(ctx, "koop", id); err != nil {
		t.Fatal(err)
	}
	// een latere update zonder positie-ID mag de koppeling niet wissen
	if err := db.SaveOrder(ctx, buy); err != nil {
		t.Fatal(err)
	}

	sell := testOrder("w1", domain.SideSell, domain.OrderOpen)
	sell.PositionID, sell.Purpose = id, usecase.PurposeTakeProfit1
	if err := db.SaveOrder(ctx, sell); err != nil {
		t.Fatal(err)
	}

	orders, err := db.PositionOrders(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(orders) != 2 {
		t.Fatalf("orders van positie = %d, verwacht 2", len(orders))
	}
	got := orders[0]
	if got.ID != "koop" || got.Status != domain.OrderFilled || !got.FilledValue.Equal(dec("60")) ||
		!got.DoneAt.Equal(t0.Add(time.Minute)) || got.PositionID != id || got.Side != domain.SideBuy {
		t.Errorf("kooporder onjuist: %+v", got)
	}
	if orders[1].Purpose != usecase.PurposeTakeProfit1 || !orders[1].DoneAt.IsZero() {
		t.Errorf("winstorder onjuist: %+v", orders[1])
	}

	fills, err := db.OrderFills(ctx, []string{"koop", "w1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(fills) != 1 || !fills[0].Fee.Equal(dec("0.15")) || fills[0].Liquidity != domain.LiquidityMaker {
		t.Errorf("fills = %+v", fills)
	}
	if none, err := db.OrderFills(ctx, nil); err != nil || none != nil {
		t.Errorf("geen orders: %v, %v", none, err)
	}

	if err := db.ClosePosition(ctx, id, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := db.AddEvent(ctx, t0, "TEST", "bericht"); err != nil {
		t.Fatal(err)
	}
}

func TestFillRequiresKnownOrder(t *testing.T) {
	db := openTestDB(t)
	err := db.SaveFills(context.Background(), []domain.Fill{{TradeID: "t", OrderID: "onbekend",
		Price: dec("1"), Size: dec("1"), Fee: dec("0"), Liquidity: domain.LiquidityMaker, Time: t0}})
	if err == nil {
		t.Error("verwacht fout bij fill van een onbekende order")
	}
}
