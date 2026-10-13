package usecase_test

import (
	"strings"
	"testing"
	"time"

	"tradingbot/internal/domain/strategy"
	"tradingbot/internal/usecase"
)

func TestDailyReport(t *testing.T) {
	s := fullCycle(t) // eindigt op dag 1 rond 07:30 UTC met een tweede, open positie
	ams, _ := time.LoadLocation("Europe/Amsterdam")
	daily, err := usecase.NewDailyReport("10:00", ams, s.reporting(0), s.notes, s.db, s.clock, nil)
	if err != nil {
		t.Fatal(err)
	}
	count := func() int { return s.notes.count("Dagoverzicht") }

	// eerste controle: alleen het tijdstip vastleggen, niets sturen
	if err := daily.Check(s.ctx); err != nil || count() != 0 {
		t.Fatalf("eerste controle: %d meldingen, %v", count(), err)
	}

	// 10:00 Amsterdam = 08:00 UTC
	s.clock.set(time.Date(2026, 10, 13, 7, 59, 0, 0, time.UTC))
	daily.Check(s.ctx)
	if count() != 0 {
		t.Fatal("overzicht vóór het geplande tijdstip")
	}
	s.clock.set(time.Date(2026, 10, 13, 8, 0, 30, 0, time.UTC))
	daily.Check(s.ctx)
	daily.Check(s.ctx)
	if count() != 1 {
		t.Fatalf("verwacht precies één overzicht, kreeg %d", count())
	}

	var msg, title string
	for _, n := range s.notes.list {
		if strings.HasPrefix(n.title, "Dagoverzicht") {
			msg, title = n.msg, n.title
		}
	}
	if title != "Dagoverzicht 13-10-2026" {
		t.Errorf("titel = %q", title)
	}
	for _, part := range []string{
		"Afgelopen 24 uur: 1 positie(s) afgerond (1 winst, 0 verlies)",
		"Open positie 2:", "volgend doel",
		"Totaal: 1 positie(s) afgerond",
		"Paper-vermogen:",
		"Toestand: " + string(strategy.PhaseInPosition),
	} {
		if !strings.Contains(msg, part) {
			t.Errorf("overzicht mist %q:\n%s", part, msg)
		}
	}

	// een dag later, na een herstart (nieuwe DailyReport op dezelfde opslag): weer precies één
	daily, _ = usecase.NewDailyReport("10:00", ams, s.reporting(0), s.notes, s.db, s.clock, nil)
	s.clock.set(time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC))
	daily.Check(s.ctx)
	daily.Check(s.ctx)
	if count() != 2 {
		t.Errorf("verwacht tweede overzicht na een dag, kreeg %d", count())
	}
}

func TestDailyReportMessageWithoutPositions(t *testing.T) {
	s := newSim(t, newPath(t0, "60000").flat(100))
	s.boot()
	s.run(50)
	s.trader.Pause(s.ctx, "onderhoud")

	daily, _ := usecase.NewDailyReport("08:00", time.UTC, s.reporting(0), s.notes, s.db, s.clock, nil)
	msg, err := daily.Message(s.ctx, s.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"geen posities afgerond", "Geen open positie", "trend VLAK", "Gepauzeerd: onderhoud"} {
		if !strings.Contains(msg, part) {
			t.Errorf("overzicht mist %q:\n%s", part, msg)
		}
	}
}

func TestDailyReportInvalidTime(t *testing.T) {
	if _, err := usecase.NewDailyReport("8 uur", time.UTC, nil, nil, nil, nil, nil); err == nil {
		t.Error("verwacht fout bij ongeldig tijdstip")
	}
}

func TestMultiNotifier(t *testing.T) {
	a, b := &recorder{}, &recorder{}
	usecase.MultiNotifier{a, b}.Notify(t.Context(), usecase.NotifyInfo, "x", "y")
	if len(a.list) != 1 || len(b.list) != 1 {
		t.Error("melding niet naar alle notifiers")
	}
}
