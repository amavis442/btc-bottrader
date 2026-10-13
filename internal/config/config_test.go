package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func noEnv(string) (string, bool) { return "", false }

// tokenEnv levert alleen een API-token; config.example.json heeft bewust geen token.
func tokenEnv(k string) (string, bool) {
	if k == EnvAPIToken {
		return "0123456789abcdef", true
	}
	return "", false
}

func exampleConfig(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestExampleConfigIsValid(t *testing.T) {
	cfg, err := Parse(exampleConfig(t), tokenEnv)
	if err != nil {
		t.Fatalf("config.example.json is ongeldig: %v", err)
	}
	if cfg.Mode != ModePaper || cfg.ProductID != "BTC-EUR" || cfg.CandleInterval.D() != 10*time.Minute ||
		cfg.RunDuration.D() != 0 || cfg.BuyOrderTimeout.D() != 10*time.Minute || cfg.CooldownRising != 5 {
		t.Errorf("onverwachte waarden: %+v", cfg)
	}
	if cfg.TelegramEnabled() {
		t.Error("Telegram hoort uit te staan zonder token")
	}
}

func TestStrategyConversion(t *testing.T) {
	cfg, err := Parse(exampleConfig(t), tokenEnv)
	if err != nil {
		t.Fatal(err)
	}
	s := cfg.Strategy()
	checks := map[string][2]decimal.Decimal{
		"vlak":           {s.Trend.FlatThreshold, decimal.RequireFromString("0.005")},
		"winst 1":        {s.TakeProfits[0].NetProfit, decimal.RequireFromString("0.06")},
		"verkoop 2":      {s.TakeProfits[1].SellFraction, decimal.RequireFromString("0.65")},
		"trailing":       {s.TrailingDistance, decimal.RequireFromString("0.05")},
		"maker fallback": {cfg.FallbackFees().Maker, decimal.RequireFromString("0.0025")},
		"taker fallback": {cfg.FallbackFees().Taker, decimal.RequireFromString("0.005")},
	}
	for name, c := range checks {
		if !c[0].Equal(c[1]) {
			t.Errorf("%s = %s, verwacht %s", name, c[0], c[1])
		}
	}
	if s.Trend.ShortPeriod != 10 || s.Trend.LongPeriod != 50 || s.CooldownReversal != 50 || s.CooldownRising != 5 {
		t.Errorf("onverwachte strategie: %+v", s)
	}
}

func TestDefaultsApplyForMissingFields(t *testing.T) {
	cfg, err := Parse([]byte(`{"max_budget_eur": 50, "paper_start_eur": 100, "api": {"token": "0123456789abcdef"}}`), noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProductID != "BTC-EUR" || cfg.SMALong != 50 || cfg.API.Listen != "127.0.0.1:8080" || len(cfg.TakeProfit) != 2 {
		t.Errorf("standaardwaarden niet toegepast: %+v", cfg)
	}
}

func TestEnvOverridesSecrets(t *testing.T) {
	env := map[string]string{
		EnvAPIToken:           "token-uit-omgeving-123",
		EnvCoinbaseKeyName:    "organizations/x/apiKeys/y",
		EnvCoinbasePrivateKey: "-----BEGIN EC PRIVATE KEY-----",
		EnvTelegramBotToken:   "123:abc",
		EnvTelegramChatID:     "42",
	}
	cfg, err := Parse(exampleConfig(t), func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.API.Token != env[EnvAPIToken] || cfg.Coinbase.KeyName != env[EnvCoinbaseKeyName] ||
		cfg.Coinbase.PrivateKey != env[EnvCoinbasePrivateKey] || !cfg.TelegramEnabled() || cfg.Telegram.ChatID != "42" {
		t.Errorf("omgevingsvariabelen niet toegepast: %+v", cfg)
	}
}

func TestValidationErrors(t *testing.T) {
	tests := map[string]struct {
		json string
		want string
	}{
		"onbekende modus":       {`{"mode": "yolo"}`, "mode moet"},
		"live zonder keys":      {`{"mode": "live"}`, "verplicht in live-modus"},
		"geen budget":           {`{"max_budget_eur": 0}`, "max_budget_eur"},
		"paper te weinig":       {`{"max_budget_eur": 50, "paper_start_eur": 10}`, "paper_start_eur"},
		"interval":              {`{"candle_interval": "90s"}`, "candle_interval"},
		"één winststap":         {`{"take_profit": [{"net_profit_pct": 6, "sell_pct_of_remaining": 50}]}`, "precies 2"},
		"stap 2 lager":          {`{"take_profit": [{"net_profit_pct": 12, "sell_pct_of_remaining": 50}, {"net_profit_pct": 6, "sell_pct_of_remaining": 65}]}`, "tweede winststap"},
		"kort token":            {`{"api": {"token": "kort"}}`, "api.token"},
		"telegram half":         {`{"telegram": {"bot_token": "x"}}`, "telegram"},
		"tijdstip":              {`{"report": {"daily_at": "8 uur", "timezone": "Europe/Amsterdam"}}`, "daily_at"},
		"tijdzone":              {`{"report": {"daily_at": "08:00", "timezone": "Mars/Olympus"}}`, "timezone"},
		"postgres nog niet":     {`{"database": {"driver": "postgres", "dsn": "x"}}`, "niet ondersteund"},
		"loglevel":              {`{"log": {"level": "alles"}}`, "log.level"},
		"onbekend veld":         {`{"max_budgte_eur": 50}`, "unknown field"},
		"ongeldige duur":        {`{"run_duration": "altijd"}`, "ongeldige duur"},
		"negatieve offset":      {`{"buy_limit_offset_pct": -1}`, "buy_limit_offset_pct"},
		"trend korter dan kort": {`{"sma_short": 50, "sma_long": 10}`, "korte periode"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(tt.json), noEnv)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("verwacht fout met %q, kreeg %v", tt.want, err)
			}
		})
	}
}

func TestExampleConfigRunsWithoutAnyKeys(t *testing.T) {
	// papertrading zonder API-token, Coinbase-keys of Telegram
	cfg, err := Parse(exampleConfig(t), noEnv)
	if err != nil {
		t.Fatalf("voorbeeldconfig zonder keys afgekeurd: %v", err)
	}
	if cfg.API.Token != "" || cfg.Coinbase.KeyName != "" || cfg.TelegramEnabled() {
		t.Errorf("verwacht geen keys: %+v", cfg)
	}
}

func TestDurationAsSeconds(t *testing.T) {
	cfg, err := Parse([]byte(`{"max_budget_eur": 50, "paper_start_eur": 100, "api": {"token": "0123456789abcdef"}, "run_duration": 3600}`), noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RunDuration.D() != time.Hour {
		t.Errorf("run_duration = %s, verwacht 1h", cfg.RunDuration.D())
	}
}

func TestLoad(t *testing.T) {
	t.Setenv(EnvAPIToken, "0123456789abcdef")
	if _, err := Load(filepath.Join("..", "..", "config.example.json")); err != nil {
		t.Errorf("Load: %v", err)
	}
	if _, err := Load("bestaat-niet.json"); err == nil {
		t.Error("verwacht fout bij ontbrekend bestand")
	}
}
