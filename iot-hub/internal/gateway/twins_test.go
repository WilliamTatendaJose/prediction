package gateway_test

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
)

type inbox struct {
	mu  sync.Mutex
	got []paho.Message
}

func (b *inbox) handler(_ paho.Client, m paho.Message) { b.mu.Lock(); b.got = append(b.got, m); b.mu.Unlock() }

func (b *inbox) wait(t *testing.T, topicPrefix string) paho.Message {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		for i, m := range b.got {
			if strings.HasPrefix(m.Topic(), topicPrefix) {
				b.got = append(b.got[:i], b.got[i+1:]...)
				b.mu.Unlock()
				return m
			}
		}
		b.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("nothing on %s", topicPrefix)
	return nil
}

func (b *inbox) count() int { b.mu.Lock(); defer b.mu.Unlock(); return len(b.got) }

func subscribe(t *testing.T, c paho.Client, filter string, h paho.MessageHandler) bool {
	tok := c.Subscribe(filter, 0, h)
	tok.WaitTimeout(2 * time.Second)
	return tok.Error() == nil && tok.(*paho.SubscribeToken).Result()[filter] != 0x80
}

func TestTwinsMethodsMessages(t *testing.T) {
	s := start(t, "")
	for _, tn := range []string{"acme", "globex"} {
		call(t, "POST", s.url+"/api/admin/tenants", super, `{"id":"`+tn+`"}`)
	}
	adm := issue(t, s, "acme", `{"id":"ops","role":"admin"}`)
	view := issue(t, s, "acme", `{"id":"screen","role":"viewer"}`)
	devTok := issue(t, s, "acme", `{"id":"pump-1","role":"device","sensors":["pump-1"]}`)
	otherTok := issue(t, s, "acme", `{"id":"pump-2","role":"device","sensors":["pump-2"]}`)
	issue(t, s, "acme", `{"id":"pump-3","role":"device","sensors":["pump-3"]}`)
	gAdm := issue(t, s, "globex", `{"id":"ops","role":"admin"}`)

	dev, err := mqttConnect(t, s, "acme/pump-1", devTok)
	if err != nil {
		t.Fatal(err)
	}
	box := &inbox{}
	if !subscribe(t, dev, "acme/devices/pump-1/#", box.handler) {
		t.Fatal("device can't subscribe to its own topics")
	}
	// Eavesdroppers: a viewer with a wildcard, another device.
	spy := &inbox{}
	vc, _ := mqttConnect(t, s, "acme/screen", view)
	if !subscribe(t, vc, "acme/#", spy.handler) {
		t.Fatal("viewer wildcard on its tenant")
	}
	if subscribe(t, vc, "acme/devices/pump-1/#", spy.handler) {
		t.Error("viewer subscribed to a device's topics")
	}
	other, _ := mqttConnect(t, s, "acme/pump-2", otherTok)
	if subscribe(t, other, "acme/devices/pump-1/#", spy.handler) {
		t.Error("another device subscribed to pump-1's topics")
	}

	// Desired properties are pushed to the device, with $version.
	code, m, raw := call(t, "PATCH", s.url+"/api/twins/pump-1", adm, `{"tags":{"site":"plant1"},"properties":{"desired":{"interval":10}}}`)
	if code != 200 {
		t.Fatalf("patch twin %d %s", code, raw)
	}
	etag := m["etag"].(string)
	msg := box.wait(t, "acme/devices/pump-1/twin/desired")
	if string(msg.Payload()) != `{"$version":1,"interval":10}` {
		t.Fatalf("desired push %s", msg.Payload())
	}
	// Stale etag.
	if code, _, _ := call(t, "PATCH", s.url+"/api/twins/pump-1", adm, `{"tags":{"x":1}}`, "If-Match", `"1"`); code != 412 {
		t.Errorf("stale If-Match: %d", code)
	}
	_ = etag

	// The device reports, and reads its twin.
	dev.Publish("acme/devices/pump-1/twin/reported/7", 0, false, `{"firmware":"2.1.0","interval":10}`).Wait()
	res := box.wait(t, "acme/devices/pump-1/twin/res/")
	if res.Topic() != "acme/devices/pump-1/twin/res/204/7" || string(res.Payload()) != `{"version":1}` {
		t.Fatalf("reported ack %s %s", res.Topic(), res.Payload())
	}
	dev.Publish("acme/devices/pump-1/twin/get/8", 0, false, "").Wait()
	res = box.wait(t, "acme/devices/pump-1/twin/res/200/8")
	var dv map[string]map[string]any
	json.Unmarshal(res.Payload(), &dv)
	if dv["desired"]["interval"] != 10.0 || dv["reported"]["firmware"] != "2.1.0" || dv["tags"] != nil {
		t.Fatalf("device twin %s", res.Payload())
	}
	_, m, _ = call(t, "GET", s.url+"/api/twins/pump-1", adm, "")
	if m["connectionState"] != "Connected" || m["properties"].(map[string]any)["reported"].(map[string]any)["firmware"] != "2.1.0" {
		t.Fatalf("twin %v", m)
	}
	_, _, raw = call(t, "GET", s.url+"/api/twins?where=tags.site=plant1", adm, "")
	if !strings.Contains(raw, `"deviceId":"pump-1"`) || strings.Contains(raw, "pump-2") {
		t.Fatalf("query %s", raw)
	}

	// Direct method: the device answers on methods/res/{status}/{rid}.
	go func() {
		m := box.wait(t, "acme/devices/pump-1/methods/reboot/")
		rid := m.Topic()[strings.LastIndex(m.Topic(), "/")+1:]
		// pump-2 tries to answer first: refused by the ACL.
		other.Publish("acme/devices/pump-1/methods/res/200/"+rid, 0, false, `{"hijacked":true}`).Wait()
		dev.Publish("acme/devices/pump-1/methods/res/200/"+rid, 0, false, `{"rebooting":true,"delay":`+string(m.Payload())+`}`).Wait()
	}()
	code, m, raw = call(t, "POST", s.url+"/api/devices/pump-1/methods/reboot?timeout=5s", adm, `3`)
	if code != 200 || m["status"] != 200.0 || !strings.Contains(raw, `"delay":3`) || strings.Contains(raw, "hijacked") {
		t.Fatalf("method: %d %s", code, raw)
	}
	if code, _, _ := call(t, "POST", s.url+"/api/devices/pump-3/methods/reboot", adm, `{}`); code != 404 {
		t.Errorf("method on an offline device: %d", code)
	}

	// Cloud-to-device message to an offline device: queued, delivered when
	// it subscribes, completed by the device.
	dev.Disconnect(10)
	time.Sleep(100 * time.Millisecond)
	code, m, _ = call(t, "POST", s.url+"/api/devices/pump-1/messages?ttl=10m", adm, `{"cmd":"close-valve"}`)
	if code != 202 || m["status"] != "queued" {
		t.Fatalf("send: %d %v", code, m)
	}
	mid := m["id"].(string)
	dev, _ = mqttConnect(t, s, "acme/pump-1", devTok)
	box2 := &inbox{}
	subscribe(t, dev, "acme/devices/pump-1/messages/#", box2.handler)
	got := box2.wait(t, "acme/devices/pump-1/messages/"+mid)
	if string(got.Payload()) != `{"cmd":"close-valve"}` {
		t.Fatalf("message %s", got.Payload())
	}
	dev.Publish("acme/devices/pump-1/messages/complete/"+mid, 0, false, "").Wait()
	time.Sleep(200 * time.Millisecond)
	_, _, raw = call(t, "GET", s.url+"/api/devices/pump-1/messages", adm, "")
	if !strings.Contains(raw, `"status":"completed"`) {
		t.Fatalf("message status %s", raw)
	}

	// Nobody else saw any of it.
	if n := spy.count(); n != 0 {
		t.Fatalf("eavesdroppers received %d messages", n)
	}
	// Admins can't publish into device topics (no impersonating the hub).
	ac, _ := mqttConnect(t, s, "acme/ops", adm)
	ac.Publish("acme/devices/pump-1/twin/desired", 0, false, `{"interval":1}`).Wait()
	time.Sleep(200 * time.Millisecond)
	if v, _, _ := call(t, "GET", s.url+"/api/twins/pump-1", adm, ""); v != 200 {
		t.Fatal(v)
	}
	if box.count() != 0 || box2.count() != 0 {
		t.Fatalf("admin MQTT publish reached the device")
	}
	// Other tenants and other roles.
	if code, _, _ := call(t, "GET", s.url+"/api/twins/pump-1", gAdm, ""); code != 404 {
		t.Errorf("globex reading acme's twin: %d", code)
	}
	if code, _, _ := call(t, "POST", s.url+"/api/devices/pump-1/methods/reboot", view, `{}`); code != 403 {
		t.Errorf("viewer invoking a method: %d", code)
	}
	if code, _, _ := call(t, "PATCH", s.url+"/api/twins/pump-1", devTok, `{"properties":{"desired":{"x":1}}}`); code != 403 {
		t.Errorf("device writing its own desired: %d", code)
	}

	// The device's own HTTP side.
	code, m, _ = call(t, "GET", s.url+"/api/device/twin", otherTok, "")
	if code != 200 || m["desired"] == nil {
		t.Fatalf("device http twin %d %v", code, m)
	}
	if code, _, _ := call(t, "PATCH", s.url+"/api/device/twin/reported", otherTok, `{"battery":87}`); code != 200 {
		t.Errorf("device http report %d", code)
	}
	call(t, "POST", s.url+"/api/devices/pump-2/messages", adm, `"hello"`)
	code, m, _ = call(t, "GET", s.url+"/api/device/messages", otherTok, "")
	if code != 200 || m["body"] != "hello" {
		t.Fatalf("device http receive %d %v", code, m)
	}
	if code, _, _ := call(t, "POST", s.url+"/api/device/messages/"+m["id"].(string)+"/complete", otherTok, ""); code != 204 {
		t.Errorf("device http complete %d", code)
	}
	if code, _, _ := call(t, "GET", s.url+"/api/device/messages", otherTok, ""); code != 204 {
		t.Errorf("empty queue %d", code)
	}
	if code, _, _ := call(t, "GET", s.url+"/api/device/twin", view, ""); code != 403 {
		t.Errorf("viewer has no twin: %d", code)
	}
	// Deleting the device removes its twin; recreating starts clean.
	call(t, "DELETE", s.url+"/api/devices/pump-2", super, "", "X-Tenant", "acme") // credentials are the platform's here
	issue(t, s, "acme", `{"id":"pump-2","role":"device","sensors":["pump-2"]}`)
	_, m, _ = call(t, "GET", s.url+"/api/twins/pump-2", adm, "")
	if rep := m["properties"].(map[string]any)["reported"].(map[string]any); rep["battery"] != nil {
		t.Fatalf("old twin survived: %v", rep)
	}
}
