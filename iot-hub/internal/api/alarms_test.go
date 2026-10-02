package api_test

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/williamtatendajose/prediction/iot-hub/internal/alarm"
	"github.com/williamtatendajose/prediction/iot-hub/internal/analytics"
	"github.com/williamtatendajose/prediction/iot-hub/internal/anomaly"
	"github.com/williamtatendajose/prediction/iot-hub/internal/api"
	"github.com/williamtatendajose/prediction/iot-hub/internal/auth"
	"github.com/williamtatendajose/prediction/iot-hub/internal/ingest"
	"github.com/williamtatendajose/prediction/iot-hub/internal/notify"
	"github.com/williamtatendajose/prediction/iot-hub/internal/store"
	"github.com/williamtatendajose/prediction/iot-hub/internal/stream"
	"github.com/williamtatendajose/prediction/iot-hub/internal/tsdb"
)

type alarmEnv struct {
	url      string
	op, view string
	notified *atomic.Int64
	db       tsdb.DB
}

func alarmHub(t *testing.T, dbPath string) alarmEnv {
	ctx, cancel := context.WithCancel(context.Background())
	db, err := tsdb.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	st := store.New(store.Options{AutoRegister: true})
	hub := stream.NewHub(16)
	det := anomaly.New(anomaly.Config{})
	w := tsdb.NewWriter(db, 256)
	w.Interval = 20 * time.Millisecond
	go w.Run(ctx)
	alarms := alarm.New(db)
	if err := alarms.Load(ctx); err != nil {
		t.Fatal(err)
	}
	var notified atomic.Int64
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { notified.Add(1) }))
	tg, _ := notify.ParseTarget("webhook=" + recv.URL)
	n := notify.New(notify.Config{Targets: []*notify.Target{tg}, Cooldown: time.Millisecond})
	go n.Run(ctx)
	pipe := &ingest.Pipeline{Store: st, Hub: hub, Detector: det, Writer: w, Alarms: alarms, OnEvent: n.Notify}
	creds := auth.New(filepath.Join(t.TempDir(), "d.json"), "adm")
	srv := &api.Server{Store: st, Hub: hub, Pipeline: pipe, Auth: creds, Alarms: alarms,
		Analytics: &analytics.Service{Store: st, DB: db, Detector: det, Alarms: alarms}}
	h := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		h.Close()
		recv.Close()
		cancel()
		<-w.Done()
		<-n.Done()
		db.Close()
	})
	e := alarmEnv{url: h.URL, notified: &notified, db: db}
	e.op = mkDevice(t, h.URL, "shift-a", "operator")
	e.view = mkDevice(t, h.URL, "screen", "viewer")
	return e
}

func mkDevice(t *testing.T, url, id, role string) string {
	code, m := do(t, nil, "POST", url+"/api/devices", "adm", `{"id":"`+id+`","role":"`+role+`"}`)
	if code != 201 {
		t.Fatalf("device %s: %d %v", id, code, m)
	}
	return m["token"].(string)
}

func activeEvents(t *testing.T, url, tok string) []anomaly.Event {
	req, _ := http.NewRequest("GET", url+"/api/anomalies", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var evs []anomaly.Event
	json.NewDecoder(res.Body).Decode(&evs)
	return evs
}

func TestAlarmWorkflow(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "a.db")
	e := alarmHub(t, dbPath)
	do(t, nil, "PUT", e.url+"/api/sensors/tank", "adm", `{"fields":{"level":{"detect":{"low":15,"z":-1}}}}`)
	do(t, nil, "POST", e.url+"/api/sensors/tank/data", "adm", `{"level":10}`)
	time.Sleep(150 * time.Millisecond)
	if e.notified.Load() != 1 {
		t.Fatalf("first alarm should notify: %d", e.notified.Load())
	}
	evs := activeEvents(t, e.url, e.view)
	if len(evs) != 1 {
		t.Fatalf("events %+v", evs)
	}
	id := evs[0].ID

	// Viewer may read but not act; operator may act but not manage.
	if code, _ := do(t, nil, "POST", e.url+"/api/anomalies/"+id+"/ack", e.view, `{"verdict":"confirmed"}`); code != 403 {
		t.Fatalf("viewer ack: %d", code)
	}
	if code, _ := do(t, nil, "GET", e.url+"/api/audit", e.op, ""); code != 403 {
		t.Fatalf("operator audit: %d", code)
	}
	if code, m := do(t, nil, "POST", e.url+"/api/anomalies/"+id+"/ack", e.op, `{"verdict":"wrong"}`); code != 400 {
		t.Fatalf("bad verdict: %d %v", code, m)
	}
	if code, _ := do(t, nil, "POST", e.url+"/api/anomalies/nope/ack", e.op, `{}`); code != 404 {
		t.Fatalf("unknown episode: %d", code)
	}
	code, m := do(t, nil, "POST", e.url+"/api/anomalies/"+id+"/ack", e.op, `{"verdict":"false_alarm","note":"=HYPERLINK(\"x\")"}`)
	if code != 200 || m["ack"].(map[string]any)["by"] != "shift-a" {
		t.Fatalf("ack: %d %v", code, m)
	}
	if code, _ := do(t, nil, "POST", e.url+"/api/anomalies/"+id+"/notes", e.op, `{"text":"float switch cleaned"}`); code != 201 {
		t.Fatalf("note: %d", code)
	}

	// Shelve the sensor: new episodes are recorded but not notified.
	if code, m := do(t, nil, "POST", e.url+"/api/shelves", e.op, `{"sensor":"tank","duration":"2h","reason":"sensor replacement"}`); code != 201 {
		t.Fatalf("shelve: %d %v", code, m)
	}
	do(t, nil, "POST", e.url+"/api/sensors/tank/data", "adm", `{"level":50}`)
	do(t, nil, "POST", e.url+"/api/sensors/tank/data", "adm", `{"level":5}`)
	time.Sleep(150 * time.Millisecond)
	if e.notified.Load() != 1 {
		t.Fatalf("shelved alarm must not notify: %d", e.notified.Load())
	}
	evs = activeEvents(t, e.url, e.view)
	if len(evs) != 2 || !evs[0].Shelved {
		t.Fatalf("shelved episode still recorded and flagged: %+v", evs)
	}
	if code, _ := do(t, nil, "DELETE", e.url+"/api/shelves/tank::", e.op, ""); code != 204 {
		t.Fatalf("unshelve: %d", code)
	}

	// Labels export: only acknowledged episodes, formula text defused.
	req, _ := http.NewRequest("GET", e.url+"/api/labels.csv", nil)
	req.Header.Set("Authorization", "Bearer "+e.view)
	res, _ := http.DefaultClient.Do(req)
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	rows, _ := csv.NewReader(strings.NewReader(string(b))).ReadAll()
	if len(rows) != 2 || rows[1][8] != "false_alarm" || rows[1][9] != "shift-a" || !strings.HasPrefix(rows[1][11], "'=") {
		t.Fatalf("labels csv:\n%s", b)
	}

	// Audit trail, newest first, covering config and alarm actions.
	req, _ = http.NewRequest("GET", e.url+"/api/audit", nil)
	req.Header.Set("Authorization", "Bearer adm")
	res, _ = http.DefaultClient.Do(req)
	var log []anomaly.AuditEntry
	json.NewDecoder(res.Body).Decode(&log)
	res.Body.Close()
	var acts []string
	for _, a := range log {
		acts = append(acts, a.Actor+":"+a.Action)
	}
	want := "shift-a:alarm.unshelve,shift-a:alarm.shelve,shift-a:alarm.note,shift-a:alarm.ack,adm:sensor.define,admin:device.add,admin:device.add"
	got := strings.Join(acts, ",")
	if strings.ReplaceAll(got, "adm:", "admin:") != strings.ReplaceAll(want, "adm:", "admin:") {
		t.Fatalf("audit:\n got %s\nwant %s", got, want)
	}
}

func TestAlarmStatePersists(t *testing.T) {
	ctx := context.Background()
	db, err := tsdb.Open(ctx, filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m := alarm.New(db)
	e := anomaly.Event{ID: "s.f.range.1", Sensor: "s", Field: "f", Kind: "range"}
	m.Acknowledge(ctx, e, "op", "expected", "planned stop")
	m.AddNote(ctx, e.ID, "op", "restarted 14:00")
	m.Shelve(ctx, "s", "f", "", time.Hour, "op", "calibration")

	m2 := alarm.New(db) // as after a restart
	if err := m2.Load(ctx); err != nil {
		t.Fatal(err)
	}
	got := m2.Decorate([]anomaly.Event{e})[0]
	if got.Ack == nil || got.Ack.Verdict != "expected" || got.Notes != 1 || !got.Shelved {
		t.Fatalf("after restart: %+v", got)
	}
	ns, _ := m2.Notes(ctx, e.ID)
	if len(ns) != 1 || ns[0].Text != "restarted 14:00" {
		t.Fatalf("notes %+v", ns)
	}
}
