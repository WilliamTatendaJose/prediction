package tenant_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/williamtatendajose/prediction/iot-hub/internal/auth"
	"github.com/williamtatendajose/prediction/iot-hub/internal/broker"
	"github.com/williamtatendajose/prediction/iot-hub/internal/tenant"
)

// Single-tenant: device topics have no tenant prefix (devices/{id}/…), and
// twins survive a restart of the runtime.
func TestSingleTenantTwins(t *testing.T) {
	dir := t.TempDir()
	creds := auth.New(filepath.Join(dir, "auth.json"), "adm")
	tok, err := creds.Add("pump-1", auth.Device, []string{"pump-1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	var mq *broker.Broker
	open := func() *tenant.Runtime {
		rt, err := tenant.Open(context.Background(), auth.DefaultTenant, tenant.Options{
			ConfigPath: filepath.Join(dir, "iothub.json"), TwinsPath: filepath.Join(dir, "twins.json"), Creds: creds,
			Capacity: 16, MaxSensors: 10, MaxFields: 4, AutoRegister: true,
			Hooks: tenant.Hooks{
				DeviceListening: func(tn, id, sub string) bool { return mq.DeviceListening(tn, id, sub) },
				DeviceConnected: func(tn, id string) bool { return mq.DeviceConnected(tn, id) },
				SendToDevice:    func(tn, id, sub string, p []byte) int { return mq.SendToDevice(tn, id, sub, p) },
			}})
		if err != nil {
			t.Fatal(err)
		}
		return rt
	}
	rt := open()
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	mq, err = broker.New(broker.Config{TCPAddr: addr, Auth: creds, Twins: func(string) broker.DeviceHandler { return rt.Twins }}, rt.Pipe)
	if err != nil {
		t.Fatal(err)
	}
	mq.Serve()
	defer mq.Close()
	h := httptest.NewServer(rt.Handler)
	defer h.Close()

	c := paho.NewClient(paho.NewClientOptions().AddBroker("tcp://"+addr).SetClientID("pump-1").SetUsername("pump-1").SetPassword(tok))
	if tk := c.Connect(); !tk.WaitTimeout(3*time.Second) || tk.Error() != nil {
		t.Fatal(tk.Error())
	}
	defer c.Disconnect(10)
	got := make(chan paho.Message, 10)
	c.Subscribe("devices/pump-1/#", 0, func(_ paho.Client, m paho.Message) { got <- m }).Wait()

	req, _ := http.NewRequest("PATCH", h.URL+"/api/twins/pump-1", strings.NewReader(`{"properties":{"desired":{"setpoint":42}}}`))
	req.Header.Set("Authorization", "Bearer adm")
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 200 {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("patch %v %s", err, b)
	}
	select {
	case m := <-got:
		if m.Topic() != "devices/pump-1/twin/desired" || !strings.Contains(string(m.Payload()), `"setpoint":42`) {
			t.Fatalf("%s %s", m.Topic(), m.Payload())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("desired not delivered")
	}
	c.Publish("devices/pump-1/twin/reported", 0, false, `{"setpoint":42}`).Wait()
	<-got // the ack
	rt.Close() // saves twins.json
	rt = open()
	defer rt.Close()
	v, err := rt.Twins.Get("pump-1")
	b, _ := json.Marshal(v)
	if err != nil || !strings.Contains(string(b), `"reported":{"$lastUpdated"`) || v.Properties.Reported["setpoint"] != 42.0 || v.Properties.Desired["setpoint"] != 42.0 {
		t.Fatalf("after restart: %s %v", b, err)
	}
}
