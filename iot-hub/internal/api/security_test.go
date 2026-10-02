package api_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"math/big"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/williamtatendajose/prediction/iot-hub/internal/analytics"
	"github.com/williamtatendajose/prediction/iot-hub/internal/api"
	"github.com/williamtatendajose/prediction/iot-hub/internal/auth"
	"github.com/williamtatendajose/prediction/iot-hub/internal/broker"
	"github.com/williamtatendajose/prediction/iot-hub/internal/ingest"
	"github.com/williamtatendajose/prediction/iot-hub/internal/store"
	"github.com/williamtatendajose/prediction/iot-hub/internal/stream"
)

type secEnv struct {
	url, mqtt, mqtts string
	st               *store.Store
	tlsCfg           *tls.Config
	pool             *x509.CertPool
}

const adminTok = "admin-secret"

func secure(t *testing.T, publicRead bool) secEnv {
	st := store.New(store.Options{AutoRegister: true})
	hub := stream.NewHub(16)
	pipe := &ingest.Pipeline{Store: st, Hub: hub}
	creds := auth.New(filepath.Join(t.TempDir(), "devices.json"), adminTok)
	cfg, pool := selfSigned(t)
	addr, tlsAddr := freeAddr(t), freeAddr(t)
	b, err := broker.New(broker.Config{TCPAddr: addr, TLSAddr: tlsAddr, TLS: cfg, Auth: creds}, pipe)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Serve(); err != nil {
		t.Fatal(err)
	}
	srv := &api.Server{Store: st, Hub: hub, Pipeline: pipe, Auth: creds, PublicRead: publicRead,
		Analytics: &analytics.Service{Store: st}, OnRevoke: func(id string) { b.Kick(id) }}
	h := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { h.Close(); b.Close() })
	return secEnv{url: h.URL, mqtt: "tcp://" + addr, mqtts: "tls://" + tlsAddr, st: st, tlsCfg: cfg, pool: pool}
}

func selfSigned(t *testing.T) (*tls.Config, *x509.CertPool) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "iothub-test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12}, pool
}

func do(t *testing.T, c *http.Client, method, url, token, body string, hdr ...string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	if c == nil {
		c = http.DefaultClient
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(res.Body).Decode(&m)
	return res.StatusCode, m
}

func addDevice(t *testing.T, e secEnv, id, role string, sensors ...string) string {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"id": id, "role": role, "sensors": sensors})
	code, m := do(t, nil, "POST", e.url+"/api/devices", adminTok, string(b))
	if code != 201 {
		t.Fatalf("add device %s: %d %v", id, code, m)
	}
	return m["token"].(string)
}

func TestHTTPRoles(t *testing.T) {
	e := secure(t, false)
	dev := addDevice(t, e, "env-node", "device", "env-*")
	view := addDevice(t, e, "screen", "viewer")
	svc := addDevice(t, e, "ml", "service", "*-ml")

	cases := []struct {
		name, method, path, token, body string
		want                            int
	}{
		{"no token read", "GET", "/api/sensors", "", "", 401},
		{"bad token", "GET", "/api/sensors", "nope", "", 401},
		{"device ingest own", "POST", "/api/sensors/env-1/data", dev, `{"t":1}`, 202},
		{"device ingest other", "POST", "/api/sensors/power-1/data", dev, `{"t":1}`, 403},
		{"device read", "GET", "/api/sensors", dev, "", 403},
		{"device define", "PUT", "/api/sensors/env-1", dev, `{}`, 403},
		{"viewer read", "GET", "/api/sensors", view, "", 200},
		{"viewer stream denied write", "POST", "/api/sensors/env-1/data", view, `1`, 403},
		{"viewer dashboard write", "PUT", "/api/dashboard", view, `{"tiles":[]}`, 403},
		{"service define own", "PUT", "/api/sensors/env-1-ml", svc, `{"fields":{}}`, 200},
		{"service define other", "PUT", "/api/sensors/env-1", svc, `{"fields":{}}`, 403},
		{"service list devices", "GET", "/api/devices", svc, "", 403},
		{"admin dashboard", "PUT", "/api/dashboard", adminTok, `{"tiles":[]}`, 204},
		{"admin devices", "GET", "/api/devices", adminTok, "", 200},
		{"health is public", "GET", "/api/health", "", "", 200},
	}
	for _, c := range cases {
		if code, m := do(t, nil, c.method, e.url+c.path, c.token, c.body); code != c.want {
			t.Errorf("%s: got %d want %d (%v)", c.name, code, c.want, m)
		}
	}
	// The device list never exposes hashes or tokens.
	req, _ := http.NewRequest("GET", e.url+"/api/devices", nil)
	req.Header.Set("Authorization", "Bearer "+adminTok)
	res, _ := http.DefaultClient.Do(req)
	var raw strings.Builder
	buf := make([]byte, 4096)
	n, _ := res.Body.Read(buf)
	raw.Write(buf[:n])
	res.Body.Close()
	if strings.Contains(raw.String(), "hash") || strings.Contains(raw.String(), dev) {
		t.Fatalf("device list leaks secrets: %s", raw.String())
	}
	if res.Header.Get("Content-Security-Policy") == "" || res.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatal("security headers missing")
	}
}

func TestPublicRead(t *testing.T) {
	e := secure(t, true)
	if code, _ := do(t, nil, "GET", e.url+"/api/sensors", "", ""); code != 200 {
		t.Fatalf("public read: %d", code)
	}
	if code, _ := do(t, nil, "POST", e.url+"/api/sensors/x/data", "", "1"); code != 401 {
		t.Fatalf("writes still need a token: %d", code)
	}
}

func TestCookieLoginAndCSRF(t *testing.T) {
	e := secure(t, false)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	if code, _ := do(t, c, "POST", e.url+"/api/login", "", `{"token":"wrong"}`); code != 401 {
		t.Fatalf("bad login: %d", code)
	}
	code, m := do(t, c, "POST", e.url+"/api/login", "", `{"token":"`+adminTok+`"}`)
	if code != 200 || m["role"] != "superadmin" {
		t.Fatalf("login: %d %v", code, m)
	}
	if code, _ := do(t, c, "GET", e.url+"/api/sensors", "", ""); code != 200 {
		t.Fatalf("cookie read: %d", code)
	}
	if code, _ := do(t, c, "PUT", e.url+"/api/dashboard", "", `{"tiles":[]}`); code != 403 {
		t.Fatalf("cookie write without CSRF header must fail: %d", code)
	}
	if code, _ := do(t, c, "PUT", e.url+"/api/dashboard", "", `{"tiles":[]}`, "X-Requested-With", "iothub"); code != 204 {
		t.Fatalf("cookie write with header: %d", code)
	}
	do(t, c, "POST", e.url+"/api/logout", "", "")
	if code, _ := do(t, c, "GET", e.url+"/api/sensors", "", ""); code != 401 {
		t.Fatalf("after logout: %d", code)
	}
}

func connect(t *testing.T, url, user, pass string, tlsCfg *tls.Config) (paho.Client, error) {
	o := paho.NewClientOptions().AddBroker(url).SetClientID(user + "-" + t.Name()).
		SetUsername(user).SetPassword(pass).SetConnectTimeout(3 * time.Second).SetAutoReconnect(false)
	if tlsCfg != nil {
		o.SetTLSConfig(tlsCfg)
	}
	c := paho.NewClient(o)
	tok := c.Connect()
	tok.WaitTimeout(4 * time.Second)
	return c, tok.Error()
}

func TestMQTTACLAndRevocation(t *testing.T) {
	e := secure(t, false)
	dev := addDevice(t, e, "env-node", "device", "env-*")
	view := addDevice(t, e, "screen", "viewer")

	if _, err := connect(t, e.mqtt, "someone-else", dev, nil); err == nil {
		t.Fatal("username must match the device id")
	}
	if _, err := connect(t, e.mqtt, "env-node", "bad", nil); err == nil {
		t.Fatal("bad password accepted")
	}
	d, err := connect(t, e.mqtt, "env-node", dev, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Allowed and forbidden publishes; forbidden ones are dropped by the ACL.
	d.Publish("iot/env-1", 0, false, `{"t":21}`).Wait()
	d.Publish("iot/power-1", 0, false, `{"kw":5}`).Wait()
	d.Publish("iot-events/anomaly/env-1", 0, false, `{"fake":true}`).Wait()
	// A device cannot subscribe.
	got := make(chan struct{}, 1)
	d.Subscribe("iot/#", 0, func(paho.Client, paho.Message) { got <- struct{}{} }).Wait()

	v, err := connect(t, e.mqtt, "screen", view, nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(chan string, 4)
	v.Subscribe("iot/#", 0, func(_ paho.Client, m paho.Message) { seen <- m.Topic() }).Wait()
	if tok := v.Publish("iot/env-1", 0, false, `{"t":99}`); tok.Wait() && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	d.Publish("iot/env-2", 0, false, `{"t":22}`).Wait()
	select {
	case topic := <-seen:
		if topic != "iot/env-2" {
			t.Fatalf("viewer saw %s", topic)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("viewer subscription got nothing")
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := e.st.Get("power-1"); err == nil {
		t.Fatal("device wrote a sensor outside its patterns")
	}
	if sv, _ := e.st.Get("env-1"); sv.Last["t"] != 21.0 {
		t.Fatalf("viewer publish must be dropped; env-1 = %v", sv.Last)
	}
	select {
	case <-got:
		t.Fatal("device subscription received data")
	default:
	}

	// Revocation drops the live session and blocks reconnects.
	if code, _ := do(t, nil, "DELETE", e.url+"/api/devices/env-node", adminTok, ""); code != 204 {
		t.Fatalf("revoke: %d", code)
	}
	deadline := time.Now().Add(2 * time.Second)
	for d.IsConnectionOpen() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if d.IsConnectionOpen() {
		t.Fatal("revoked session still connected")
	}
	if _, err := connect(t, e.mqtt, "env-node", dev, nil); err == nil {
		t.Fatal("revoked token reconnected")
	}
}

func TestMQTTOverTLS(t *testing.T) {
	e := secure(t, false)
	dev := addDevice(t, e, "env-node", "device", "env-*")
	c, err := connect(t, e.mqtts, "env-node", dev, &tls.Config{RootCAs: e.pool, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatalf("tls connect: %v", err)
	}
	c.Publish("iot/env-9", 0, false, `{"t":1}`).Wait()
	time.Sleep(100 * time.Millisecond)
	if _, err := e.st.Get("env-9"); err != nil {
		t.Fatal("data over TLS not ingested")
	}
	if _, err := connect(t, e.mqtts, "env-node", dev, &tls.Config{MinVersion: tls.VersionTLS12}); err == nil {
		t.Fatal("untrusted certificate must fail verification")
	}
}

func TestDevicesNeedAuthEnabled(t *testing.T) {
	st := store.New(store.Options{})
	srv := &api.Server{Store: st, Hub: stream.NewHub(1), Analytics: &analytics.Service{Store: st}}
	h := httptest.NewServer(srv.Handler())
	defer h.Close()
	if code, _ := do(t, nil, "POST", h.URL+"/api/devices", "", `{"id":"x","role":"viewer"}`); code != 409 {
		t.Fatalf("adding a device to an open hub must be refused: %d", code)
	}
}
