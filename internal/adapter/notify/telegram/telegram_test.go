package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"tradingbot/internal/usecase"
)

// fakeTelegram neemt berichten op en geeft per verzoek het volgende antwoord uit responses
// (daarna altijd 200 OK).
type fakeTelegram struct {
	mu        sync.Mutex
	messages  []sendRequest
	paths     []string
	responses []func(w http.ResponseWriter)
}

func (f *fakeTelegram) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paths = append(f.paths, r.URL.Path)
	if len(f.responses) > 0 {
		resp := f.responses[0]
		f.responses = f.responses[1:]
		resp(w)
		return
	}
	var req sendRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	f.messages = append(f.messages, req)
	w.Write([]byte(`{"ok":true,"result":{}}`))
}

func (f *fakeTelegram) sent() []sendRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sendRequest(nil), f.messages...)
}

func setup(t *testing.T, debounce time.Duration) (*Notifier, *fakeTelegram, *bytes.Buffer) {
	t.Helper()
	fake := &fakeTelegram{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	logs := &bytes.Buffer{}
	n := New("123:geheim", "42", "Tradingbot (paper)", slog.New(slog.NewTextHandler(logs, nil)),
		WithBaseURL(srv.URL), WithDebounce(debounce), WithRetry(2, time.Millisecond))
	t.Cleanup(func() { n.Close(context.Background()) })
	return n, fake, logs
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("voorwaarde niet bereikt")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestBundlesMessagesWithinDebounce(t *testing.T) {
	n, fake, _ := setup(t, 100*time.Millisecond)
	ctx := context.Background()
	n.Notify(ctx, usecase.NotifyInfo, "Gekocht", "0,0008 BTC")
	n.Notify(ctx, usecase.NotifyInfo, "Toestand gewijzigd", "ZOEK_INSTAP → IN_POSITIE")
	n.Notify(ctx, usecase.NotifyWarning, "Let op", "iets")

	waitFor(t, func() bool { return len(fake.sent()) == 1 })
	msg := fake.sent()[0]
	if msg.ChatID != "42" || !msg.DisableWebPagePreview {
		t.Errorf("verzoek = %+v", msg)
	}
	want := "Tradingbot (paper)\n\nℹ️ Gekocht\n0,0008 BTC\n\nℹ️ Toestand gewijzigd\nZOEK_INSTAP → IN_POSITIE\n\n⚠️ Let op\niets"
	if msg.Text != want {
		t.Errorf("tekst =\n%s\nverwacht\n%s", msg.Text, want)
	}
	if fake.paths[0] != "/bot123:geheim/sendMessage" {
		t.Errorf("pad = %s", fake.paths[0])
	}

	// een latere melding komt in een nieuw bericht
	n.Notify(ctx, usecase.NotifyError, "Fout", "x")
	waitFor(t, func() bool { return len(fake.sent()) == 2 })
	if !strings.Contains(fake.sent()[1].Text, "🚨 Fout") {
		t.Errorf("tweede bericht = %q", fake.sent()[1].Text)
	}
}

func TestCloseFlushesPending(t *testing.T) {
	n, fake, _ := setup(t, time.Hour) // zonder Close zou er een uur gewacht worden
	n.Notify(context.Background(), usecase.NotifyInfo, "Bot gestopt", "looptijd verstreken")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := n.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if sent := fake.sent(); len(sent) != 1 || !strings.Contains(sent[0].Text, "Bot gestopt") {
		t.Errorf("verstuurd = %+v", sent)
	}

	// na Close worden meldingen genegeerd, en een tweede Close is geen probleem
	n.Notify(context.Background(), usecase.NotifyInfo, "te laat", "")
	if err := n.Close(ctx); err != nil {
		t.Error(err)
	}
	if len(fake.sent()) != 1 {
		t.Error("melding na Close is toch verstuurd")
	}
}

func TestRetries(t *testing.T) {
	n, fake, _ := setup(t, time.Millisecond)
	fake.responses = []func(http.ResponseWriter){
		func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"ok":false,"description":"Too Many Requests","parameters":{"retry_after":1}}`))
		},
		func(w http.ResponseWriter) { w.WriteHeader(http.StatusBadGateway) },
	}
	start := time.Now()
	n.Notify(context.Background(), usecase.NotifyInfo, "a", "b")
	waitFor(t, func() bool { return len(fake.sent()) == 1 })
	if time.Since(start) < time.Second {
		t.Error("retry_after van Telegram niet gerespecteerd")
	}
}

func TestRejectedMessageIsNotRetriedAndHidesToken(t *testing.T) {
	n, fake, logs := setup(t, time.Millisecond)
	fake.responses = []func(http.ResponseWriter){
		func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"ok":false,"description":"Bad Request: chat not found"}`))
		},
	}
	n.Notify(context.Background(), usecase.NotifyInfo, "a", "b")
	n.Close(context.Background())

	if len(fake.paths) != 1 || len(fake.sent()) != 0 {
		t.Errorf("verzoeken = %d, verstuurd = %d; verwacht 1 en 0", len(fake.paths), len(fake.sent()))
	}
	if !strings.Contains(logs.String(), "chat not found") || strings.Contains(logs.String(), "geheim") {
		t.Errorf("log = %s", logs)
	}
}

func TestNetworkErrorHidesToken(t *testing.T) {
	logs := &bytes.Buffer{}
	n := New("123:geheim", "42", "", slog.New(slog.NewTextHandler(logs, nil)),
		WithBaseURL("http://127.0.0.1:1"), WithDebounce(time.Millisecond), WithRetry(1, time.Millisecond))
	n.Notify(context.Background(), usecase.NotifyInfo, "a", "b")
	n.Close(context.Background())
	if !strings.Contains(logs.String(), "niet verstuurd") || strings.Contains(logs.String(), "geheim") {
		t.Errorf("log = %s", logs)
	}
}

func TestSplit(t *testing.T) {
	long := strings.Repeat("x", 3000)
	parts := split("Kop", []string{long, long, "kort"})
	if len(parts) != 2 {
		t.Fatalf("delen = %d, verwacht 2", len(parts))
	}
	for _, p := range parts {
		if !strings.HasPrefix(p, "Kop\n\n") || len(p) > maxMessageLength {
			t.Errorf("deel van %d tekens zonder kop of te lang", len(p))
		}
	}
	if !strings.HasSuffix(parts[1], "kort") {
		t.Error("korte melding hoort bij het tweede deel")
	}

	huge := split("", []string{strings.Repeat("y", 10000)})
	if len(huge) != 1 || len(huge[0]) > maxMessageLength || !strings.HasSuffix(huge[0], "...") {
		t.Errorf("te lange melding niet afgekapt: %d tekens", len(huge[0]))
	}
}

func TestQueueFullDropsWithoutBlocking(t *testing.T) {
	block, arrived := make(chan struct{}), make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case arrived <- struct{}{}:
		default:
		}
		<-block
	}))
	defer srv.Close()
	defer close(block)
	logs := &safeBuffer{}
	n := New("t", "c", "", slog.New(slog.NewTextHandler(logs, nil)), WithBaseURL(srv.URL), WithDebounce(time.Millisecond))

	// eerste melding: de verzending blijft hangen, dus de wachtrij wordt niet meer geleegd
	n.Notify(context.Background(), usecase.NotifyInfo, "eerste", "")
	<-arrived

	done := make(chan struct{})
	go func() {
		for i := 0; i < queueSize+10; i++ {
			n.Notify(context.Background(), usecase.NotifyInfo, "m", "")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Notify blokkeert bij een volle wachtrij")
	}
	if !strings.Contains(logs.String(), "wachtrij vol") {
		t.Error("verwacht waarschuwing over volle wachtrij")
	}
}

type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
