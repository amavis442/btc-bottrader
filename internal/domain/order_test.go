package domain

import "testing"

func TestOrderAveragePrice(t *testing.T) {
	o := Order{FilledSize: d("0.002"), FilledValue: d("121")}
	if got := o.AveragePrice(); !got.Equal(d("60500")) {
		t.Errorf("gemiddelde prijs = %s", got)
	}
	if got := (Order{}).AveragePrice(); !got.IsZero() {
		t.Errorf("lege order: %s", got)
	}
}

func TestOrderStatusDone(t *testing.T) {
	for status, want := range map[OrderStatus]bool{OrderOpen: false, OrderFilled: true, OrderCancelled: true} {
		if status.Done() != want {
			t.Errorf("%s.Done() = %v", status, !want)
		}
	}
}

func TestBalanceTotal(t *testing.T) {
	if got := (Balance{Available: d("1.5"), Hold: d("0.5")}).Total(); !got.Equal(d("2")) {
		t.Errorf("totaal = %s", got)
	}
}

func TestCurrencies(t *testing.T) {
	base, quote := Currencies("BTC-EUR")
	if base != "BTC" || quote != "EUR" {
		t.Errorf("Currencies = %s, %s", base, quote)
	}
}
