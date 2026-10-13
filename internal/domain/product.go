package domain

import (
	"errors"

	"github.com/shopspring/decimal"
)

// Product bevat de handelsregels van een product op de exchange, zoals BTC-EUR.
type Product struct {
	ID             string
	BaseIncrement  decimal.Decimal // kleinste stap in BTC
	QuoteIncrement decimal.Decimal // kleinste prijsstap in EUR
	BaseMinSize    decimal.Decimal // minimale ordergrootte in BTC
	QuoteMinSize   decimal.Decimal // minimale orderwaarde in EUR
}

// Validate controleert of de productregels bruikbaar zijn.
func (p Product) Validate() error {
	if p.ID == "" {
		return errors.New("product: id ontbreekt")
	}
	if !p.BaseIncrement.IsPositive() || !p.QuoteIncrement.IsPositive() {
		return errors.New("product: increments moeten positief zijn")
	}
	if p.BaseMinSize.IsNegative() || p.QuoteMinSize.IsNegative() {
		return errors.New("product: minimumgroottes mogen niet negatief zijn")
	}
	return nil
}

// RoundSizeDown rondt een hoeveelheid BTC naar beneden af op de base-increment.
func (p Product) RoundSizeDown(size decimal.Decimal) decimal.Decimal {
	return roundDown(size, p.BaseIncrement)
}

// RoundPriceDown rondt een prijs naar beneden af op de quote-increment.
func (p Product) RoundPriceDown(price decimal.Decimal) decimal.Decimal {
	return roundDown(price, p.QuoteIncrement)
}

// RoundPriceUp rondt een prijs naar boven af op de quote-increment.
func (p Product) RoundPriceUp(price decimal.Decimal) decimal.Decimal {
	return price.Div(p.QuoteIncrement).Ceil().Mul(p.QuoteIncrement)
}

// IsTradable geeft aan of een order van deze grootte tegen deze prijs aan de minimumeisen voldoet.
func (p Product) IsTradable(size, price decimal.Decimal) bool {
	if !size.IsPositive() || size.LessThan(p.BaseMinSize) {
		return false
	}
	return !size.Mul(price).LessThan(p.QuoteMinSize)
}

func roundDown(value, increment decimal.Decimal) decimal.Decimal {
	return value.Div(increment).Floor().Mul(increment)
}
