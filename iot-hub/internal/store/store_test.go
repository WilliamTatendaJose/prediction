package store

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestRingWrap(t *testing.T) {
	r := NewRing(3)
	for i := int64(1); i <= 5; i++ {
		r.Push(i, float32(i*10))
	}
	ts, v := r.Last(0, 0)
	if len(ts) != 3 || ts[0] != 3 || ts[2] != 5 || v[0] != 30 || v[2] != 50 {
		t.Fatalf("got %v %v", ts, v)
	}
	ts, _ = r.Last(2, 0)
	if len(ts) != 2 || ts[0] != 4 {
		t.Fatalf("limit: %v", ts)
	}
	ts, _ = r.Last(0, 4)
	if len(ts) != 1 || ts[0] != 5 {
		t.Fatalf("since: %v", ts)
	}
}

func TestIngestTypesAndLimits(t *testing.T) {
	s := New(Options{Capacity: 4, MaxSensors: 1, MaxFields: 2, AutoRegister: true})
	r, err := s.Ingest("a", 1000, map[string]any{"temp": 21.5, "door": "Open", "on": true, "nested": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	// Fields are capped at 2; nested is dropped. Map order decides which two.
	if len(r.Values) != 2 {
		t.Fatalf("values: %v", r.Values)
	}
	if _, err := s.Ingest("b", 0, map[string]any{"x": 1.0}); err == nil {
		t.Fatal("expected sensor limit")
	}
	if _, err := s.Ingest("bad id", 0, map[string]any{"x": 1.0}); err == nil {
		t.Fatal("expected invalid id")
	}
}

func TestStringsAreStateOnly(t *testing.T) {
	s := New(Options{AutoRegister: true})
	if _, err := s.Ingest("d", 1, map[string]any{"door": "Open", "locked": false}); err != nil {
		t.Fatal(err)
	}
	ts, _, _ := s.History("d", "door", 0, 0)
	if len(ts) != 0 {
		t.Fatal("strings must not create a series")
	}
	ts, v, _ := s.History("d", "locked", 0, 0)
	if len(ts) != 1 || v[0] != 0 {
		t.Fatalf("bool series: %v %v", ts, v)
	}
	sv, _ := s.Get("d")
	if sv.Last["door"] != "Open" || sv.LastSeen != 1 {
		t.Fatalf("last: %+v", sv)
	}
}

func TestNoAutoRegister(t *testing.T) {
	s := New(Options{})
	if _, err := s.Ingest("x", 0, map[string]any{"v": 1.0}); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	_ = s.Upsert(Sensor{ID: "x", Fields: map[string]Field{"v": {Unit: "C"}}})
	r, err := s.Ingest("x", 0, map[string]any{"v": 1.0, "other": 2.0})
	if err != nil || len(r.Values) != 1 {
		t.Fatalf("got %v %v", r, err)
	}
}

func TestPersistRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	s := New(Options{Path: p, AutoRegister: true})
	_ = s.Upsert(Sensor{ID: "x", Name: "X", Fields: map[string]Field{"v": {Unit: "C"}}})
	_ = s.SetDashboard([]byte(`{"tiles":[{"id":"1"}]}`))
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	s2 := New(Options{Path: p})
	if err := s2.Load(); err != nil {
		t.Fatal(err)
	}
	v, err := s2.Get("x")
	if err != nil || v.Name != "X" || v.Fields["v"].Unit != "C" {
		t.Fatalf("%+v %v", v, err)
	}
	var buf bytes.Buffer
	_ = json.Compact(&buf, s2.Dashboard())
	if buf.String() != `{"tiles":[{"id":"1"}]}` {
		t.Fatalf("dashboard %s", buf.String())
	}
}

func TestRestore(t *testing.T) {
	s := New(Options{AutoRegister: true, Capacity: 3})
	// Types are learnt on ingest (and persisted with the definition).
	if _, err := s.Ingest("d", 1, map[string]any{"locked": true, "door": "Open", "t": 1.0}); err != nil {
		t.Fatal(err)
	}
	s2 := New(Options{AutoRegister: true, Capacity: 3})
	sv, _ := s.Get("d")
	_ = s2.Upsert(sv.Sensor) // as if loaded from iothub.json
	if sv.Fields["locked"].Type != "bool" || sv.Fields["door"].Type != "text" || sv.Fields["t"].Type != "number" {
		t.Fatalf("types: %+v", sv.Fields)
	}
	s2.Restore("d", "t", []int64{10, 20, 30, 40, 50}, []float64{1, 2, 3, 4, 5}, "", 0)
	s2.Restore("d", "locked", []int64{10, 60}, []float64{1, 0}, "", 0)
	s2.Restore("d", "door", nil, nil, "Closed", 70)
	got, _ := s2.Get("d")
	if got.Last["t"] != 5.0 || got.Last["locked"] != false || got.Last["door"] != "Closed" || got.LastSeen != 70 {
		t.Fatalf("restored last: %+v seen %d", got.Last, got.LastSeen)
	}
	ts, v, _ := s2.History("d", "t", 0, 0)
	if len(ts) != 3 || ts[0] != 30 || v[2] != 5 {
		t.Fatalf("ring keeps the newest capacity points: %v %v", ts, v)
	}
	// Live data that arrived first is never overwritten by older history.
	s2.Ingest("d", 100, map[string]any{"t": 42.0})
	s2.Restore("d", "t", []int64{10}, []float64{1}, "", 0)
	if got, _ := s2.Get("d"); got.Last["t"] != 42.0 {
		t.Fatalf("restore overwrote live value: %v", got.Last["t"])
	}
	// Unknown sensors come back only with auto-register.
	s3 := New(Options{})
	if s3.Restore("x", "t", []int64{1}, []float64{1}, "", 0) {
		t.Fatal("restored an unknown sensor without auto-register")
	}
}
