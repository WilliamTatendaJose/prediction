package notify

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/williamtatendajose/prediction/iot-hub/internal/anomaly"
)

func ev(id, sensor, kind string, end int64) anomaly.Event {
	return anomaly.Event{ID: id, Sensor: sensor, Field: "t", Kind: kind, Start: 1000, End: end, Value: 9, Message: "t 9 is above limit 5"}
}

func TestParseTarget(t *testing.T) {
	good := []string{
		"webhook=https://example.com/hook",
		"slack=https://hooks.slack.com/services/T/B/SECRET",
		"telegram=https://api.telegram.org/bot123:ABC/sendMessage?chat_id=42",
		"email=smtp://u:p@smtp.example.com:587?from=a@x.com&to=b@x.com,c@x.com",
	}
	for _, g := range good {
		tg, err := ParseTarget(g)
		if err != nil {
			t.Fatalf("%s: %v", g, err)
		}
		r := tg.Redacted()
		for _, secret := range []string{"SECRET", "123:ABC", ":p@", "chat_id=42"} {
			if strings.Contains(r, secret) {
				t.Errorf("redacted %q leaks %q", r, secret)
			}
		}
	}
	for _, b := range []string{"pager=https://x", "slack", "slack=ftp://x", "email=smtp://h:25?to=a", "webhook=not a url"} {
		if _, err := ParseTarget(b); err == nil {
			t.Errorf("%q should be rejected", b)
		}
	}
}

type capture struct {
	mu     sync.Mutex
	bodies [][]byte
	hdrs   []http.Header
	codes  []int // responses to return in order, then 200
	calls  atomic.Int64
}

func (c *capture) server(t *testing.T) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.bodies = append(c.bodies, b)
		c.hdrs = append(c.hdrs, r.Header.Clone())
		code := 200
		if n := int(c.calls.Add(1)) - 1; n < len(c.codes) {
			code = c.codes[n]
		}
		c.mu.Unlock()
		w.WriteHeader(code)
	}))
	t.Cleanup(s.Close)
	return s
}

func run(t *testing.T, n *Notifier) func() {
	ctx, cancel := context.WithCancel(context.Background())
	go n.Run(ctx)
	return func() { cancel(); <-n.Done() }
}

func TestWebhookSignedAndRetried(t *testing.T) {
	c := &capture{codes: []int{503}}
	s := c.server(t)
	tg, _ := ParseTarget("webhook=" + s.URL)
	n := New(Config{Targets: []*Target{tg}, Secret: "k", Resolved: true})
	stop := run(t, n)
	n.Notify(ev("e1", "tank", "range", 0))
	n.Notify(ev("e1", "tank", "range", 5000))
	time.Sleep(1500 * time.Millisecond) // one retry after 1 s
	stop()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.bodies) != 3 || tg.Sent.Load() != 2 {
		t.Fatalf("calls %d sent %d (want 503 retry + open + resolved)", len(c.bodies), tg.Sent.Load())
	}
	for i, b := range c.bodies {
		mac := hmac.New(sha256.New, []byte("k"))
		mac.Write(b)
		if c.hdrs[i].Get("X-IoTHub-Signature") != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
			t.Fatal("bad signature")
		}
	}
	var m Message
	json.Unmarshal(c.bodies[2], &m)
	if m.Status != "resolved" || m.Event.ID != "e1" || !strings.Contains(m.Text, "back to normal after 4s") {
		t.Fatalf("resolved message %+v", m)
	}
}

func TestNoRetryOn4xx(t *testing.T) {
	c := &capture{codes: []int{404, 404, 404}}
	s := c.server(t)
	tg, _ := ParseTarget("slack=" + s.URL)
	n := New(Config{Targets: []*Target{tg}})
	stop := run(t, n)
	n.Notify(ev("e1", "tank", "range", 0))
	time.Sleep(200 * time.Millisecond)
	stop()
	if c.calls.Load() != 1 || tg.Failed.Load() != 1 || !strings.Contains(tg.LastError(), "404") {
		t.Fatalf("calls %d failed %d err %q", c.calls.Load(), tg.Failed.Load(), tg.LastError())
	}
}

func TestCooldownRateLimitAndResolvedOnlyIfSent(t *testing.T) {
	c := &capture{}
	s := c.server(t)
	tg, _ := ParseTarget("webhook=" + s.URL)
	n := New(Config{Targets: []*Target{tg}, Cooldown: 300 * time.Millisecond, PerMinute: 3, Resolved: true})
	stop := run(t, n)
	n.Notify(ev("a1", "tank", "range", 0)) // sent
	n.Notify(ev("a2", "tank", "range", 0)) // same key within cooldown: held
	n.Notify(ev("a2", "tank", "range", 9)) // its close: not sent (open was not)
	n.Notify(ev("b1", "pump", "stale", 0)) // other key: sent
	time.Sleep(350 * time.Millisecond)
	n.Notify(ev("a3", "tank", "range", 0)) // after cooldown: sent, mentions 1 held
	n.Notify(ev("c1", "line", "spike", 0)) // 4th open in a minute: rate limited
	time.Sleep(200 * time.Millisecond)
	stop()
	c.mu.Lock()
	defer c.mu.Unlock()
	var ids []string
	for _, b := range c.bodies {
		var m Message
		json.Unmarshal(b, &m)
		ids = append(ids, m.Event.ID+"/"+m.Status)
		if m.Event.ID == "a3" && m.Held != 1 {
			t.Errorf("a3 should report 1 held alert, got %d", m.Held)
		}
	}
	if strings.Join(ids, ",") != "a1/open,b1/open,a3/open" {
		t.Fatalf("sent %v", ids)
	}
	if n.Suppressed.Load() != 2 {
		t.Fatalf("suppressed %d", n.Suppressed.Load())
	}
}

func TestFormats(t *testing.T) {
	u := func(s string) *Target { tg, _ := ParseTarget(s); return tg }
	m := New(Config{BaseURL: "https://hub:8443"}).render(ev("e", "tank", "range", 0), "open", 0)
	for kind, check := range map[string]func(map[string]any) bool{
		"slack": func(p map[string]any) bool {
			return strings.HasPrefix(p["text"].(string), "*Anomaly: range on tank · t*")
		},
		"discord": func(p map[string]any) bool { return strings.Contains(p["content"].(string), "https://hub:8443/") },
		"telegram": func(p map[string]any) bool {
			return p["chat_id"] == "42" && strings.Contains(p["text"].(string), "limit 5")
		},
		"teams": func(p map[string]any) bool {
			a := p["attachments"].([]any)[0].(map[string]any)
			return p["type"] == "message" && a["contentType"] == "application/vnd.microsoft.card.adaptive"
		},
	} {
		tg := u(kind + "=https://x.test/p?chat_id=42")
		b, err := payload(kind, tg.URL, m)
		if err != nil {
			t.Fatal(err)
		}
		var p map[string]any
		json.Unmarshal(b, &p)
		if !check(p) {
			t.Errorf("%s payload: %s", kind, b)
		}
	}
}

// fakeSMTP accepts one message and records it.
func fakeSMTP(t *testing.T) (addr string, got chan string) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	got = make(chan string, 1)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		say := func(s string) { conn.Write([]byte(s + "\r\n")) }
		say("220 fake")
		var data strings.Builder
		var auth string
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			cmd := strings.ToUpper(strings.TrimSpace(line))
			switch {
			case strings.HasPrefix(cmd, "EHLO"):
				say("250-fake")
				say("250 AUTH PLAIN")
			case strings.HasPrefix(cmd, "AUTH"):
				auth = strings.TrimSpace(line)
				say("235 ok")
			case strings.HasPrefix(cmd, "MAIL"), strings.HasPrefix(cmd, "RCPT"):
				data.WriteString(strings.TrimSpace(line) + "\n")
				say("250 ok")
			case cmd == "DATA":
				say("354 go")
				for {
					l, _ := r.ReadString('\n')
					if l == ".\r\n" {
						break
					}
					data.WriteString(l)
				}
				say("250 queued")
			case cmd == "QUIT":
				say("221 bye")
				got <- auth + "\n" + data.String()
				return
			default:
				say("250 ok")
			}
		}
	}()
	return l.Addr().String(), got
}

func TestEmail(t *testing.T) {
	addr, got := fakeSMTP(t)
	tg, err := ParseTarget("email=smtp://ops:pw@" + addr + "?from=hub@plant.test&to=a@plant.test,b@plant.test")
	if err != nil {
		t.Fatal(err)
	}
	// net/smtp only sends PLAIN credentials over TLS or to localhost; the
	// fake server is on 127.0.0.1, so authentication is exercised here.
	m := New(Config{}).render(ev("e", "tank", "range", 0), "open", 0)
	m.Title = "Anomaly: tank — 25 °C"
	if err := sendEmail(tg.URL, m); err != nil {
		t.Fatal(err)
	}
	select {
	case s := <-got:
		for _, want := range []string{"AUTH PLAIN", "MAIL FROM:<hub@plant.test>", "RCPT TO:<a@plant.test>", "RCPT TO:<b@plant.test>",
			"Subject: =?utf-8?q?", "t 9 is above limit 5"} {
			if !strings.Contains(s, want) {
				t.Errorf("email missing %q in:\n%s", want, s)
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no email")
	}
}
