package lognotify

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"tradingbot/internal/usecase"
)

func TestLevels(t *testing.T) {
	var buf bytes.Buffer
	n := New(slog.New(slog.NewTextHandler(&buf, nil)))
	n.Notify(context.Background(), usecase.NotifyInfo, "Gekocht", "a")
	n.Notify(context.Background(), usecase.NotifyWarning, "Saldo", "b")
	n.Notify(context.Background(), usecase.NotifyError, "Afwijking", "c")

	out := buf.String()
	for _, want := range []string{`level=INFO msg="melding: Gekocht" bericht=a`, `level=WARN msg="melding: Saldo"`, `level=ERROR msg="melding: Afwijking"`} {
		if !strings.Contains(out, want) {
			t.Errorf("log mist %q:\n%s", want, out)
		}
	}
}
