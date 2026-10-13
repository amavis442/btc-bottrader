// Command tradingbot start de BTC-tradingbot.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
	_ "time/tzdata" // tijdzones ook beschikbaar op Windows

	"tradingbot/internal/adapter/exchange/coinbase"
	"tradingbot/internal/adapter/exchange/paper"
	"tradingbot/internal/adapter/httpapi"
	"tradingbot/internal/adapter/notify/lognotify"
	"tradingbot/internal/adapter/notify/telegram"
	"tradingbot/internal/adapter/storage/sqlite"
	"tradingbot/internal/config"
	"tradingbot/internal/domain"
	"tradingbot/internal/domain/strategy"
	"tradingbot/internal/platform"
	"tradingbot/internal/usecase"
)

// version wordt bij het bouwen gezet via -ldflags "-X main.version=..." (zie de Makefile).
var version = "dev"

func main() {
	configPath := flag.String("config", "config.json", "pad naar het configuratiebestand")
	showVersion := flag.Bool("version", false, "toon de versie en stop")
	flag.Parse()

	if *showVersion {
		fmt.Println("tradingbot", version)
		return
	}

	if err := run(*configPath); err != nil {
		fmt.Fprintln(os.Stderr, "fout:", err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	log, logCloser := platform.NewLogger(cfg.Log.Level, cfg.Log.File)
	defer logCloser.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	if d := cfg.RunDuration.D(); d > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}

	db, err := sqlite.Open(cfg.Database.DSN)
	if err != nil {
		return err
	}
	defer db.Close()

	clock := platform.SystemClock{}
	var clientOpts []coinbase.Option
	hasKey := cfg.Coinbase.KeyName != "" && cfg.Coinbase.PrivateKey != ""
	if hasKey {
		signer, err := coinbase.NewSigner(cfg.Coinbase.KeyName, cfg.Coinbase.PrivateKey)
		if err != nil {
			return err
		}
		clientOpts = append(clientOpts, coinbase.WithSigner(signer))
	}
	coinbaseClient := coinbase.New(clientOpts...)
	product, err := usecase.ResolveProduct(ctx, coinbaseClient, cfg.ProductID, cfg.FallbackProductID)
	if err != nil {
		return fmt.Errorf("product bepalen: %w", err)
	}
	if product.ID != cfg.ProductID {
		log.Warn("primair product niet beschikbaar, terugval gebruikt", "primair", cfg.ProductID, "product", product.ID)
	}

	machine, err := strategy.NewMachine(cfg.Strategy(), product)
	if err != nil {
		return err
	}
	market, err := usecase.NewMarketDataService(coinbaseClient, db, clock, log,
		product.ID, cfg.CandleInterval.D(), cfg.SMALong)
	if err != nil {
		return err
	}

	fees, err := feeSource(ctx, cfg, hasKey, coinbaseClient, clock, log)
	if err != nil {
		return err
	}
	var exchange usecase.Exchange
	if cfg.Mode == config.ModeLive {
		live := coinbase.NewExchange(coinbaseClient, product)
		if err := checkLiveAccount(ctx, live, product.ID, log); err != nil {
			return err
		}
		exchange = live
	} else {
		exchange = paper.New(db, db, coinbaseClient, fees, clock, product, cfg.PaperStart)
	}
	notifiers := usecase.MultiNotifier{lognotify.New(log)}
	var tg *telegram.Notifier
	if cfg.TelegramEnabled() {
		tg = telegram.New(cfg.Telegram.BotToken, cfg.Telegram.ChatID, fmt.Sprintf("Tradingbot (%s)", cfg.Mode), log)
		notifiers = append(notifiers, tg)
	} else {
		log.Warn("Telegram is niet ingesteld; meldingen gaan alleen naar de log")
	}
	var notifier usecase.Notifier = notifiers

	trader, err := usecase.NewTrader(ctx, usecase.TraderConfig{
		Budget:           cfg.MaxBudget,
		BuyLimitOffset:   cfg.BuyLimitOffset(),
		BuyOrderTimeout:  cfg.BuyOrderTimeout.D(),
		StopOrderTimeout: cfg.StopOrderTimeout.D(),
		FeeRefresh:       cfg.Fees.RefreshInterval.D(),
	}, usecase.TraderDeps{
		Product: product, Machine: machine, Market: market, Tickers: coinbaseClient, Exchange: exchange,
		Fees: fees, Store: db, KV: db, Notifier: notifier, Clock: clock, Log: log,
	})
	if err != nil {
		return err
	}

	location, _ := time.LoadLocation(cfg.Report.Timezone) // gevalideerd in config
	reporting := usecase.NewReporting(usecase.ReportingConfig{
		Mode: cfg.Mode, ProductID: product.ID, Budget: cfg.MaxBudget, PaperStart: cfg.PaperStart,
		StartedAt: clock.Now(), RunDuration: cfg.RunDuration.D(), Location: location,
		Cooldowns: map[strategy.CooldownKind]int{
			strategy.CooldownReversal: cfg.CooldownReversal,
			strategy.CooldownRising:   cfg.CooldownRising,
		},
	}, db, trader, machine, market, exchange, clock)

	daily, err := usecase.NewDailyReport(cfg.Report.DailyAt, location, reporting, notifier, db, clock, log)
	if err != nil {
		return err
	}

	apiToken, err := resolveAPIToken(cfg, log)
	if err != nil {
		return err
	}
	api := httpapi.New(httpapi.Config{Listen: cfg.API.Listen, Token: apiToken, CORSOrigins: cfg.API.CORSOrigins},
		reporting, control{trader: trader, cancel: cancel}, log)
	go func() {
		if err := api.Run(ctx); err != nil {
			cancel(err) // zonder API is de bot niet te volgen of te bedienen
		}
	}()

	log.Info("bot gestart", "versie", version, "modus", cfg.Mode, "product", product.ID, "interval", cfg.CandleInterval.D(),
		"inleg", cfg.MaxBudget, "looptijd", runDurationText(cfg.RunDuration.D()))
	notifier.Notify(ctx, usecase.NotifyInfo, "Bot gestart", fmt.Sprintf("modus %s, product %s", cfg.Mode, product.ID))

	failures := 0
	tick := func(ctx context.Context) {
		err := trader.Tick(ctx)
		if err == nil {
			failures = 0
			if err := daily.Check(ctx); err != nil && ctx.Err() == nil {
				log.Error("dagoverzicht mislukt", "fout", err)
			}
			return
		}
		if ctx.Err() != nil {
			return // afsluiten, geen fout
		}
		failures++
		log.Error("ronde mislukt", "fout", err, "achter_elkaar", failures)
		if failures == 3 || failures%60 == 0 {
			notifier.Notify(ctx, usecase.NotifyError, "Herhaalde fouten", fmt.Sprintf("%d rondes achter elkaar mislukt: %v", failures, err))
		}
	}

	tick(ctx)
	platform.EveryMinute(ctx, 5*time.Second, tick)

	reason := "gestopt"
	switch cause := context.Cause(ctx); {
	case errors.Is(cause, context.DeadlineExceeded):
		reason = "looptijd verstreken"
	case errors.Is(cause, errStopRequested):
		reason = "gestopt via de API"
	case cause != nil && !errors.Is(cause, context.Canceled):
		log.Error("bot gestopt door een fout", "fout", cause)
		reason = "gestopt door een fout: " + cause.Error()
	}
	// Met een nieuwe context, want ctx is al afgelopen.
	notifier.Notify(context.Background(), usecase.NotifyInfo, "Bot gestopt", reason+"; open posities en orders blijven staan")
	if tg != nil {
		closeCtx, cancelClose := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancelClose()
		if err := tg.Close(closeCtx); err != nil {
			log.Warn("laatste meldingen niet verstuurd", "fout", err)
		}
	}
	return nil
}

var errStopRequested = errors.New("stop aangevraagd via de API")

// resolveAPIToken geeft het token voor de API: uit de config, of anders een automatisch aangemaakt
// token dat naast de database bewaard wordt (zodat het na een herstart hetzelfde blijft).
func resolveAPIToken(cfg *config.Config, log *slog.Logger) (string, error) {
	if cfg.API.Token != "" {
		return cfg.API.Token, nil
	}
	path := filepath.Join(filepath.Dir(cfg.Database.DSN), "api_token")
	token, created, err := platform.LoadOrCreateToken(path)
	if err != nil {
		return "", fmt.Errorf("API-token: %w", err)
	}
	if created {
		log.Info("API-token aangemaakt; gebruik het als 'Authorization: Bearer <token>'", "bestand", path)
	} else {
		log.Info("API-token gelezen", "bestand", path)
	}
	return token, nil
}

// feeSource bepaalt waar de fee-tarieven vandaan komen. Met een API-key: de echte tarieven van het
// account (live verplicht, paper valt terug op de config). Zonder key: de terugvalwaarden uit de config.
func feeSource(ctx context.Context, cfg *config.Config, hasKey bool, client *coinbase.Client,
	clock usecase.Clock, log *slog.Logger) (usecase.FeeSource, error) {
	fallback := usecase.StaticFees(cfg.FallbackFees())
	if !hasKey {
		log.Info("geen Coinbase API-key: terugval-fees uit de config", "maker", cfg.Fees.FallbackMakerPct, "taker", cfg.Fees.FallbackTakerPct)
		return fallback, nil
	}
	cached := usecase.NewCachedFees(client, cfg.Fees.RefreshInterval.D(), clock)
	fees, err := cached.Fees(ctx)
	if err != nil {
		if cfg.Mode == config.ModeLive {
			return nil, fmt.Errorf("live trading: fee-tarieven ophalen bij Coinbase mislukt: %w", err)
		}
		log.Warn("fee-tarieven ophalen mislukt, terugval-fees uit de config", "fout", err)
		return fallback, nil
	}
	log.Info("fee-tarieven van Coinbase", "maker", fees.Maker, "taker", fees.Taker)
	return cached, nil
}

// checkLiveAccount controleert vóór live trading dat de API-key werkt en logt de saldi.
func checkLiveAccount(ctx context.Context, exchange usecase.Exchange, productID string, log *slog.Logger) error {
	balances, err := exchange.Balances(ctx)
	if err != nil {
		return fmt.Errorf("live trading: saldi opvragen mislukt; controleer de API-key en de rechten (view + trade): %w", err)
	}
	base, quote := domain.Currencies(productID)
	log.Warn("LIVE TRADING: er worden echte orders geplaatst",
		quote, balances[quote].Available, base, balances[base].Available)
	return nil
}

// control verbindt de API met de trader en het stoppen van de bot.
type control struct {
	trader *usecase.Trader
	cancel context.CancelCauseFunc
}

func (c control) Pause(ctx context.Context, reason string) error { return c.trader.Pause(ctx, reason) }
func (c control) Resume(ctx context.Context) error               { return c.trader.Resume(ctx) }
func (c control) Stop()                                          { c.cancel(errStopRequested) }

func runDurationText(d time.Duration) string {
	if d == 0 {
		return "onbepaald"
	}
	return d.String()
}
