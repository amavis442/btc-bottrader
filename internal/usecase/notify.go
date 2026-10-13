package usecase

import "context"

// MultiNotifier stuurt elke melding naar alle notifiers (bijv. log en Telegram).
type MultiNotifier []Notifier

// Notify stuurt de melding door.
func (m MultiNotifier) Notify(ctx context.Context, level NotificationLevel, title, message string) {
	for _, n := range m {
		n.Notify(ctx, level, title, message)
	}
}
