package gateway_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/williamtatendajose/prediction/iot-hub/internal/notify"
)

func TestTenantSettings(t *testing.T) {
	var mu sync.Mutex
	var got []map[string]any
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		mu.Lock()
		got = append(got, m)
		mu.Unlock()
	}))
	defer hook.Close()

	s := start(t, "")
	call(t, "POST", s.url+"/api/admin/tenants", super, `{"id":"acme"}`)
	adm := issue(t, s, "acme", `{"id":"ops","role":"admin"}`)
	op := issue(t, s, "acme", `{"id":"shift","role":"operator"}`)

	// Targets are named; their URLs (secrets) are never shown back.
	secretURL := hook.URL + "/T000/B000/very-secret-token"
	if code, _, raw := call(t, "PUT", s.url+"/api/settings/targets/ops-hook", adm, `{"spec":"webhook=`+secretURL+`"}`); code != 200 || strings.Contains(raw, "very-secret-token") {
		t.Fatalf("add target: %d %s", code, raw)
	}
	if code, _, _ := call(t, "GET", s.url+"/api/settings", op, ""); code != 403 {
		t.Errorf("operator reading settings: %d", code)
	}
	for name, body := range map[string]string{
		"unknown target":   `{"notify":{"targets":["nope"]}}`,
		"levels unordered": `{"escalation":{"levels":[{"afterMin":30,"targets":["ops-hook"]},{"afterMin":15,"targets":["ops-hook"]}]}}`,
		"bad time zone":    `{"timeZone":"Mars/Olympus"}`,
		"bad shift":        `{"shifts":["25:00"]}`,
		"unknown field":    `{"notfy":{}}`,
		"short repeat":     `{"escalation":{"repeatMin":1,"levels":[{"afterMin":5,"targets":["ops-hook"]}]}}`,
	} {
		if code, _, _ := call(t, "PUT", s.url+"/api/settings", adm, body); code != 400 {
			t.Errorf("%s: %d", name, code)
		}
	}
	body := `{"notify":{"targets":["ops-hook"],"resolved":true},"escalation":{"levels":[{"afterMin":15,"targets":["ops-hook"]}],"repeatMin":30},
		"shifts":["07:00","19:00"],"timeZone":"Africa/Harare","webhookSecret":"s3cret"}`
	code, _, raw := call(t, "PUT", s.url+"/api/settings", adm, body)
	if code != 200 || strings.Contains(raw, "s3cret") || !strings.Contains(raw, `"webhookSecretSet":true`) {
		t.Fatalf("put settings: %d %s", code, raw)
	}
	// In use: can't be removed.
	if code, _, _ := call(t, "DELETE", s.url+"/api/settings/targets/ops-hook", adm, ""); code != 400 {
		t.Errorf("removing a target in use: %d", code)
	}
	// Applied live: an alarm reaches the webhook, signed.
	call(t, "PUT", s.url+"/api/sensors/tank", adm, `{"fields":{"level":{"detect":{"high":90}}}}`)
	call(t, "POST", s.url+"/api/sensors/tank/data", adm, `{"level":97}`)
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("alarm not delivered to the tenant's webhook")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got[0]["status"] != "open" || !strings.Contains(got[0]["title"].(string), "tank") {
		t.Fatalf("delivered %v", got[0])
	}
	// Shifts and time zone apply at once: the current shift starts at
	// 07:00 or 19:00 Harare time.
	_, rep, _ := call(t, "GET", s.url+"/api/reports?shift=current", adm, "")
	harare, _ := time.LoadLocation("Africa/Harare")
	from := time.UnixMilli(int64(rep["from"].(float64))).In(harare)
	if h := from.Hour(); from.Minute() != 0 || h != 7 && h != 19 {
		t.Fatalf("current shift starts %v, want 07:00 or 19:00 Harare", from)
	}
	// Stored per tenant, private, and reloaded after a restart.
	p := filepath.Join(s.dir, "tenants", "acme", "settings.json")
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatalf("settings file: %v", err)
	}
	// Windows has no POSIX mode bits (access is by ACL).
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("settings file mode %v, want 0600", fi.Mode().Perm())
	}
	call(t, "PATCH", s.url+"/api/admin/tenants/acme", super, `{"status":"suspended"}`)
	call(t, "PATCH", s.url+"/api/admin/tenants/acme", super, `{"status":"active"}`)
	_, m, _ := call(t, "GET", s.url+"/api/settings", adm, "")
	st := m["settings"].(map[string]any)
	if st["timeZone"] != "Africa/Harare" || len(st["targets"].([]any)) != 1 {
		t.Fatalf("after restart: %v", st)
	}
}

func TestPrivateTargetsBlocked(t *testing.T) {
	notify.BlockPrivateNetworks(true)
	defer notify.BlockPrivateNetworks(false)
	s := start(t, "")
	call(t, "POST", s.url+"/api/admin/tenants", super, `{"id":"acme"}`)
	adm := issue(t, s, "acme", `{"id":"ops","role":"admin"}`)
	for _, spec := range []string{"webhook=http://127.0.0.1:8080/api/admin/tenants", "webhook=http://169.254.169.254/latest/meta-data/",
		"slack=http://localhost/x", "email=smtp://10.0.0.2:25?from=a@b.co&to=c@d.co", "webhook=http://[::1]/x"} {
		code, _, raw := call(t, "PUT", s.url+"/api/settings/targets/x", adm, `{"spec":"`+spec+`"}`)
		if code != 400 || !strings.Contains(raw, "not a public address") {
			t.Errorf("%s: %d %s", spec, code, raw)
		}
	}
}
