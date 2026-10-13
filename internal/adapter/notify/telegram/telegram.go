// Package telegram verstuurt meldingen via een Telegram-bot.
//
// Meldingen worden op de achtergrond verstuurd, zodat een trage of onbereikbare Telegram de bot nooit
// ophoudt. Meldingen die kort na elkaar komen (binnen de bundeltijd) gaan samen in één bericht; één
// ronde van de bot levert zo één bericht op in plaats van een reeks losse berichten.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"tradingbot/internal/usecase"
)

const (
	// DefaultBaseURL is de basis-URL van de Telegram Bot API.
	DefaultBaseURL = "https://api.telegram.org"
	// maxMessageLength blijft onder de limiet van Telegram (4096 tekens).
	maxMessageLength = 4000
	queueSize        = 200
)

// Notifier verstuurt meldingen naar één Telegram-chat.
type Notifier struct {
	baseURL  string
	token    string
	chatID   string
	header   string
	http     *http.Client
	log      *slog.Logger
	debounce time.Duration
	retries  int
	backoff  time.Duration

	queue    chan string
	done     chan struct{}
	finished chan struct{}
	mu       sync.Mutex
	closed   bool
}

// Option past de notifier aan.
type Option func(*Notifier)

// WithBaseURL zet een andere basis-URL (voor tests).
func WithBaseURL(u string) Option { return func(n *Notifier) { n.baseURL = u } }

// WithDebounce zet de bundeltijd.
func WithDebounce(d time.Duration) Option { return func(n *Notifier) { n.debounce = d } }

// WithRetry zet het aantal herhalingen en de start-wachttijd bij fouten.
func WithRetry(retries int, backoff time.Duration) Option {
	return func(n *Notifier) { n.retries, n.backoff = retries, backoff }
}

// New maakt de notifier en start het versturen op de achtergrond. header staat boven elk bericht,
// bijvoorbeeld "Tradingbot (paper)". Roep Close aan bij het afsluiten.
func New(token, chatID, header string, log *slog.Logger, opts ...Option) *Notifier {
	n := &Notifier{
		baseURL: DefaultBaseURL, token: token, chatID: chatID, header: header,
		http: &http.Client{Timeout: 15 * time.Second}, log: log,
		debounce: 2 * time.Second, retries: 3, backoff: time.Second,
		queue: make(chan string, queueSize), done: make(chan struct{}), finished: make(chan struct{}),
	}
	for _, opt := range opts {
		opt(n)
	}
	go n.run()
	return n
}

var _ usecase.Notifier = (*Notifier)(nil)

var icons = map[usecase.NotificationLevel]string{
	usecase.NotifyInfo:    "ℹ️",
	usecase.NotifyWarning: "⚠️",
	usecase.NotifyError:   "🚨",
}

// Notify zet een melding in de wachtrij. Blokkeert nooit; bij een volle wachtrij vervalt de melding.
func (n *Notifier) Notify(_ context.Context, level usecase.NotificationLevel, title, message string) {
	text := fmt.Sprintf("%s %s\n%s", icons[level], title, message)

	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		n.log.Warn("Telegram: melding na afsluiten genegeerd", "titel", title)
		return
	}
	select {
	case n.queue <- text:
	default:
		n.log.Warn("Telegram: wachtrij vol, melding vervalt", "titel", title)
	}
}

// Close verstuurt de meldingen die nog in de wachtrij staan en stopt. Wacht maximaal tot ctx afloopt.
func (n *Notifier) Close(ctx context.Context) error {
	n.mu.Lock()
	if !n.closed {
		n.closed = true
		close(n.done)
	}
	n.mu.Unlock()

	select {
	case <-n.finished:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("Telegram: niet alle meldingen verstuurd: %w", ctx.Err())
	}
}

func (n *Notifier) run() {
	defer close(n.finished)
	for {
		var batch []string
		select {
		case msg := <-n.queue:
			batch = append(batch, msg)
		case <-n.done:
			n.flush(n.drain(nil))
			return
		}

		timer := time.NewTimer(n.debounce)
	collect:
		for {
			select {
			case msg := <-n.queue:
				batch = append(batch, msg)
			case <-timer.C:
				break collect
			case <-n.done:
				timer.Stop()
				n.flush(n.drain(batch))
				return
			}
		}
		n.flush(batch)
	}
}

// drain voegt alles wat nog in de wachtrij staat toe aan batch.
func (n *Notifier) drain(batch []string) []string {
	for {
		select {
		case msg := <-n.queue:
			batch = append(batch, msg)
		default:
			return batch
		}
	}
}

func (n *Notifier) flush(batch []string) {
	if len(batch) == 0 {
		return
	}
	for _, text := range split(n.header, batch) {
		if err := n.send(text); err != nil {
			n.log.Error("Telegram: bericht niet verstuurd", "fout", err)
		}
	}
}

// split verdeelt de meldingen over berichten van maximaal maxMessageLength tekens, met de header
// bovenaan elk bericht. Een te lange melding wordt afgekapt.
func split(header string, batch []string) []string {
	var out []string
	var cur strings.Builder
	start := func() {
		cur.Reset()
		if header != "" {
			cur.WriteString(header)
		}
	}
	start()
	for _, msg := range batch {
		if limit := maxMessageLength - len(header) - 2; len(msg) > limit {
			msg = msg[:limit-3] + "..."
		}
		if cur.Len()+2+len(msg) > maxMessageLength && cur.Len() > len(header) {
			out = append(out, cur.String())
			start()
		}
		if cur.Len() > 0 {
			cur.WriteString("\n\n")
		}
		cur.WriteString(msg)
	}
	return append(out, cur.String())
}

type sendRequest struct {
	ChatID                string `json:"chat_id"`
	Text                  string `json:"text"`
	DisableWebPagePreview bool   `json:"disable_web_page_preview"`
}

type sendResponse struct {
	OK          bool   `json:"ok"`
	Description string `json:"description"`
	Parameters  struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

// send verstuurt één bericht, met herhalingen bij netwerkfouten, 429 (met de opgegeven wachttijd) en 5xx.
func (n *Notifier) send(text string) error {
	body, err := json.Marshal(sendRequest{ChatID: n.chatID, Text: text, DisableWebPagePreview: true})
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/bot%s/sendMessage", n.baseURL, n.token)

	wait := n.backoff
	var lastErr error
	for attempt := 0; attempt <= n.retries; attempt++ {
		if attempt > 0 {
			time.Sleep(wait)
			wait *= 2
		}
		resp, err := n.http.Post(url, "application/json", bytes.NewReader(body))
		if err != nil {
			// de URL bevat het token: niet in de foutmelding opnemen
			lastErr = fmt.Errorf("versturen mislukt (netwerk)")
			continue
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()

		var r sendResponse
		_ = json.Unmarshal(data, &r)
		switch {
		case resp.StatusCode == http.StatusOK && r.OK:
			return nil
		case resp.StatusCode == http.StatusTooManyRequests:
			lastErr = fmt.Errorf("te veel berichten (429)")
			if r.Parameters.RetryAfter > 0 {
				wait = time.Duration(min(r.Parameters.RetryAfter, 60)) * time.Second
			}
		case resp.StatusCode >= 500:
			lastErr = fmt.Errorf("serverfout %d", resp.StatusCode)
		default:
			return fmt.Errorf("geweigerd (%d): %s", resp.StatusCode, r.Description)
		}
	}
	return fmt.Errorf("na %d pogingen: %w", n.retries+1, lastErr)
}
