// Package notify delivers anomaly episodes to people: generic webhooks
// (HMAC-signed), Slack, Microsoft Teams (Workflows), Discord, Telegram and
// email. Delivery is asynchronous and bounded, so a slow or dead endpoint
// never delays ingestion; floods are contained by a per-source cooldown and
// a global rate limit.
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/smtp"
	"net/textproto"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/williamtatendajose/prediction/iot-hub/internal/anomaly"
)

// Target is one destination, configured as "kind=URL":
//
//	webhook=https://example.com/hook      JSON event, X-IoTHub-Signature: sha256=<hmac>
//	slack=https://hooks.slack.com/services/…
//	teams=https://….logic.azure.com/…      (Teams "Workflows" webhook)
//	discord=https://discord.com/api/webhooks/…
//	telegram=https://api.telegram.org/bot<token>/sendMessage?chat_id=<id>
//	email=smtp://user:pass@smtp.example.com:587?from=hub@x.com&to=a@x.com,b@x.com
type Target struct {
	Kind string
	URL  *url.URL
	raw  string

	Sent   atomic.Uint64
	Failed atomic.Uint64
	last   atomic.Value // string: last error
}

var kinds = map[string]bool{"webhook": true, "slack": true, "teams": true, "discord": true, "telegram": true, "email": true}

func ParseTarget(spec string) (*Target, error) {
	kind, raw, ok := strings.Cut(strings.TrimSpace(spec), "=")
	if !ok || !kinds[kind] {
		return nil, fmt.Errorf("notify target %q: want kind=URL with kind one of webhook, slack, teams, discord, telegram, email", redactSpec(spec))
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("notify target %s: %w", kind, err)
	}
	switch kind {
	case "email":
		if u.Scheme != "smtp" && u.Scheme != "smtps" || u.Host == "" || u.Query().Get("from") == "" || u.Query().Get("to") == "" {
			return nil, fmt.Errorf("notify email: want smtp://user:pass@host:587?from=...&to=a,b")
		}
	default:
		if u.Scheme != "https" && u.Scheme != "http" || u.Host == "" {
			return nil, fmt.Errorf("notify %s: URL must be http(s)", kind)
		}
	}
	return &Target{Kind: kind, URL: u, raw: raw}, nil
}

// Redacted hides credentials and tokens for display. Chat webhooks carry
// their secret in the path (Slack, Discord, Telegram bot token), so only
// scheme and host are shown; email shows server, sender and recipients.
func (t *Target) Redacted() string {
	if t.Kind != "email" {
		return t.Kind + "=" + t.URL.Scheme + "://" + t.URL.Host + "/…"
	}
	q := t.URL.Query()
	return fmt.Sprintf("email=%s://%s?from=%s&to=%s", t.URL.Scheme, t.URL.Host, q.Get("from"), q.Get("to"))
}

func redactSpec(s string) string {
	if len(s) > 16 {
		return s[:16] + "…"
	}
	return s
}

type Config struct {
	Targets []*Target
	// Secret signs generic webhook bodies (HMAC-SHA256).
	Secret string
	// Kinds limits which anomaly kinds notify (empty = all).
	Kinds map[string]bool
	// Resolved also sends a message when an episode closes (only for
	// episodes whose opening was sent).
	Resolved bool
	// Cooldown: a new episode for the same sensor/field/kind within this
	// window is counted but not sent (default 10m).
	Cooldown time.Duration
	// PerMinute caps total messages per target (default 20).
	PerMinute int
	// BaseURL, if set, adds a dashboard link to messages.
	BaseURL string
	// Names maps sensor id to display name (optional).
	Names func(sensor string) string
	Logf  func(string, ...any)
}

type Notifier struct {
	cfg    Config
	queue  chan job
	client *http.Client

	mu       sync.Mutex
	lastSent map[string]time.Time // cooldown key -> last open sent
	held     map[string]int       // cooldown key -> suppressed since
	opened   map[string]bool      // episode ids whose open was sent
	tokens   float64
	refillAt time.Time

	Suppressed atomic.Uint64
	Dropped    atomic.Uint64
	done       chan struct{}
}

type job struct {
	msg Message
	t   *Target // nil = all targets
}

// Message is the rendered notification, shared by all formats.
type Message struct {
	Event    anomaly.Event `json:"event"`
	Status   string        `json:"status"` // open | resolved | test
	Title    string        `json:"title"`
	Text     string        `json:"text"`
	Sensor   string        `json:"sensorName"`
	Link     string        `json:"link,omitempty"`
	Held     int           `json:"suppressedSimilar,omitempty"`
	Severity string        `json:"severity"`
	// Reports: HTML for email, the structured report for webhooks.
	HTML   string `json:"-"`
	Report any    `json:"report,omitempty"`
}

// SendReport delivers a report to every target now (one retry on transient
// failure) and returns the outcome per target. Email gets HTML with a
// plain-text alternative; chat targets get the text, clipped to their limits;
// webhooks get the structured report.
func (n *Notifier) SendReport(ctx context.Context, title, text, html string, data any) map[string]string {
	m := Message{Status: "report", Title: title, Text: text, HTML: html, Report: data, Severity: "info"}
	out := map[string]string{}
	for _, t := range n.cfg.Targets {
		err := n.deliver(ctx, t, m)
		if err != nil && !errors.Is(err, errPermanent) {
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
				err = n.deliver(ctx, t, m)
			}
		}
		if err != nil {
			t.Failed.Add(1)
			t.last.Store(err.Error())
			out[t.Redacted()] = err.Error()
			continue
		}
		t.Sent.Add(1)
		out[t.Redacted()] = "ok"
	}
	return out
}

// clip shortens chat text to a service's message limit.
func clip(s string, n int) string {
	const more = "\n… (shortened; full report by email or on the dashboard)"
	if len(s) <= n {
		return s
	}
	cut := s[:n-len(more)]
	for len(cut) > 0 && !utf8.RuneStart(s[len(cut)]) { // don't split a character
		cut = cut[:len(cut)-1]
	}
	if i := strings.LastIndex(cut, "\n"); i > n/2 {
		cut = cut[:i]
	}
	return cut + more
}

func New(cfg Config) *Notifier {
	if cfg.Cooldown == 0 {
		cfg.Cooldown = 10 * time.Minute
	}
	if cfg.PerMinute <= 0 {
		cfg.PerMinute = 20
	}
	return &Notifier{
		cfg: cfg, queue: make(chan job, 256), client: &http.Client{Timeout: 10 * time.Second},
		lastSent: map[string]time.Time{}, held: map[string]int{}, opened: map[string]bool{},
		tokens: float64(cfg.PerMinute), refillAt: time.Now(), done: make(chan struct{}),
	}
}

func (n *Notifier) Targets() []*Target { return n.cfg.Targets }

// Notify is called for every episode open/close. It never blocks.
func (n *Notifier) Notify(e anomaly.Event) {
	if len(n.cfg.Targets) == 0 || e.Shelved || (len(n.cfg.Kinds) > 0 && !n.cfg.Kinds[e.Kind]) {
		return // shelved: an operator has taken this alarm out of service
	}
	key := e.Sensor + "\x00" + e.Field + "\x00" + e.Kind
	now := time.Now()
	n.mu.Lock()
	status := "open"
	held := 0
	if e.End != 0 {
		status = "resolved"
		sent := n.opened[e.ID]
		delete(n.opened, e.ID)
		if !n.cfg.Resolved || !sent {
			n.mu.Unlock()
			return
		}
	} else {
		if last, ok := n.lastSent[key]; ok && now.Sub(last) < n.cfg.Cooldown {
			n.held[key]++
			n.mu.Unlock()
			n.Suppressed.Add(1)
			return
		}
		if !n.take(now) {
			n.mu.Unlock()
			n.Suppressed.Add(1)
			return
		}
		n.lastSent[key] = now
		held = n.held[key]
		delete(n.held, key)
		n.opened[e.ID] = true
		if len(n.opened) > 10000 { // bound memory if closes are never seen
			n.opened = map[string]bool{e.ID: true}
		}
	}
	n.mu.Unlock()
	n.enqueue(job{msg: n.render(e, status, held)})
}

// take implements a token bucket refilled at PerMinute per minute.
func (n *Notifier) take(now time.Time) bool {
	n.tokens += now.Sub(n.refillAt).Minutes() * float64(n.cfg.PerMinute)
	n.refillAt = now
	if n.tokens > float64(n.cfg.PerMinute) {
		n.tokens = float64(n.cfg.PerMinute)
	}
	if n.tokens < 1 {
		return false
	}
	n.tokens--
	return true
}

func (n *Notifier) enqueue(j job) {
	select {
	case n.queue <- j:
	default:
		n.Dropped.Add(1)
	}
}

// Test sends a test message to one target synchronously.
func (n *Notifier) Test(ctx context.Context, t *Target) error {
	m := n.render(anomaly.Event{ID: "test", Sensor: "test", Kind: "test", Start: time.Now().UnixMilli(),
		Message: "This is a test notification from the IoT hub."}, "test", 0)
	return n.deliver(ctx, t, m)
}

// severity: a limit breach is critical; spikes and silence are warnings.
func severity(e anomaly.Event) string {
	if e.Kind == "range" {
		return "critical"
	}
	return "warning"
}

func (n *Notifier) render(e anomaly.Event, status string, held int) Message {
	name := e.Sensor
	if n.cfg.Names != nil {
		if s := n.cfg.Names(e.Sensor); s != "" {
			name = s
		}
	}
	where := name
	if e.Field != "" {
		where += " · " + e.Field
	}
	m := Message{Event: e, Status: status, Sensor: name, Held: held, Severity: severity(e)}
	switch status {
	case "resolved":
		m.Title = "Resolved: " + e.Kind + " on " + where
		m.Text = fmt.Sprintf("%s — back to normal after %s.", e.Message, (time.Duration(e.End-e.Start) * time.Millisecond).Round(time.Second))
	case "test":
		m.Title = "IoT hub test notification"
		m.Text = e.Message
	default:
		m.Title = "Anomaly: " + e.Kind + " on " + where
		m.Text = e.Message + " (" + time.UnixMilli(e.Start).UTC().Format("2006-01-02 15:04:05 UTC") + ")"
		if held > 0 {
			m.Text += fmt.Sprintf(". %d similar alert(s) were held back by the cooldown.", held)
		}
	}
	if n.cfg.BaseURL != "" {
		m.Link = strings.TrimRight(n.cfg.BaseURL, "/") + "/"
	}
	return m
}

// Run delivers queued messages until ctx ends, then drains for up to 5 s.
func (n *Notifier) Run(ctx context.Context) {
	defer close(n.done)
	for {
		select {
		case j := <-n.queue:
			n.send(ctx, j)
		case <-ctx.Done():
			drain, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			for {
				select {
				case j := <-n.queue:
					n.send(drain, j)
				default:
					return
				}
			}
		}
	}
}

func (n *Notifier) Done() <-chan struct{} { return n.done }

func (n *Notifier) send(ctx context.Context, j job) {
	targets := n.cfg.Targets
	if j.t != nil {
		targets = []*Target{j.t}
	}
	for _, t := range targets {
		var err error
		// Retry transient failures: 1 s, 4 s.
		for attempt, wait := 0, time.Second; attempt < 3; attempt, wait = attempt+1, wait*4 {
			if err = n.deliver(ctx, t, j.msg); err == nil || errors.Is(err, errPermanent) {
				break
			}
			select {
			case <-ctx.Done():
				attempt = 3
			case <-time.After(wait):
			}
		}
		if err != nil {
			t.Failed.Add(1)
			t.last.Store(err.Error())
			if n.cfg.Logf != nil {
				n.cfg.Logf("notify %s: %v", t.Kind, err)
			}
		} else {
			t.Sent.Add(1)
		}
	}
}

func (t *Target) LastError() string {
	s, _ := t.last.Load().(string)
	return s
}

var errPermanent = errors.New("permanent failure")

func (n *Notifier) deliver(ctx context.Context, t *Target, m Message) error {
	if t.Kind == "email" {
		return sendEmail(t.URL, m)
	}
	body, err := payload(t.Kind, t.URL, m)
	if err != nil {
		return err
	}
	u := *t.URL
	if t.Kind == "telegram" { // chat_id goes in the body, not the URL
		q := u.Query()
		q.Del("chat_id")
		u.RawQuery = q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "iothub-notify/1")
	if t.Kind == "webhook" {
		req.Header.Set("X-IoTHub-Event", m.Status)
		req.Header.Set("X-IoTHub-Event-Id", m.Event.ID)
		if n.cfg.Secret != "" {
			mac := hmac.New(sha256.New, []byte(n.cfg.Secret))
			mac.Write(body)
			req.Header.Set("X-IoTHub-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		}
	}
	res, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	snippet, _ := io.ReadAll(io.LimitReader(res.Body, 300))
	switch {
	case res.StatusCode < 300:
		return nil
	case res.StatusCode == 429 || res.StatusCode >= 500:
		return fmt.Errorf("HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(snippet)))
	default: // 4xx: wrong URL or payload, retrying will not help
		return fmt.Errorf("%w: HTTP %d: %s", errPermanent, res.StatusCode, strings.TrimSpace(string(snippet)))
	}
}

func payload(kind string, u *url.URL, m Message) ([]byte, error) {
	line := m.Title + "\n" + m.Text
	if m.Link != "" {
		line += "\n" + m.Link
	}
	switch kind {
	case "slack":
		return json.Marshal(map[string]string{"text": clip("*"+m.Title+"*\n"+m.Text+linkSuffix(m.Link), 3900)})
	case "discord":
		return json.Marshal(map[string]string{"content": clip("**"+m.Title+"**\n"+m.Text+linkSuffix(m.Link), 2000)})
	case "telegram":
		return json.Marshal(map[string]string{"chat_id": u.Query().Get("chat_id"), "text": clip(line, 4096)})
	case "teams":
		// Teams Workflows webhooks take a message with an Adaptive Card.
		color := "Attention"
		switch m.Status {
		case "resolved":
			color = "Good"
		case "report":
			color = "Default"
		}
		card := map[string]any{
			"$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
			"type":    "AdaptiveCard", "version": "1.4",
			"body": []any{
				map[string]any{"type": "TextBlock", "text": m.Title, "weight": "Bolder", "size": "Medium", "color": color, "wrap": true},
				map[string]any{"type": "TextBlock", "text": m.Text, "wrap": true},
			},
		}
		if m.Link != "" {
			card["actions"] = []any{map[string]any{"type": "Action.OpenUrl", "title": "Open dashboard", "url": m.Link}}
		}
		return json.Marshal(map[string]any{"type": "message", "attachments": []any{
			map[string]any{"contentType": "application/vnd.microsoft.card.adaptive", "content": card},
		}})
	default: // webhook
		return json.Marshal(m)
	}
}

func linkSuffix(l string) string {
	if l == "" {
		return ""
	}
	return "\n" + l
}

// sendEmail uses STARTTLS on smtp:// (port 587) and implicit TLS on smtps://
// (port 465); credentials are only ever sent over TLS.
func sendEmail(u *url.URL, m Message) error {
	q := u.Query()
	from := q.Get("from")
	to := strings.Split(q.Get("to"), ",")
	host := u.Hostname()
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMIME-Version: 1.0\r\n",
		from, strings.Join(to, ", "), mime.QEncoding.Encode("utf-8", m.Title), time.Now().Format(time.RFC1123Z))
	text := m.Text
	if m.Link != "" && !strings.Contains(text, m.Link) {
		text += "\n" + m.Link
	}
	if m.HTML == "" {
		b.WriteString("Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
		writeQP(&b, text)
	} else {
		// multipart/alternative: HTML for mail clients, plain text for the rest.
		mw := multipart.NewWriter(&b)
		fmt.Fprintf(&b, "Content-Type: multipart/alternative; boundary=%s\r\n\r\n", mw.Boundary())
		for _, part := range []struct{ typ, body string }{{"text/plain", text}, {"text/html", m.HTML}} {
			w, err := mw.CreatePart(textproto.MIMEHeader{
				"Content-Type":              {part.typ + "; charset=utf-8"},
				"Content-Transfer-Encoding": {"quoted-printable"},
			})
			if err != nil {
				return err
			}
			var pb strings.Builder
			writeQP(&pb, part.body)
			io.WriteString(w, pb.String())
		}
		mw.Close()
	}
	var auth smtp.Auth
	if u.User != nil {
		pass, _ := u.User.Password()
		auth = smtp.PlainAuth("", u.User.Username(), pass, host) // refuses to send over plain text
	}
	return smtpSend(u.Scheme == "smtps", u.Host, host, auth, from, to, []byte(b.String()))
}

// writeQP encodes with quoted-printable: keeps SMTP lines under 998 bytes
// and survives 7-bit relays.
func writeQP(b *strings.Builder, s string) {
	w := quotedprintable.NewWriter(b)
	io.WriteString(w, strings.ReplaceAll(s, "\n", "\r\n"))
	w.Close()
	b.WriteString("\r\n")
}

// smtpSend is net/smtp.SendMail with a deadline (a hung server must not
// block the notifier) and implicit TLS support.
func smtpSend(implicitTLS bool, addr, host string, auth smtp.Auth, from string, to []string, msg []byte) error {
	d := net.Dialer{Timeout: 15 * time.Second}
	var conn net.Conn
	var err error
	tlsCfg := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	if implicitTLS {
		conn, err = tls.DialWithDialer(&d, "tcp", addr, tlsCfg)
	} else {
		conn, err = d.Dial("tcp", addr)
	}
	if err != nil {
		return err
	}
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if !implicitTLS {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(tlsCfg); err != nil {
				return err
			}
		}
	}
	if auth != nil {
		if err := c.Auth(auth); err != nil {
			return err
		}
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	for _, r := range to {
		if err := c.Rcpt(strings.TrimSpace(r)); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}
