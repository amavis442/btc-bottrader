// Package config laadt en valideert config.json.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"tradingbot/internal/domain/indicator"
	"tradingbot/internal/domain/strategy"
)

// Duration is een time.Duration die in JSON als tekst staat, bijv. "10m" of "0".
type Duration time.Duration

// UnmarshalJSON leest een duur als tekst ("72h") of als getal in seconden.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		var secs int64
		if err := json.Unmarshal(b, &secs); err != nil {
			return fmt.Errorf("ongeldige duur %s", b)
		}
		*d = Duration(time.Duration(secs) * time.Second)
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("ongeldige duur %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// D geeft de waarde als time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// Config is de volledige configuratie van de bot. Percentages staan als procenten (0.25 = 0,25%).
type Config struct {
	Mode              string          `json:"mode"`
	ProductID         string          `json:"product_id"`
	FallbackProductID string          `json:"fallback_product_id"`
	RunDuration       Duration        `json:"run_duration"`
	CandleInterval    Duration        `json:"candle_interval"`
	SMAShort          int             `json:"sma_short"`
	SMALong           int             `json:"sma_long"`
	FlatThresholdPct  decimal.Decimal `json:"flat_threshold_pct"`
	MaxBudget         decimal.Decimal `json:"max_budget_eur"`
	PaperStart        decimal.Decimal `json:"paper_start_eur"`
	Fees              FeesConfig      `json:"fees"`
	TakeProfit        []TakeProfit    `json:"take_profit"`
	TrailingStopPct   decimal.Decimal `json:"trailing_stop_pct"`
	BuyLimitOffsetPct decimal.Decimal `json:"buy_limit_offset_pct"`
	BuyOrderTimeout   Duration        `json:"buy_order_timeout"`
	StopOrderTimeout  Duration        `json:"stop_order_timeout"`
	CooldownReversal  int             `json:"cooldown_candles_reversal"`
	CooldownRising    int             `json:"cooldown_candles_rising"`
	Database          DatabaseConfig  `json:"database"`
	API               APIConfig       `json:"api"`
	Coinbase          CoinbaseConfig  `json:"coinbase"`
	Telegram          TelegramConfig  `json:"telegram"`
	Report            ReportConfig    `json:"report"`
	Log               LogConfig       `json:"log"`
}

type FeesConfig struct {
	RefreshInterval  Duration        `json:"refresh_interval"`
	FallbackMakerPct decimal.Decimal `json:"fallback_maker_pct"`
	FallbackTakerPct decimal.Decimal `json:"fallback_taker_pct"`
}

type TakeProfit struct {
	NetProfitPct       decimal.Decimal `json:"net_profit_pct"`
	SellPctOfRemaining decimal.Decimal `json:"sell_pct_of_remaining"`
}

type DatabaseConfig struct {
	Driver string `json:"driver"`
	DSN    string `json:"dsn"`
}

type APIConfig struct {
	Listen      string   `json:"listen"`
	Token       string   `json:"token"`
	CORSOrigins []string `json:"cors_origins"`
}

type CoinbaseConfig struct {
	KeyName    string `json:"key_name"`
	PrivateKey string `json:"private_key"`
}

type TelegramConfig struct {
	BotToken string `json:"bot_token"`
	ChatID   string `json:"chat_id"`
}

type ReportConfig struct {
	DailyAt  string `json:"daily_at"`
	Timezone string `json:"timezone"`
}

type LogConfig struct {
	Level string `json:"level"`
	File  string `json:"file"`
}

const (
	ModePaper = "paper"
	ModeLive  = "live"
)

// Omgevingsvariabelen die secrets uit config.json kunnen overschrijven.
const (
	EnvCoinbaseKeyName    = "TRADINGBOT_COINBASE_KEY_NAME"
	EnvCoinbasePrivateKey = "TRADINGBOT_COINBASE_PRIVATE_KEY"
	EnvAPIToken           = "TRADINGBOT_API_TOKEN"
	EnvTelegramBotToken   = "TRADINGBOT_TELEGRAM_BOT_TOKEN"
	EnvTelegramChatID     = "TRADINGBOT_TELEGRAM_CHAT_ID"
)

// Load leest het configuratiebestand, past omgevingsvariabelen toe en valideert het resultaat.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return Parse(data, os.LookupEnv)
}

// Parse ontleedt de configuratie. lookupEnv wordt gebruikt voor de overschrijvingen.
func Parse(data []byte, lookupEnv func(string) (string, bool)) (*Config, error) {
	cfg := defaults()
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	for env, dst := range map[string]*string{
		EnvCoinbaseKeyName:    &cfg.Coinbase.KeyName,
		EnvCoinbasePrivateKey: &cfg.Coinbase.PrivateKey,
		EnvAPIToken:           &cfg.API.Token,
		EnvTelegramBotToken:   &cfg.Telegram.BotToken,
		EnvTelegramChatID:     &cfg.Telegram.ChatID,
	} {
		if v, ok := lookupEnv(env); ok && v != "" {
			*dst = v
		}
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func defaults() *Config {
	pct := decimal.RequireFromString
	return &Config{
		Mode:              ModePaper,
		ProductID:         "BTC-EUR",
		FallbackProductID: "BTC-USD",
		CandleInterval:    Duration(10 * time.Minute),
		SMAShort:          10,
		SMALong:           50,
		FlatThresholdPct:  pct("0.5"),
		Fees: FeesConfig{
			RefreshInterval:  Duration(24 * time.Hour),
			FallbackMakerPct: pct("0.25"),
			FallbackTakerPct: pct("0.50"),
		},
		TakeProfit: []TakeProfit{
			{NetProfitPct: pct("6"), SellPctOfRemaining: pct("50")},
			{NetProfitPct: pct("12"), SellPctOfRemaining: pct("65")},
		},
		TrailingStopPct:  pct("5"),
		BuyOrderTimeout:  Duration(10 * time.Minute),
		StopOrderTimeout: Duration(60 * time.Second),
		CooldownReversal: 50,
		CooldownRising:   5,
		Database:         DatabaseConfig{Driver: "sqlite", DSN: "data/tradingbot.db"},
		API:              APIConfig{Listen: "127.0.0.1:8080"},
		Report:           ReportConfig{DailyAt: "08:00", Timezone: "Europe/Amsterdam"},
		Log:              LogConfig{Level: "info", File: "logs/tradingbot.log"},
	}
}

// Validate controleert de hele configuratie en meldt alle fouten tegelijk.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	switch c.Mode {
	case ModePaper:
	case ModeLive:
		if c.Coinbase.KeyName == "" || c.Coinbase.PrivateKey == "" {
			add("coinbase.key_name en coinbase.private_key zijn verplicht in live-modus")
		}
	default:
		add("mode moet %q of %q zijn, niet %q", ModePaper, ModeLive, c.Mode)
	}
	if c.ProductID == "" {
		add("product_id is verplicht")
	}
	if c.RunDuration < 0 {
		add("run_duration mag niet negatief zijn")
	}
	if iv := c.CandleInterval.D(); iv < time.Minute || iv%time.Minute != 0 {
		add("candle_interval moet een veelvoud van 1m zijn")
	}
	if !c.MaxBudget.IsPositive() {
		add("max_budget_eur moet positief zijn")
	}
	if c.Mode == ModePaper && c.PaperStart.LessThan(c.MaxBudget) {
		add("paper_start_eur moet minstens max_budget_eur zijn")
	}
	if len(c.TakeProfit) != 2 {
		add("take_profit moet precies 2 stappen hebben")
	}
	if c.BuyLimitOffsetPct.IsNegative() || c.BuyLimitOffsetPct.GreaterThanOrEqual(decimal.NewFromInt(100)) {
		add("buy_limit_offset_pct moet tussen 0 en 100 liggen")
	}
	if c.BuyOrderTimeout <= 0 || c.StopOrderTimeout <= 0 {
		add("buy_order_timeout en stop_order_timeout moeten positief zijn")
	}
	if c.Fees.RefreshInterval <= 0 {
		add("fees.refresh_interval moet positief zijn")
	}
	if err := c.FallbackFees().Validate(); err != nil {
		add("fees: %w", err)
	}
	if len(c.TakeProfit) == 2 {
		if err := c.Strategy().Validate(); err != nil {
			errs = append(errs, err)
		}
	}
	if c.Database.Driver != "sqlite" {
		add("database.driver %q wordt (nog) niet ondersteund", c.Database.Driver)
	}
	if c.Database.DSN == "" {
		add("database.dsn is verplicht")
	}
	if c.API.Listen == "" {
		add("api.listen is verplicht")
	}
	if c.API.Token != "" && len(c.API.Token) < 16 {
		add("api.token moet minstens 16 tekens zijn, of leeg om er automatisch een te laten maken")
	}
	if (c.Telegram.BotToken == "") != (c.Telegram.ChatID == "") {
		add("telegram.bot_token en telegram.chat_id moeten allebei gevuld of allebei leeg zijn")
	}
	if _, err := time.Parse("15:04", c.Report.DailyAt); err != nil {
		add("report.daily_at moet de vorm UU:MM hebben")
	}
	if _, err := time.LoadLocation(c.Report.Timezone); err != nil {
		add("report.timezone: %w", err)
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		add("log.level moet debug, info, warn of error zijn")
	}

	if len(errs) > 0 {
		return fmt.Errorf("config ongeldig: %w", errors.Join(errs...))
	}
	return nil
}

var hundred = decimal.NewFromInt(100)

func fraction(pct decimal.Decimal) decimal.Decimal { return pct.Div(hundred) }

// Strategy zet de strategieparameters om naar het domein (procenten → fracties).
func (c *Config) Strategy() strategy.Config {
	cfg := strategy.Config{
		Trend: indicator.TrendConfig{
			ShortPeriod:   c.SMAShort,
			LongPeriod:    c.SMALong,
			FlatThreshold: fraction(c.FlatThresholdPct),
		},
		TrailingDistance: fraction(c.TrailingStopPct),
		CooldownReversal: c.CooldownReversal,
		CooldownRising:   c.CooldownRising,
	}
	for i := 0; i < len(c.TakeProfit) && i < len(cfg.TakeProfits); i++ {
		cfg.TakeProfits[i] = strategy.TakeProfit{
			NetProfit:    fraction(c.TakeProfit[i].NetProfitPct),
			SellFraction: fraction(c.TakeProfit[i].SellPctOfRemaining),
		}
	}
	return cfg
}

// FallbackFees geeft de terugval-fees als fracties.
func (c *Config) FallbackFees() strategy.Fees {
	return strategy.Fees{Maker: fraction(c.Fees.FallbackMakerPct), Taker: fraction(c.Fees.FallbackTakerPct)}
}

// BuyLimitOffset geeft de offset van de koop-limit als fractie.
func (c *Config) BuyLimitOffset() decimal.Decimal { return fraction(c.BuyLimitOffsetPct) }

// TelegramEnabled geeft aan of Telegram-notificaties geconfigureerd zijn.
func (c *Config) TelegramEnabled() bool { return c.Telegram.BotToken != "" }
