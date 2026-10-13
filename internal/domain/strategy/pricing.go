// Package strategy bevat de handelsstrategie: prijsberekeningen en de state machine.
package strategy

import (
	"errors"

	"github.com/shopspring/decimal"

	"tradingbot/internal/domain"
)

var one = decimal.NewFromInt(1)

// Fees bevat de fee-tarieven als fractie (0.0025 = 0,25%).
type Fees struct {
	Maker decimal.Decimal
	Taker decimal.Decimal
}

// Validate controleert of de tarieven tussen 0 en 1 liggen.
func (f Fees) Validate() error {
	for _, fee := range []decimal.Decimal{f.Maker, f.Taker} {
		if fee.IsNegative() || !fee.LessThan(one) {
			return errors.New("fees: tarief moet tussen 0 en 1 liggen")
		}
	}
	return nil
}

// Fill is een (deel)uitvoering van een order.
type Fill struct {
	Price decimal.Decimal // prijs per BTC
	Size  decimal.Decimal // hoeveelheid BTC
	Fee   decimal.Decimal // betaalde fee in EUR
}

// CostPrice berekent de kostprijs per BTC van een aankoop, inclusief de betaalde fees.
func CostPrice(fills []Fill) (decimal.Decimal, error) {
	paid, size := decimal.Zero, decimal.Zero
	for _, f := range fills {
		paid = paid.Add(f.Price.Mul(f.Size)).Add(f.Fee)
		size = size.Add(f.Size)
	}
	if !size.IsPositive() {
		return decimal.Zero, errors.New("strategy: geen gevulde hoeveelheid")
	}
	return paid.Div(size), nil
}

// TargetPrice berekent de verkoopprijs waarbij de netto winst (na verkoopfee) gelijk is aan netProfit,
// gerekend vanaf de kostprijs. De prijs wordt naar boven afgerond zodat het doel altijd gehaald wordt.
//
//	P_doel = kostprijs × (1 + netProfit) / (1 − sellFee)
func TargetPrice(costPrice, netProfit, sellFee decimal.Decimal, product domain.Product) decimal.Decimal {
	price := costPrice.Mul(one.Add(netProfit)).Div(one.Sub(sellFee))
	return product.RoundPriceUp(price)
}

// BuySize berekent hoeveel BTC er gekocht kan worden voor budget (inclusief fee) tegen limitPrice.
// Er wordt met het taker-tarief gerekend, zodat het budget ook niet overschreden wordt als de order direct matcht.
// Geeft nul terug als de order niet aan de minimumeisen van het product voldoet.
func BuySize(budget, limitPrice decimal.Decimal, fees Fees, product domain.Product) decimal.Decimal {
	if !budget.IsPositive() || !limitPrice.IsPositive() {
		return decimal.Zero
	}
	size := product.RoundSizeDown(budget.Div(limitPrice.Mul(one.Add(fees.Taker))))
	if !product.IsTradable(size, limitPrice) {
		return decimal.Zero
	}
	return size
}

// SellQuantity berekent hoeveel BTC er bij een winststap verkocht wordt: sellFraction van remaining,
// naar beneden afgerond. Is het deel of het restant daarna te klein om te verhandelen (dust-regel),
// dan wordt alles verkocht.
func SellQuantity(remaining, sellFraction, price decimal.Decimal, product domain.Product) decimal.Decimal {
	qty := product.RoundSizeDown(remaining.Mul(sellFraction))
	rest := remaining.Sub(qty)
	if !product.IsTradable(qty, price) || !product.IsTradable(rest, price) {
		return remaining
	}
	return qty
}
