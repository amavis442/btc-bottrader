package usecase

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"tradingbot/internal/domain"
)

const dailyReportKey = "daily_report"

// DailyReport stuurt elke dag op een vast tijdstip een overzicht (plan 4.7).
type DailyReport struct {
	hour, minute int
	location     *time.Location
	reporting    *Reporting
	notifier     Notifier
	kv           KeyValueStore
	clock        Clock
	log          *slog.Logger
}

type dailyReportState struct {
	LastSlot time.Time `json:"last_slot"` // tijdstip van het laatst verstuurde overzicht
}

// NewDailyReport maakt het dagoverzicht. at is een tijdstip als "08:00" in de opgegeven tijdzone.
func NewDailyReport(at string, location *time.Location, reporting *Reporting, notifier Notifier,
	kv KeyValueStore, clock Clock, log *slog.Logger) (*DailyReport, error) {
	t, err := time.Parse("15:04", at)
	if err != nil {
		return nil, fmt.Errorf("dagoverzicht: ongeldig tijdstip %q", at)
	}
	return &DailyReport{hour: t.Hour(), minute: t.Minute(), location: location, reporting: reporting,
		notifier: notifier, kv: kv, clock: clock, log: log}, nil
}

// slot geeft het meest recente geplande tijdstip op of vóór now.
func (d *DailyReport) slot(now time.Time) time.Time {
	local := now.In(d.location)
	s := time.Date(local.Year(), local.Month(), local.Day(), d.hour, d.minute, 0, 0, d.location)
	if s.After(local) {
		s = s.AddDate(0, 0, -1)
	}
	return s
}

// Check stuurt het overzicht als het geplande tijdstip is verstreken en het nog niet verstuurd is.
// Bij de allereerste start wordt niet meteen een overzicht gestuurd. Was de bot op het geplande
// tijdstip niet actief, dan volgt het overzicht bij de eerstvolgende controle.
func (d *DailyReport) Check(ctx context.Context) error {
	slot := d.slot(d.clock.Now())

	var st dailyReportState
	data, ok, err := d.kv.GetValue(ctx, dailyReportKey)
	if err != nil {
		return err
	}
	if ok {
		if err := json.Unmarshal(data, &st); err != nil {
			return err
		}
	}
	if !st.LastSlot.IsZero() && !slot.After(st.LastSlot) {
		return nil
	}
	if !st.LastSlot.IsZero() {
		msg, err := d.Message(ctx, slot)
		if err != nil {
			return fmt.Errorf("dagoverzicht: %w", err)
		}
		d.notifier.Notify(ctx, NotifyInfo, "Dagoverzicht "+slot.Format("02-01-2006"), msg)
	}

	st.LastSlot = slot
	data, err = json.Marshal(st)
	if err != nil {
		return err
	}
	return d.kv.SetValue(ctx, dailyReportKey, data)
}

// Message stelt de tekst van het overzicht samen: de afgelopen 24 uur, de open positie, het totaal
// en de toestand van de bot.
func (d *DailyReport) Message(ctx context.Context, slot time.Time) (string, error) {
	r := d.reporting
	day, err := r.ReportSince(ctx, slot.Add(-24*time.Hour))
	if err != nil {
		return "", err
	}
	all, err := r.Report(ctx, PeriodAll)
	if err != nil {
		return "", err
	}
	open, err := r.OpenPosition(ctx)
	if err != nil {
		return "", err
	}
	status := r.Status(ctx)
	_, quote := domain.Currencies(status.ProductID)

	var b strings.Builder
	if day.ClosedPositions == 0 {
		b.WriteString("Afgelopen 24 uur: geen posities afgerond.\n")
	} else {
		fmt.Fprintf(&b, "Afgelopen 24 uur: %d positie(s) afgerond (%d winst, %d verlies), resultaat %s %s (%s%%), fees %s.\n",
			day.ClosedPositions, day.Winners, day.Losers, day.Realized.EUR.StringFixed(2), quote,
			day.Realized.Pct.StringFixed(2), day.Fees.StringFixed(2))
	}

	if open == nil {
		b.WriteString("Geen open positie.\n")
	} else {
		fmt.Fprintf(&b, "Open positie %d: %s BTC, kostprijs %s, resultaat %s %s (%s%%)",
			open.ID, open.Remaining, open.CostPrice.StringFixed(2), open.Total.EUR.StringFixed(2), quote, open.Total.Pct.StringFixed(2))
		if s := open.Status; s != nil {
			switch {
			case s.TrailingStop != nil:
				fmt.Fprintf(&b, ", trailing stop %s (hoogste koers %s)", s.TrailingStop.StringFixed(2), s.TrailingHigh.StringFixed(2))
			case s.NextTarget != nil:
				fmt.Fprintf(&b, ", volgend doel %s", s.NextTarget.StringFixed(2))
			}
		}
		b.WriteString(".\n")
	}

	fmt.Fprintf(&b, "Totaal: %d positie(s) afgerond, resultaat %s %s (%s%%), fees %s.\n",
		all.ClosedPositions, all.Realized.EUR.StringFixed(2), quote, all.Realized.Pct.StringFixed(2), all.Fees.StringFixed(2))
	if p := all.Paper; p != nil {
		fmt.Fprintf(&b, "Paper-vermogen: %s %s (%s %s / %s%% t.o.v. %s).\n", p.Equity.StringFixed(2), quote,
			p.Change.EUR.StringFixed(2), quote, p.Change.Pct.StringFixed(2), p.StartBalance.StringFixed(2))
	}

	fmt.Fprintf(&b, "Toestand: %s, trend %s, koers %s", status.Phase, orUnknown(status.Trend), status.LastPrice.StringFixed(2))
	switch {
	case status.Halted:
		fmt.Fprintf(&b, ". GESTOPT door afwijking: %s", status.PauseReason)
	case status.Paused:
		fmt.Fprintf(&b, ". Gepauzeerd: %s", status.PauseReason)
	}
	b.WriteString(".")
	return b.String(), nil
}

func orUnknown(s string) string {
	if s == "" {
		return "onbekend"
	}
	return s
}
