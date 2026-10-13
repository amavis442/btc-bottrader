// Package lognotify schrijft meldingen naar de log. Gebruikt als er geen Telegram is ingesteld.
package lognotify

import (
	"context"
	"log/slog"

	"tradingbot/internal/usecase"
)

// Notifier logt meldingen.
type Notifier struct {
	log *slog.Logger
}

// New maakt een log-notifier.
func New(log *slog.Logger) *Notifier { return &Notifier{log: log} }

var _ usecase.Notifier = (*Notifier)(nil)

// Notify logt de melding op een niveau dat bij het belang past.
func (n *Notifier) Notify(ctx context.Context, level usecase.NotificationLevel, title, message string) {
	lvl := slog.LevelInfo
	switch level {
	case usecase.NotifyWarning:
		lvl = slog.LevelWarn
	case usecase.NotifyError:
		lvl = slog.LevelError
	}
	n.log.Log(ctx, lvl, "melding: "+title, "bericht", message)
}
