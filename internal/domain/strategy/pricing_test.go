package strategy

import (
	"testing"

	"github.com/shopspring/decimal"

	"tradingbot/internal/domain"
)

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func btcEur() domain.Product {
	return domain.Product{
		ID:             "BTC-EUR",
		BaseIncrement:  d("0.00000001"),
		QuoteIncrement: d("0.01"),
		BaseMinSize:    d("0.00000001"),
		QuoteMinSize:   d("1"),
	}
}

var testFees = Fees{Maker: d("0.0025"), Taker: d("0.005")}

func assertDecimal(t *testing.T, name string, got decimal.Decimal, want string) {
	t.Helper()
	if !got.Equal(d(want)) {
		t.Errorf("%s = %s, verwacht %s", name, got, want)
	}
}

func TestCostPrice(t *testing.T) {
	got, err := CostPrice([]Fill{
		{Price: d("60000"), Size: d("0.0005"), Fee: d("0.075")},
		{Price: d("60010"), Size: d("0.0005"), Fee: d("0.075")},
	})
	if err != nil {
		t.Fatal(err)
	}
	// (30 + 30,005 + 0,15) / 0,001
	assertDecimal(t, "kostprijs", got, "60155")
}

func TestCostPriceWithoutFills(t *testing.T) {
	if _, err := CostPrice(nil); err == nil {
		t.Error("verwacht fout zonder fills")
	}
}

func TestTargetPrice(t *testing.T) {
	tests := []struct {
		name      string
		netProfit string
		want      string
	}{
		// voorbeeld uit het plan: 60.000 × 1,06 / 0,9975 = 63.759,398… → naar boven afgerond
		{"+6%", "0.06", "63759.40"},
		// 60.000 × 1,12 / 0,9975 = 67.368,421…
		{"+12%", "0.12", "67368.43"},
	}
	for _, tt := range tests {
		got := TargetPrice(d("60000"), d(tt.netProfit), testFees.Maker, btcEur())
		assertDecimal(t, tt.name, got, tt.want)
	}
}

func TestTargetPriceGivesAtLeastNetProfit(t *testing.T) {
	cost, profit := d("61234.567"), d("0.06")
	price := TargetPrice(cost, profit, testFees.Maker, btcEur())
	net := price.Mul(one.Sub(testFees.Maker))
	if net.LessThan(cost.Mul(one.Add(profit))) {
		t.Errorf("netto opbrengst %s haalt doel %s niet", net, cost.Mul(one.Add(profit)))
	}
}

func TestBuySize(t *testing.T) {
	// 50 / (60.000 × 1,005) = 0,000829187… → naar beneden afgerond
	got := BuySize(d("50"), d("60000"), testFees, btcEur())
	assertDecimal(t, "grootte", got, "0.00082918")

	total := got.Mul(d("60000")).Mul(one.Add(testFees.Taker))
	if total.GreaterThan(d("50")) {
		t.Errorf("totaal incl. fee %s overschrijdt budget", total)
	}
}

func TestBuySizeBelowMinimum(t *testing.T) {
	tests := []struct{ name, budget, price string }{
		{"budget onder minimale orderwaarde", "0.5", "60000"},
		{"geen budget", "0", "60000"},
		{"geen prijs", "50", "0"},
	}
	for _, tt := range tests {
		if got := BuySize(d(tt.budget), d(tt.price), testFees, btcEur()); !got.IsZero() {
			t.Errorf("%s: verwacht 0, kreeg %s", tt.name, got)
		}
	}
}

func TestSellQuantity(t *testing.T) {
	tests := []struct {
		name      string
		remaining string
		fraction  string
		want      string
	}{
		{"50% van de positie", "0.001", "0.5", "0.0005"},
		{"65% van het restant", "0.0005", "0.65", "0.000325"},
		{"naar beneden afgerond", "0.00100003", "0.5", "0.00050001"},
		// 50% = 0,00001 BTC ≈ 0,64 EUR < 1 EUR minimum → alles verkopen
		{"deel te klein: alles verkopen", "0.00002", "0.5", "0.00002"},
		// 90% verkopen laat 0,00001 BTC over → restant te klein → alles verkopen
		{"restant te klein: alles verkopen", "0.0001", "0.9", "0.0001"},
	}
	for _, tt := range tests {
		got := SellQuantity(d(tt.remaining), d(tt.fraction), d("63759.40"), btcEur())
		assertDecimal(t, tt.name, got, tt.want)
	}
}

func TestFeesValidate(t *testing.T) {
	if err := testFees.Validate(); err != nil {
		t.Errorf("geldige fees afgekeurd: %v", err)
	}
	for _, f := range []Fees{{Maker: d("-0.01")}, {Taker: d("1")}} {
		if err := f.Validate(); err == nil {
			t.Errorf("ongeldige fees %+v goedgekeurd", f)
		}
	}
}
