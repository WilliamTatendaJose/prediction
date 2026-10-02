package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/williamtatendajose/prediction/iot-hub/internal/analytics"
	"github.com/williamtatendajose/prediction/iot-hub/internal/api"
	"github.com/williamtatendajose/prediction/iot-hub/internal/auth"
	"github.com/williamtatendajose/prediction/iot-hub/internal/backup"
	"github.com/williamtatendajose/prediction/iot-hub/internal/ingest"
	"github.com/williamtatendajose/prediction/iot-hub/internal/store"
	"github.com/williamtatendajose/prediction/iot-hub/internal/stream"
)

type bkHub struct {
	url   string
	st    *store.Store
	creds *auth.Store
}

func backupHub(t *testing.T) bkHub {
	st := store.New(store.Options{AutoRegister: true})
	creds := auth.New(filepath.Join(t.TempDir(), "auth.json"), "admin-token")
	pipe := &ingest.Pipeline{Store: st, Hub: stream.NewHub(4)}
	srv := &api.Server{Store: st, Hub: pipe.Hub, Pipeline: pipe, Auth: creds, Analytics: &analytics.Service{Store: st}}
	srv.Backups = &backup.Scheduler{Dir: filepath.Join(t.TempDir(), "bk"), Every: 24 * time.Hour, Keep: 3,
		Config: func() backup.Config { return backup.Export(st, creds, true) }}
	h := httptest.NewServer(srv.Handler())
	t.Cleanup(h.Close)
	return bkHub{h.URL, st, creds}
}

func call(t *testing.T, method, url, token, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func TestConfigBackupAPI(t *testing.T) {
	a, b := backupHub(t), backupHub(t)
	const adm = "admin-token"
	call(t, "PUT", a.url+"/api/sensors/tank", adm, `{"name":"Tank","fields":{"level":{"unit":"%","max":100}}}`)
	call(t, "PUT", a.url+"/api/dashboard", adm, `{"tiles":[{"type":"meter","sensor":"tank","field":"level"}]}`)
	viewer, _ := a.creds.Add("screen", auth.Viewer, nil, "")

	if code, _ := call(t, "GET", a.url+"/api/config", viewer, ""); code != 403 {
		t.Fatalf("viewer export: %d", code)
	}
	code, exp := call(t, "GET", a.url+"/api/config?devices=1", adm, "")
	if code != 200 || !strings.Contains(exp, `"hash"`) || strings.Contains(exp, viewer) {
		t.Fatalf("export %d %s", code, exp)
	}

	// Dry run changes nothing; the real import restores sensors, dashboard
	// and the viewer's working token.
	code, body := call(t, "POST", b.url+"/api/config?dryRun=1", adm, exp)
	if code != 200 || !strings.Contains(body, `"created":["tank"]`) || len(b.st.Definitions()) != 0 {
		t.Fatalf("dry run %d %s", code, body)
	}
	if code, body = call(t, "POST", b.url+"/api/config", adm, exp); code != 200 {
		t.Fatalf("import %d %s", code, body)
	}
	if code, body = call(t, "GET", b.url+"/api/dashboard", viewer, ""); code != 200 || !strings.Contains(body, `"meter"`) {
		t.Fatalf("restored viewer token / dashboard: %d %s", code, body)
	}
	if code, _ = call(t, "POST", b.url+"/api/config", adm, `{"version":1,"sensors":[],"sensorz":[]}`); code != 400 {
		t.Errorf("unknown field accepted: %d", code)
	}
	if code, _ = call(t, "POST", b.url+"/api/config?mode=wipe", adm, exp); code != 400 {
		t.Errorf("bad mode: %d", code)
	}

	// Scheduled-style backup on demand, then download it.
	code, body = call(t, "POST", a.url+"/api/backups", adm, "")
	var run struct{ Files []string }
	json.Unmarshal([]byte(body), &run)
	if code != 200 || len(run.Files) != 1 { // no database: config only
		t.Fatalf("backup now %d %s", code, body)
	}
	if code, body = call(t, "GET", a.url+"/api/backups/"+run.Files[0], adm, ""); code != 200 || !strings.Contains(body, `"tank"`) {
		t.Fatalf("download %d", code)
	}
	for _, bad := range []string{"..%2Fauth.json", "auth.json", "iothub-20261002-060000.db"} {
		if code, _ = call(t, "GET", a.url+"/api/backups/"+bad, adm, ""); code != 404 {
			t.Errorf("%s: %d", bad, code)
		}
	}
	if code, _ = call(t, "GET", a.url+"/api/backups/"+run.Files[0], viewer, ""); code != 403 {
		t.Errorf("viewer download: %d", code)
	}
}
