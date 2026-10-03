package backup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/williamtatendajose/prediction/iot-hub/internal/auth"
	"github.com/williamtatendajose/prediction/iot-hub/internal/calc"
	"github.com/williamtatendajose/prediction/iot-hub/internal/store"
	"github.com/williamtatendajose/prediction/iot-hub/internal/tsdb"
)

func hub(t *testing.T, max int) (*store.Store, *auth.Store) {
	st := store.New(store.Options{MaxSensors: max})
	creds := auth.New(filepath.Join(t.TempDir(), "auth.json"), "")
	return st, creds
}

func TestRoundTrip(t *testing.T) {
	st, creds := hub(t, 10)
	lo := 2.0
	st.Upsert(store.Sensor{ID: "meter", Name: "Main meter", Fields: map[string]store.Field{
		"p":   {Unit: "kW", Min: &lo, Calc: &calc.Spec{Formula: "v * i / 1000"}},
		"kwh": {Unit: "kWh", Calc: &calc.Spec{Integrate: "p"}},
	}})
	st.SetDashboard(json.RawMessage(`{"tiles":[{"type":"stat","sensor":"meter"}]}`))
	tok, _ := creds.Add("plc-1", auth.Device, []string{"meter"}, "line 1")

	b, _ := json.Marshal(Export(st, creds, true))
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	st2, creds2 := hub(t, 10)
	plan, err := Import(c, st2, creds2, false, false)
	if err != nil || len(plan.Created) != 1 || !plan.Dashboard || plan.Devices != 1 {
		t.Fatalf("%+v %v", plan, err)
	}
	if !reflect.DeepEqual(st.Definitions(), st2.Definitions()) || string(st2.Dashboard()) != string(st.Dashboard()) {
		t.Fatalf("definitions differ:\n%+v\n%+v", st.Definitions(), st2.Definitions())
	}
	if id, ok := creds2.Authenticate(tok); !ok || id.ID != "plc-1" {
		t.Fatal("the existing token must work on the restored hub")
	}
	// Without ?devices the export has no credentials at all.
	if c := Export(st, creds, false); c.Devices != nil {
		t.Fatal("devices exported without being asked")
	}
}

func TestImportIsAllOrNothing(t *testing.T) {
	st, creds := hub(t, 3)
	st.Upsert(store.Sensor{ID: "a"})
	st.Upsert(store.Sensor{ID: "old"})
	before := st.Definitions()
	for name, c := range map[string]Config{
		"bad formula": {Version: 1, Sensors: []store.Sensor{{ID: "b"}, {ID: "c", Fields: map[string]store.Field{"x": {Calc: &calc.Spec{Formula: "1 +"}}}}}},
		"bad id":      {Version: 1, Sensors: []store.Sensor{{ID: "b"}, {ID: "../etc"}}},
		"duplicate":   {Version: 1, Sensors: []store.Sensor{{ID: "b"}, {ID: "b"}}},
		"over limit":  {Version: 1, Sensors: []store.Sensor{{ID: "b"}, {ID: "c"}}}, // 2 existing + 2 new > 3 in merge
		"bad json":    {Version: 1, Sensors: []store.Sensor{{ID: "b"}}, Dashboard: json.RawMessage(`{`)},
		"bad version": {Version: 2, Sensors: []store.Sensor{{ID: "b"}}},
		"bad device":  {Version: 1, Sensors: []store.Sensor{{ID: "b"}}, Devices: []auth.Credential{{Identity: auth.Identity{ID: "d", Role: auth.Device}, Hash: "nothex"}}},
	} {
		if _, err := Import(c, st, creds, false, false); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if !reflect.DeepEqual(st.Definitions(), before) {
			t.Fatalf("%s: store changed by a failed import", name)
		}
	}
	// Replace frees the slots of sensors not in the backup, so the same
	// two new sensors fit: the result is a, b, c with "old" deleted.
	c := Config{Version: 1, Sensors: []store.Sensor{{ID: "a"}, {ID: "b"}, {ID: "c"}}}
	plan, err := Import(c, st, creds, true, true)
	if err != nil || !reflect.DeepEqual(plan.Deleted, []string{"old"}) || len(plan.Created) != 2 || !reflect.DeepEqual(st.Definitions(), before) {
		t.Fatalf("dry run: %+v %v (store must be unchanged)", plan, err)
	}
	if _, err := Import(c, st, creds, true, false); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, d := range st.Definitions() {
		ids = append(ids, d.ID)
	}
	if !reflect.DeepEqual(ids, []string{"a", "b", "c"}) {
		t.Fatalf("after replace: %v", ids)
	}
}

func TestImportDevices(t *testing.T) {
	st, creds := hub(t, 10)
	tokA, _ := creds.Add("a", auth.Device, []string{"x"}, "")
	tokB, _ := creds.Add("b", auth.Viewer, nil, "")
	exp := creds.Export()

	// Restoring an older backup where "a" had another token revokes a's
	// current token; replace also removes "c", which the backup lacks.
	creds.Add("c", auth.Viewer, nil, "")
	exp[0].Hash = "aa" + exp[0].Hash[2:]
	if exp[0].Hash == creds.Export()[0].Hash {
		exp[0].Hash = "bb" + exp[0].Hash[2:]
	}
	plan, err := Import(Config{Version: 1, Devices: exp}, st, creds, true, false)
	if err != nil || !reflect.DeepEqual(plan.Revoked, []string{"a", "c"}) {
		t.Fatalf("%+v %v", plan, err)
	}
	if _, ok := creds.Authenticate(tokA); ok {
		t.Error("a's replaced token still works")
	}
	if _, ok := creds.Authenticate(tokB); !ok {
		t.Error("b's unchanged token stopped working")
	}
	// Two identities can't share a token.
	dup := creds.Export()
	dup[1].Hash = dup[0].Hash
	if _, err := Import(Config{Version: 1, Devices: dup}, st, creds, true, false); err == nil {
		t.Error("shared token accepted")
	}
	// A hub without -auth-file can't keep restored credentials: refuse
	// rather than lose them at the next restart.
	if _, err := Import(Config{Version: 1, Devices: exp}, st, auth.New("", ""), false, false); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("non-persistent auth: %v", err)
	}
}

func TestSchedulerRotation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := tsdb.Open(ctx, "sqlite:"+filepath.Join(dir, "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st, creds := hub(t, 10)
	st.Upsert(store.Sensor{ID: "a"})
	now := time.Date(2026, 10, 2, 6, 0, 0, 0, time.UTC)
	s := &Scheduler{Dir: filepath.Join(dir, "bk"), Every: time.Hour, Keep: 2, DB: db,
		Config: func() Config { return Export(st, creds, true) }, Now: func() time.Time { return now }}
	for i := range 3 {
		now = now.Add(time.Hour)
		files, err := s.Once(ctx)
		if err != nil || len(files) != 2 {
			t.Fatalf("run %d: %v %v", i, files, err)
		}
	}
	stat, _ := s.Status()
	var names []string
	for _, f := range stat.Files {
		names = append(names, f.Name)
	}
	want := []string{"iothub-20261002-090000.json", "iothub-20261002-090000.db", "iothub-20261002-080000.json", "iothub-20261002-080000.db"}
	if !reflect.DeepEqual(names, want) || !stat.Database {
		t.Fatalf("kept %v, want %v", names, want)
	}
	fi, _ := os.Stat(filepath.Join(s.Dir, want[1]))
	di, _ := os.Stat(s.Dir)
	// Windows has no POSIX mode bits (access is by ACL).
	if runtime.GOOS != "windows" && (fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700) {
		t.Errorf("modes %v %v", fi.Mode(), di.Mode())
	}
	var c Config
	b, _ := os.ReadFile(filepath.Join(s.Dir, want[0]))
	if json.Unmarshal(b, &c) != nil || len(c.Sensors) != 1 {
		t.Fatalf("config backup %s", b)
	}
	// Downloads only resolve real backup names inside the directory.
	for _, bad := range []string{"../live.db", "iothub-20261002-090000.db/..", "live.db", "iothub-20261002-090000.db.partial", "iothub-2026.db"} {
		if _, ok := s.Path(bad); ok {
			t.Errorf("%q resolved", bad)
		}
	}
	if _, ok := s.Path(want[1]); !ok {
		t.Error("real backup not found")
	}
}
