package gateway_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/williamtatendajose/prediction/iot-hub/internal/anomaly"
	"github.com/williamtatendajose/prediction/iot-hub/internal/api"
	"github.com/williamtatendajose/prediction/iot-hub/internal/auth"
	"github.com/williamtatendajose/prediction/iot-hub/internal/broker"
	"github.com/williamtatendajose/prediction/iot-hub/internal/gateway"
	"github.com/williamtatendajose/prediction/iot-hub/internal/ingest"
	"github.com/williamtatendajose/prediction/iot-hub/internal/store"
	"github.com/williamtatendajose/prediction/iot-hub/internal/tenant"
	"github.com/williamtatendajose/prediction/iot-hub/internal/tsdb"
	"github.com/williamtatendajose/prediction/iot-hub/web"
)

const super = "super-secret"

var client = &http.Client{Timeout: 3 * time.Second}

type resolver struct{ p *tenant.Platform }

func (r resolver) Pipeline(t string) (*ingest.Pipeline, error) {
	rt, err := r.p.Runtime(t)
	if err != nil {
		return nil, err
	}
	return rt.Pipe, nil
}

type saas struct {
	url, mqtt string
	plat      *tenant.Platform
	dir       string
}

func freeAddr(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func start(t *testing.T, dbURL string) saas {
	dir := t.TempDir()
	creds := auth.New(filepath.Join(dir, "auth.json"), super)
	var mq *broker.Broker
	plat := &tenant.Platform{
		Dir: filepath.Join(dir, "tenants"), DBURL: dbURL, Creds: creds,
		Base: tenant.Options{Capacity: 256, MaxSensors: 100, MaxFields: 16, AutoRegister: true, Anomaly: true,
			AnomalyCf: anomaly.Config{Warmup: 20, Persist: 1},
			Hooks: tenant.Hooks{
				Publish:         func(tn string, r store.Reading) { mq.RepublishTo(tn, r) },
				PublishEvent:    func(tn string, e anomaly.Event) { mq.PublishEventTo(tn, e) },
				Kick:            func(tn, id string) { mq.KickTenant(tn, id) },
				DeviceListening: func(tn, id, sub string) bool { return mq.DeviceListening(tn, id, sub) },
				DeviceConnected: func(tn, id string) bool { return mq.DeviceConnected(tn, id) },
				SendToDevice:    func(tn, id, sub string, p []byte) int { return mq.SendToDevice(tn, id, sub, p) },
			}},
		OnStop: func(id string) { mq.KickTenant(id, "") },
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := plat.Load(ctx); err != nil {
		t.Fatal(err)
	}
	addr := freeAddr(t)
	var err error
	twins := func(tn string) broker.DeviceHandler {
		if rt, err := plat.Runtime(tn); err == nil && rt != nil {
			return rt.Twins
		}
		return nil
	}
	mq, err = broker.New(broker.Config{TCPAddr: addr, Auth: creds.Platform(), Tenants: resolver{plat}, Twins: twins}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := mq.Serve(); err != nil {
		t.Fatal(err)
	}
	static, err := api.StaticHandler(web.FS)
	if err != nil {
		t.Fatal(err)
	}
	gw := &gateway.Gateway{Platform: plat, Creds: creds.Platform(), Static: static}
	h := httptest.NewServer(gw.Handler())
	t.Cleanup(func() { h.Close(); mq.Close(); plat.Close(); cancel() })
	return saas{url: h.URL, mqtt: "tcp://" + addr, plat: plat, dir: dir}
}

func call(t *testing.T, method, url, token, body string, hdr ...string) (int, map[string]any, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body) // a stream that opened (it shouldn't) ends at the timeout
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return res.StatusCode, m, string(b)
}

// issue creates an identity in a tenant as the superadmin and returns its token.
func issue(t *testing.T, s saas, tn, body string) string {
	t.Helper()
	code, m, raw := call(t, "POST", s.url+"/api/devices", super, body, "X-Tenant", tn)
	if code != 201 {
		t.Fatalf("issue in %s: %d %s", tn, code, raw)
	}
	tok, _ := m["token"].(string)
	return tok
}

func mqttConnect(t *testing.T, s saas, user, pass string) (paho.Client, error) {
	o := paho.NewClientOptions().AddBroker(s.mqtt).SetClientID(fmt.Sprintf("%s-%d", strings.ReplaceAll(user, "/", "-"), time.Now().UnixNano())).
		SetUsername(user).SetPassword(pass).SetAutoReconnect(false).SetConnectRetry(false)
	c := paho.NewClient(o)
	tok := c.Connect()
	tok.WaitTimeout(3 * time.Second)
	if tok.Error() == nil {
		t.Cleanup(func() { c.Disconnect(10) })
	}
	return c, tok.Error()
}

func TestTenantIsolation(t *testing.T) { isolation(t, "sqlite:readings.db") }

// IOTHUB_TEST_PG=postgres://postgres@127.0.0.1:55432/iothub_test
func TestTenantIsolationPostgres(t *testing.T) {
	u := os.Getenv("IOTHUB_TEST_PG")
	if u == "" {
		t.Skip("IOTHUB_TEST_PG not set")
	}
	isolation(t, u)
}

func isolation(t *testing.T, dbURL string) {
	pg := strings.HasPrefix(dbURL, "postgres")
	if pg {
		for _, sch := range []string{"t_acme", "t_globex", "t_orphan"} {
			tsdb.DropSchema(context.Background(), dbURL, sch)
		}
		t.Cleanup(func() {
			for _, sch := range []string{"t_acme", "t_globex", "t_orphan"} {
				tsdb.DropSchema(context.Background(), dbURL, sch)
			}
		})
	}
	s := start(t, dbURL)
	if pg {
		// A schema left behind must never be adopted by a new tenant.
		db, _ := tsdb.OpenSchema(context.Background(), dbURL, "t_orphan")
		db.Close()
		if code, _, raw := call(t, "POST", s.url+"/api/admin/tenants", super, `{"id":"orphan"}`); code != 409 {
			t.Fatalf("tenant over an existing schema: %d %s", code, raw)
		}
	}
	for _, tn := range []string{"acme", "globex"} {
		if code, _, raw := call(t, "POST", s.url+"/api/admin/tenants", super, `{"id":"`+tn+`","name":"`+tn+` Ltd"}`); code != 201 {
			t.Fatalf("create %s: %d %s", tn, code, raw)
		}
	}
	acmeAdmin := issue(t, s, "acme", `{"id":"ops","role":"admin"}`)
	acmeDev := issue(t, s, "acme", `{"id":"pump","role":"device","sensors":["tank-*"]}`)
	globexAdmin := issue(t, s, "globex", `{"id":"ops","role":"admin"}`)
	globexView := issue(t, s, "globex", `{"id":"screen","role":"viewer"}`)

	// The same sensor id in both tenants holds separate data.
	if code, _, _ := call(t, "POST", s.url+"/api/sensors/tank-1/data", acmeDev, `{"level":11}`); code != 202 {
		t.Fatalf("acme ingest %d", code)
	}
	if code, _, _ := call(t, "POST", s.url+"/api/sensors/tank-1/data", globexAdmin, `{"level":99}`); code != 202 {
		t.Fatalf("globex ingest %d", code)
	}
	level := func(tok string, hdr ...string) any {
		_, m, _ := call(t, "GET", s.url+"/api/sensors/tank-1", tok, "", hdr...)
		last, _ := m["last"].(map[string]any)
		return last["level"]
	}
	if level(acmeAdmin) != 11.0 || level(globexView) != 99.0 {
		t.Fatalf("separate values: acme %v globex %v", level(acmeAdmin), level(globexView))
	}
	if level(super, "X-Tenant", "acme") != 11.0 || level(super, "X-Tenant", "globex") != 99.0 {
		t.Fatal("superadmin sees each tenant via X-Tenant")
	}

	// Naming another tenant is refused outright, on every route.
	for _, path := range []string{"/api/sensors", "/api/sensors/tank-1", "/api/dashboard", "/api/anomalies", "/api/devices",
		"/api/config", "/api/audit", "/api/oee", "/api/reports", "/api/stream", "/api/sensors/tank-1/history?field=level"} {
		if code, _, _ := call(t, "GET", s.url+path, acmeAdmin, "", "X-Tenant", "globex"); code != 403 {
			t.Errorf("%s with X-Tenant globex: %d", path, code)
		}
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		if code, _, _ := call(t, "GET", s.url+path+sep+"tenant=globex", acmeAdmin, ""); code != 403 {
			t.Errorf("%s?tenant=globex: %d", path, code)
		}
	}
	for _, m := range []string{"POST /api/sensors/tank-1/data", "PUT /api/dashboard", "POST /api/devices", "DELETE /api/sensors/tank-1"} {
		method, path, _ := strings.Cut(m, " ")
		if code, _, _ := call(t, method, s.url+path, acmeAdmin, `{"tiles":[],"level":1,"id":"x","role":"viewer"}`, "X-Tenant", "globex"); code != 403 {
			t.Errorf("%s into globex: %d", m, code)
		}
	}
	if level(globexView) != 99.0 {
		t.Fatal("globex data changed")
	}
	// Device listings and config exports contain only the tenant's own.
	_, _, raw := call(t, "GET", s.url+"/api/devices", acmeAdmin, "")
	if strings.Contains(raw, "screen") || !strings.Contains(raw, "pump") {
		t.Fatalf("acme device list: %s", raw)
	}
	_, _, raw = call(t, "GET", s.url+"/api/config?devices=1", acmeAdmin, "")
	if strings.Contains(raw, "screen") || strings.Contains(raw, `"tenant":"globex"`) {
		t.Fatalf("acme export leaks globex: %s", raw)
	}
	// The dashboard is served to everyone; unknown paths 404.
	if code, _, raw := call(t, "GET", s.url+"/", "", ""); code != 200 || !strings.Contains(raw, "<!doctype html>") {
		t.Errorf("dashboard: %d", code)
	}
	if code, _, _ := call(t, "POST", s.url+"/", "", ""); code != 405 {
		t.Errorf("POST /: %d", code)
	}
	// Tenant admins can't reach platform administration.
	if code, _, _ := call(t, "GET", s.url+"/api/admin/tenants", acmeAdmin, ""); code != 403 {
		t.Errorf("tenant admin listing tenants: %d", code)
	}
	if code, _, _ := call(t, "GET", s.url+"/api/sensors", super, ""); code != 400 {
		t.Errorf("superadmin without a tenant: %d", code)
	}
	// Each tenant has its own database file (SQLite) or schema (Postgres),
	// and history comes from it.
	for _, tn := range []string{"acme", "globex"} {
		if _, err := os.Stat(filepath.Join(s.dir, "tenants", tn, "readings.db")); err != nil && !pg {
			t.Errorf("%s database: %v", tn, err)
		}
	}
	time.Sleep(1500 * time.Millisecond) // writer flush
	_, st, _ := call(t, "GET", s.url+"/api/sensors/tank-1/stats?field=level&from=-1h", acmeAdmin, "")
	if st["source"] != "db" || st["n"] != 1.0 || st["mean"] != 11.0 {
		t.Fatalf("acme stats from its database: %v", st)
	}

	// SSE: acme's stream never carries globex readings.
	req, _ := http.NewRequest("GET", s.url+"/api/stream", nil)
	req.Header.Set("Authorization", "Bearer "+acmeAdmin)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	call(t, "POST", s.url+"/api/sensors/tank-1/data", globexAdmin, `{"level":77}`)
	call(t, "POST", s.url+"/api/sensors/tank-1/data", acmeDev, `{"level":12}`)
	deadline := time.After(3 * time.Second)
	for got := false; !got; {
		select {
		case l := <-lines:
			if strings.Contains(l, "77") {
				t.Fatal("globex reading on acme's stream")
			}
			got = strings.Contains(l, `"level":12`)
		case <-deadline:
			t.Fatal("acme reading not streamed")
		}
	}

	// MQTT: usernames are tenant/id; topics are tenant/iot/...
	if _, err := mqttConnect(t, s, "globex/pump", acmeDev); err == nil {
		t.Error("acme's token accepted with a globex username")
	}
	if _, err := mqttConnect(t, s, "pump", acmeDev); err == nil {
		t.Error("username without tenant accepted")
	}
	dev, err := mqttConnect(t, s, "acme/pump", acmeDev)
	if err != nil {
		t.Fatalf("acme device connect: %v", err)
	}
	gsub, err := mqttConnect(t, s, "globex/screen", globexView)
	if err != nil {
		t.Fatal(err)
	}
	asub, err := mqttConnect(t, s, "acme/ops", acmeAdmin)
	if err != nil {
		t.Fatal(err)
	}
	granted := func(c paho.Client, filter string) bool {
		tok := c.Subscribe(filter, 0, func(paho.Client, paho.Message) {})
		tok.WaitTimeout(2 * time.Second)
		q := tok.(*paho.SubscribeToken).Result()[filter]
		return tok.Error() == nil && q != 0x80
	}
	for _, f := range []string{"#", "+/iot/#", "acme/#", "acme/iot/tank-1", "$SYS/#"} {
		if granted(gsub, f) {
			t.Errorf("globex viewer granted %q", f)
		}
	}
	gotGlobex := make(chan string, 10)
	gsub.Subscribe("globex/#", 0, func(_ paho.Client, m paho.Message) { gotGlobex <- m.Topic() }).Wait()
	gotAcme := make(chan string, 10)
	asub.Subscribe("acme/iot/#", 0, func(_ paho.Client, m paho.Message) { gotAcme <- string(m.Payload()) }).Wait()
	dev.Publish("globex/iot/tank-1", 0, false, `{"level":55}`).Wait() // denied
	dev.Publish("acme/iot/tank-2", 0, false, `{"level":5}`).Wait()
	select {
	case p := <-gotAcme:
		if !strings.Contains(p, `"level":5`) {
			t.Fatalf("acme subscriber got %s", p)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("acme publish not delivered to acme subscriber")
	}
	select {
	case topic := <-gotGlobex:
		t.Fatalf("globex subscriber received %s", topic)
	case <-time.After(300 * time.Millisecond):
	}
	if level(globexView) != 77.0 {
		t.Fatalf("acme device wrote into globex: %v", level(globexView))
	}
	if _, m, _ := call(t, "GET", s.url+"/api/sensors/tank-2", acmeAdmin, ""); m["last"].(map[string]any)["level"] != 5.0 {
		t.Fatal("MQTT reading not ingested into acme")
	}

	// Suspension: sessions dropped, API refused, data kept; resume restores.
	if code, _, raw := call(t, "PATCH", s.url+"/api/admin/tenants/acme", super, `{"status":"suspended"}`); code != 200 {
		t.Fatalf("suspend: %d %s", code, raw)
	}
	time.Sleep(200 * time.Millisecond)
	if dev.IsConnectionOpen() {
		t.Error("suspended tenant's MQTT session still open")
	}
	if code, _, _ := call(t, "GET", s.url+"/api/sensors", acmeAdmin, ""); code != 403 {
		t.Errorf("suspended tenant API: %d", code)
	}
	if code, _, _ := call(t, "POST", s.url+"/api/login", "", `{"token":"`+acmeAdmin+`"}`); code != 403 {
		t.Errorf("suspended tenant login: %d", code)
	}
	if _, err := mqttConnect(t, s, "acme/pump", acmeDev); err == nil {
		t.Error("suspended tenant MQTT connect accepted")
	}
	if level(globexView) != 77.0 {
		t.Fatal("suspending acme affected globex")
	}
	call(t, "PATCH", s.url+"/api/admin/tenants/acme", super, `{"status":"active"}`)
	if level(acmeAdmin) != 12.0 {
		t.Fatalf("after resume: %v", level(acmeAdmin))
	}

	// Deletion erases data and credentials; the id can be reused cleanly.
	if code, _, _ := call(t, "DELETE", s.url+"/api/admin/tenants/acme", super, ""); code != 400 {
		t.Errorf("delete without confirm: %d", code)
	}
	if code, _, _ := call(t, "DELETE", s.url+"/api/admin/tenants/acme?confirm=acme", super, ""); code != 204 {
		t.Fatalf("delete: %d", code)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "tenants", "acme")); !os.IsNotExist(err) {
		t.Error("acme's data directory remains")
	}
	if code, _, _ := call(t, "GET", s.url+"/api/sensors", acmeAdmin, ""); code != 401 {
		t.Errorf("deleted tenant's token: %d", code)
	}
	call(t, "POST", s.url+"/api/admin/tenants", super, `{"id":"acme"}`)
	newAdmin := issue(t, s, "acme", `{"id":"ops","role":"admin"}`)
	if _, m, _ := call(t, "GET", s.url+"/api/sensors", newAdmin, ""); m != nil {
		t.Fatalf("recreated tenant is not empty")
	}
	if level(globexView) != 77.0 {
		t.Fatal("deleting acme affected globex")
	}
}

func TestQuotasAndSelfService(t *testing.T) {
	s := start(t, "")
	call(t, "POST", s.url+"/api/admin/tenants", super, `{"id":"small","quota":{"messagesPerSecond":5,"maxDevices":2,"maxSensors":2}}`)
	adm := issue(t, s, "small", `{"id":"ops","role":"admin"}`)

	// Self-service is off by default: only the platform issues credentials.
	if code, _, _ := call(t, "POST", s.url+"/api/devices", adm, `{"id":"d1","role":"device","sensors":["*"]}`); code != 403 {
		t.Errorf("tenant admin issuing without self-service: %d", code)
	}
	if code, _, _ := call(t, "GET", s.url+"/api/devices", adm, ""); code != 200 {
		t.Errorf("tenant admin listing devices: %d", code)
	}
	call(t, "PATCH", s.url+"/api/admin/tenants/small", super, `{"deviceSelfService":true}`)
	if code, _, _ := call(t, "POST", s.url+"/api/devices", adm, `{"id":"d1","role":"device","sensors":["*"]}`); code != 201 {
		t.Errorf("with self-service: %d", code)
	}
	if code, _, _ := call(t, "POST", s.url+"/api/devices", adm, `{"id":"d2","role":"device","sensors":["*"]}`); code != 507 {
		t.Errorf("device quota (2): %d", code)
	}

	// Rate: burst of 2 s at 5/s, then 429.
	codes := map[int]int{}
	for i := range 30 {
		code, _, _ := call(t, "POST", s.url+"/api/sensors/m/data", adm, fmt.Sprintf(`{"v":%d}`, i))
		codes[code]++
	}
	if codes[202] < 10 || codes[202] > 12 || codes[429] == 0 {
		t.Errorf("rate limit outcomes %v (want ~10 accepted, rest 429)", codes)
	}
	// Sensor quota.
	time.Sleep(time.Second)
	call(t, "POST", s.url+"/api/sensors/m2/data", adm, `{"v":1}`)
	time.Sleep(300 * time.Millisecond)
	if code, _, _ := call(t, "POST", s.url+"/api/sensors/m3/data", adm, `{"v":1}`); code != 507 {
		t.Errorf("sensor quota: %d", code)
	}
	// Raising the quota applies at once.
	call(t, "PATCH", s.url+"/api/admin/tenants/small", super, `{"quota":{"messagesPerSecond":5,"maxDevices":2,"maxSensors":10}}`)
	time.Sleep(300 * time.Millisecond)
	if code, _, _ := call(t, "POST", s.url+"/api/sensors/m3/data", adm, `{"v":1}`); code != 202 {
		t.Errorf("after raising the quota: %d", code)
	}
	code, m, _ := call(t, "GET", s.url+"/api/admin/tenants/small", super, "")
	if code != 200 || m["messagesRejected"].(float64) == 0 || m["sensors"].(float64) != 3 {
		t.Errorf("usage %v", m)
	}
	// Bad tenant ids and duplicate creation.
	for _, bad := range []string{`{"id":"A"}`, `{"id":"admin"}`, `{"id":"x"}`, `{"id":"has_underscore"}`, `{"id":"ok-1","bogus":1}`} {
		if code, _, _ := call(t, "POST", s.url+"/api/admin/tenants", super, bad); code != 400 {
			t.Errorf("%s: %d", bad, code)
		}
	}
	if code, _, _ := call(t, "POST", s.url+"/api/admin/tenants", super, `{"id":"small"}`); code != 409 {
		t.Errorf("duplicate: %d", code)
	}
}
