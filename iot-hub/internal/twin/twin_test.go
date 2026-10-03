package twin

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDevice records what the hub sends and can answer like a device.
type fakeDevice struct {
	mu        sync.Mutex
	listening bool
	sent      []string // sub + " " + payload
	onMethod  func(sub string, payload []byte)
}

func (d *fakeDevice) transport() Transport {
	return Transport{
		Listening: func(id, sub string) bool { d.mu.Lock(); defer d.mu.Unlock(); return d.listening && id == "pump-1" },
		Connected: func(id string) bool { d.mu.Lock(); defer d.mu.Unlock(); return d.listening && id == "pump-1" },
		Send: func(id, sub string, p []byte) int {
			d.mu.Lock()
			if !d.listening || id != "pump-1" {
				d.mu.Unlock()
				return 0
			}
			d.sent = append(d.sent, sub+" "+string(p))
			f := d.onMethod
			d.mu.Unlock()
			if f != nil && strings.HasPrefix(sub, "methods/") {
				go f(sub, p)
			}
			return 1
		},
	}
}

func (d *fakeDevice) got() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.sent...)
}

func newSvc(t *testing.T, path string) (*Service, *fakeDevice) {
	s := New(path, func(id string) bool { return id == "pump-1" || id == "pump-2" })
	d := &fakeDevice{}
	s.SetTransport(d.transport())
	return s, d
}

func TestTwinPatchesAndVersions(t *testing.T) {
	s, d := newSvc(t, "")
	d.listening = true
	if _, err := s.Get("ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown device: %v", err)
	}
	v, _ := s.Get("pump-1")
	etag := v.ETag
	v, err := s.Update("pump-1", etag, []byte(`{"tags":{"site":"plant1","line":3},"properties":{"desired":{"interval":10,"thresholds":{"high":90,"low":10}}}}`))
	if err != nil || v.Properties.Desired["$version"] != int64(1) || v.Tags["site"] != "plant1" {
		t.Fatalf("%+v %v", v, err)
	}
	// A stale etag is refused; nothing changes.
	if _, err := s.Update("pump-1", etag, []byte(`{"tags":{"site":"x"}}`)); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("stale etag: %v", err)
	}
	// Merge patch: null deletes, nested objects merge.
	v, _ = s.Update("pump-1", v.ETag, []byte(`{"properties":{"desired":{"interval":null,"thresholds":{"high":95}}}}`))
	th := v.Properties.Desired["thresholds"].(map[string]any)
	if _, ok := v.Properties.Desired["interval"]; ok || th["high"] != 95.0 || th["low"] != 10.0 || v.Properties.Desired["$version"] != int64(2) {
		t.Fatalf("merge: %+v", v.Properties.Desired)
	}
	// The device was sent each desired patch, nulls included.
	sent := d.got()
	if len(sent) != 2 || !strings.Contains(sent[1], `"interval":null`) || !strings.Contains(sent[1], `"$version":2`) || !strings.HasPrefix(sent[1], "twin/desired ") {
		t.Fatalf("pushed %v", sent)
	}
	// Tags-only changes are not pushed (tags are cloud-only).
	s.Update("pump-1", "", []byte(`{"tags":{"owner":"maint"}}`))
	if len(d.got()) != 2 {
		t.Fatal("tags pushed to the device")
	}
	// The device reports; it sees desired and reported, not tags.
	if ver, err := s.Report("pump-1", []byte(`{"firmware":"1.4.2","interval":10}`)); err != nil || ver != 1 {
		t.Fatalf("report %v %v", ver, err)
	}
	dv := s.DeviceView("pump-1")
	if _, ok := dv["tags"]; ok || dv["reported"].(map[string]any)["firmware"] != "1.4.2" {
		t.Fatalf("device view %v", dv)
	}
	v, _ = s.Get("pump-1")
	if v.Version != 5 || v.ConnectionState != "Connected" {
		t.Fatalf("version %d state %s", v.Version, v.ConnectionState)
	}
	// Validation.
	for name, body := range map[string]string{
		"reserved key":  `{"properties":{"desired":{"$version":5}}}`,
		"dotted key":    `{"tags":{"a.b":1}}`,
		"too deep":      `{"tags":{"a":{"b":{"c":{"d":{"e":{"f":1}}}}}}}`,
		"unknown field": `{"properties":{"reported":{"x":1}}}`,
		"object array":  `{"tags":{"a":[{"b":1}]}}`,
		"too big":       `{"properties":{"desired":{"blob":"` + strings.Repeat("x", MaxDesired) + `"}}}`,
	} {
		if _, err := s.Update("pump-1", "", []byte(body)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := s.Report("pump-1", []byte(`[1,2]`)); err == nil {
		t.Error("non-object reported accepted")
	}
}

func TestQueryAndBulkUpdate(t *testing.T) {
	s, d := newSvc(t, "")
	d.listening = true
	s.Update("pump-1", "", []byte(`{"tags":{"site":"plant1"}}`))
	s.Update("pump-2", "", []byte(`{"tags":{"site":"plant2"}}`))
	s.Report("pump-1", []byte(`{"firmware":"1.4"}`))
	ids := []string{"pump-1", "pump-2"}
	f, _ := ParseFilters([]string{"tags.site=plant1", "properties.reported.firmware=1.4"})
	if got := s.Query(ids, f); len(got) != 1 || got[0].DeviceID != "pump-1" {
		t.Fatalf("query %v", got)
	}
	f, _ = ParseFilters([]string{"connectionState=Disconnected"})
	if got := s.Query(ids, f); len(got) != 1 || got[0].DeviceID != "pump-2" {
		t.Fatalf("connection state query %v", got)
	}
	f, _ = ParseFilters([]string{"tags.site=plant2"})
	changed, err := s.UpdateMany(ids, f, []byte(`{"properties":{"desired":{"interval":30}}}`))
	if err != nil || len(changed) != 1 || changed[0] != "pump-2" {
		t.Fatalf("bulk %v %v", changed, err)
	}
	if v, _ := s.Get("pump-1"); v.Properties.Desired["interval"] != nil {
		t.Fatal("bulk update touched a non-matching twin")
	}
}

func TestDirectMethods(t *testing.T) {
	s, d := newSvc(t, "")
	ctx := context.Background()
	if _, err := s.Invoke(ctx, "pump-1", "reboot", nil, time.Second); !errors.Is(err, ErrOffline) {
		t.Fatalf("offline: %v", err)
	}
	d.listening = true
	d.onMethod = func(sub string, p []byte) {
		parts := strings.Split(sub, "/") // methods/{name}/{rid}
		rid := parts[2]
		// Another device answering is ignored.
		s.HandleMQTT("pump-2", "methods/res/200/"+rid, []byte(`{"hijacked":true}`))
		if parts[1] == "slow" {
			return
		}
		if parts[1] == "text" {
			s.HandleMQTT("pump-1", "methods/res/200/"+rid, []byte(`done`))
			return
		}
		s.HandleMQTT("pump-1", "methods/res/202/"+rid, []byte(`{"rebooting_in":`+string(p)+`}`))
	}
	r, err := s.Invoke(ctx, "pump-1", "reboot", json.RawMessage(`5`), time.Second)
	if err != nil || r.Status != 202 || string(r.Payload) != `{"rebooting_in":5}` {
		t.Fatalf("invoke %+v %v", r, err)
	}
	r, err = s.Invoke(ctx, "pump-1", "text", nil, time.Second)
	if err != nil || string(r.Payload) != `"done"` {
		t.Fatalf("non-JSON answer %+v %v", r, err)
	}
	start := time.Now()
	if _, err := s.Invoke(ctx, "pump-1", "slow", nil, 200*time.Millisecond); !errors.Is(err, ErrTimeout) || time.Since(start) > time.Second {
		t.Fatalf("timeout: %v", err)
	}
	if _, err := s.Invoke(ctx, "pump-1", "bad/name", nil, time.Second); err == nil {
		t.Fatal("bad method name accepted")
	}
	if _, err := s.Invoke(ctx, "pump-1", "x", json.RawMessage(`{not json`), time.Second); err == nil {
		t.Fatal("bad payload accepted")
	}
	if len(s.pending) != 0 {
		t.Fatal("pending calls leak")
	}
}

func TestCloudToDeviceMessages(t *testing.T) {
	p := filepath.Join(t.TempDir(), "twins.json")
	s, d := newSvc(t, p)
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }

	// Offline: queued.
	m1, err := s.Send("pump-1", json.RawMessage(`{"cmd":"close-valve"}`), 0)
	if err != nil || m1.Status != "queued" || len(d.got()) != 0 {
		t.Fatalf("queued: %+v %v", m1, err)
	}
	m2, _ := s.Send("pump-1", json.RawMessage(`plain text`), 2*time.Minute)
	// The device subscribes: both delivered, in order.
	d.listening = true
	s.Listening("pump-1")
	got := d.got()
	if len(got) != 2 || got[0] != "messages/"+m1.ID+` {"cmd":"close-valve"}` || got[1] != "messages/"+m2.ID+` "plain text"` {
		t.Fatalf("delivered %v", got)
	}
	// It completes the first; the second is not settled and its lock runs
	// out: it is delivered again.
	s.HandleMQTT("pump-1", "messages/complete/"+m1.ID, nil)
	now = now.Add(30 * time.Second)
	s.Tick()
	if len(d.got()) != 2 {
		t.Fatal("redelivered while still locked")
	}
	now = now.Add(31 * time.Second)
	s.Tick()
	if got := d.got(); len(got) != 3 || !strings.HasPrefix(got[2], "messages/"+m2.ID) {
		t.Fatalf("redelivery %v", got)
	}
	// Expiry (m2 has a 2-minute TTL).
	now = now.Add(time.Minute)
	s.Tick()
	ms, _ := s.Messages("pump-1")
	st := map[string]string{}
	for _, m := range ms {
		st[m.ID] = m.Status
	}
	if st[m1.ID] != "completed" || st[m2.ID] != "expired" {
		t.Fatalf("statuses %v", st)
	}

	// Never completed: dead-lettered after MaxDeliveries.
	m3, _ := s.Send("pump-1", json.RawMessage(`1`), 2*time.Hour)
	for i := 0; i < MaxDeliveries+1; i++ {
		now = now.Add(LockTime + time.Second)
		s.Tick()
	}
	ms, _ = s.Messages("pump-1")
	if ms[0].ID != m3.ID || ms[0].Status != "deadlettered" || ms[0].Deliveries != MaxDeliveries {
		t.Fatalf("dead letter %+v", ms[0])
	}

	// HTTP devices poll: receive locks it, complete settles it.
	d.listening = false
	m4, _ := s.Send("pump-1", json.RawMessage(`{"cmd":"x"}`), 0)
	r, _ := s.Receive("pump-1")
	if r == nil || r.ID != m4.ID {
		t.Fatalf("receive %+v", r)
	}
	if r2, _ := s.Receive("pump-1"); r2 != nil {
		t.Fatal("locked message handed out twice")
	}
	if err := s.Settle("pump-1", m4.ID, "complete"); err != nil {
		t.Fatal(err)
	}
	if err := s.Settle("pump-1", m4.ID, "complete"); err == nil {
		t.Fatal("settled twice")
	}

	// The queue is bounded.
	for i := 0; i < MaxQueued; i++ {
		if _, err := s.Send("pump-1", json.RawMessage(`1`), 0); err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
	}
	if _, err := s.Send("pump-1", json.RawMessage(`1`), 0); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("queue full: %v", err)
	}
	if _, err := s.Send("pump-1", json.RawMessage(`1`), 72*time.Hour); err == nil {
		t.Fatal("ttl over 48h accepted")
	}

	// Saved and reloaded; a delivery lock does not survive a restart.
	s.Update("pump-1", "", []byte(`{"tags":{"site":"plant1"}}`))
	d.listening = true
	s.Listening("pump-1") // some now delivered (locked)
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	s2, _ := newSvc(t, p)
	if err := s2.Load(); err != nil {
		t.Fatal(err)
	}
	v, _ := s2.Get("pump-1")
	ms, _ = s2.Messages("pump-1")
	delivered := 0
	for _, m := range ms {
		if m.Status == "delivered" {
			delivered++
		}
	}
	if v.Tags["site"] != "plant1" || delivered != 0 || len(ms) == 0 {
		t.Fatalf("reload: tags %v delivered %d of %d", v.Tags, delivered, len(ms))
	}
}

// Last data: recorded per reading, saved at most once a minute, survives a
// restart, and never invents a twin for an unknown identity.
func TestLastData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "twins.json")
	s := New(path, func(id string) bool { return id == "pump-1" })
	now := time.UnixMilli(1_000_000_000_000)
	s.now = func() time.Time { return now }
	pending := func() bool {
		select {
		case <-s.dirty:
			return true
		default:
			return false
		}
	}
	s.Data("ghost")
	if _, err := s.Get("ghost"); err == nil || len(s.devs) != 0 {
		t.Fatal("data from an unknown identity created a twin")
	}
	s.Data("pump-1")
	if !pending() {
		t.Fatal("first data did not ask for a save")
	}
	now = now.Add(30 * time.Second)
	s.Data("pump-1")
	if pending() {
		t.Error("saved again within a minute")
	}
	v, _ := s.Get("pump-1")
	if v.LastData != now.UnixMilli() || v.LastActivity != now.UnixMilli() {
		t.Errorf("view %d/%d, want %d", v.LastData, v.LastActivity, now.UnixMilli())
	}
	now = now.Add(31 * time.Second)
	s.Data("pump-1")
	if !pending() {
		t.Error("no save after a minute")
	}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	r := New(path, nil)
	if err := r.Load(); err != nil {
		t.Fatal(err)
	}
	if v, _ := r.Get("pump-1"); v.LastData != now.UnixMilli() {
		t.Errorf("after restart %d, want %d", v.LastData, now.UnixMilli())
	}
	var nilSvc *Service
	nilSvc.Data("pump-1") // a runtime without twins: no panic
}

// The catalog and each device's command history survive a restart.
func TestCommandsPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "twins.json")
	s := New(path, func(id string) bool { return id == "valve-1" })
	if _, err := s.SetCommand(Command{Name: "close", Kind: "message", Payload: json.RawMessage(`{"cmd":"close"}`)}); err != nil {
		t.Fatal(err)
	}
	run, err := s.RunCommand(context.Background(), "valve-1", "close", "ana", nil)
	if err != nil || run.Status != "queued" {
		t.Fatalf("run %+v %v", run, err)
	}
	if _, err := s.RunCommand(context.Background(), "ghost", "close", "ana", nil); err == nil {
		t.Error("ran on an unknown device")
	}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	r := New(path, func(string) bool { return true })
	if err := r.LoadAll(); err != nil {
		t.Fatal(err)
	}
	if cs := r.Commands(); len(cs) != 1 || cs[0].Role != "operator" {
		t.Errorf("catalog after restart %+v", cs)
	}
	if h := r.Runs("valve-1"); len(h) != 1 || h[0].By != "ana" || h[0].Status != "queued" {
		t.Errorf("history after restart %+v", h)
	}
}
