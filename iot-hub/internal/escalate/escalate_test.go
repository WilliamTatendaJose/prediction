package escalate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/williamtatendajose/prediction/iot-hub/internal/anomaly"
	"github.com/williamtatendajose/prediction/iot-hub/internal/notify"
)

type sink struct {
	mu   sync.Mutex
	got  []string
	srv  *httptest.Server
	tgt  *notify.Target
	name string
}

func newSink(t *testing.T, name string) *sink {
	s := &sink{name: name}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m struct{ Title string }
		json.NewDecoder(r.Body).Decode(&m)
		s.mu.Lock()
		s.got = append(s.got, m.Title)
		s.mu.Unlock()
	}))
	t.Cleanup(s.srv.Close)
	s.tgt, _ = notify.ParseTarget("webhook=" + s.srv.URL)
	return s
}

func (s *sink) n() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.got) }

func TestEscalationLevels(t *testing.T) {
	sup, mgr := newSink(t, "supervisor"), newSink(t, "manager")
	start := time.Date(2026, 10, 2, 6, 0, 0, 0, time.UTC)
	ev := anomaly.Event{ID: "e1", Sensor: "tank", Field: "level", Kind: "range", Start: start.UnixMilli(), Message: "level 97 is above limit 90"}
	events := []anomaly.Event{ev}
	var mu sync.Mutex
	e := New(func() []anomaly.Event { mu.Lock(); defer mu.Unlock(); return append([]anomaly.Event(nil), events...) })
	now := start
	e.now = func() time.Time { return now }
	e.SetPolicy(Policy{Repeat: 30 * time.Minute, Sender: notify.New(notify.Config{}),
		Levels: []Level{{After: 60 * time.Minute, Targets: []*notify.Target{mgr.tgt}}, {After: 15 * time.Minute, Targets: []*notify.Target{sup.tgt}}}})
	at := func(min int) { now = start.Add(time.Duration(min) * time.Minute); e.Tick(context.Background()) }

	at(10)
	if sup.n()+mgr.n() != 0 {
		t.Fatal("escalated before 15 min")
	}
	at(16)
	at(20)
	if sup.n() != 1 || mgr.n() != 0 {
		t.Fatalf("level 1: supervisor %d manager %d", sup.n(), mgr.n())
	}
	at(61)
	if sup.n() != 1 || mgr.n() != 1 {
		t.Fatalf("level 2: supervisor %d manager %d", sup.n(), mgr.n())
	}
	at(80)
	at(92) // 31 min after level 2: repeat to level 2's targets
	if mgr.n() != 2 {
		t.Fatalf("repeat: manager %d", mgr.n())
	}
	if got := sup.got[0]; got != "Escalation 1 — not acknowledged: range on tank · level" {
		t.Errorf("title %q", got)
	}

	// Acknowledged: it stops.
	mu.Lock()
	events[0].Ack = &anomaly.Ack{By: "op"}
	mu.Unlock()
	at(200)
	if mgr.n() != 2 || len(e.state) != 0 {
		t.Fatalf("after ack: manager %d state %d", mgr.n(), len(e.state))
	}

	// After a restart with an alarm already 2 h old: one message, the
	// highest due level, not a burst of every level.
	e2 := New(func() []anomaly.Event {
		return []anomaly.Event{{ID: "e2", Sensor: "pump", Kind: "stale", Start: start.UnixMilli()}}
	})
	e2.now = func() time.Time { return start.Add(2 * time.Hour) }
	e2.SetPolicy(Policy{Sender: notify.New(notify.Config{}),
		Levels: []Level{{After: 15 * time.Minute, Targets: []*notify.Target{sup.tgt}}, {After: 60 * time.Minute, Targets: []*notify.Target{mgr.tgt}}}})
	e2.Tick(context.Background())
	if sup.n() != 1 || mgr.n() != 3 {
		t.Fatalf("restart: supervisor %d manager %d", sup.n(), mgr.n())
	}

	// Shelved alarms never escalate.
	e3 := New(func() []anomaly.Event { return []anomaly.Event{{ID: "e3", Start: start.UnixMilli(), Shelved: true}} })
	e3.now = func() time.Time { return start.Add(5 * time.Hour) }
	e3.SetPolicy(Policy{Sender: notify.New(notify.Config{}), Levels: []Level{{After: time.Minute, Targets: []*notify.Target{sup.tgt}}}})
	e3.Tick(context.Background())
	if sup.n() != 1 {
		t.Fatal("shelved alarm escalated")
	}
}
