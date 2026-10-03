package gateway_test

import (
	"strings"
	"testing"
	"time"
)

// "Last data" is per identity and per tenant, whatever the transport.
func TestDeviceLastData(t *testing.T) {
	s := start(t, "")
	for _, tn := range []string{"acme", "globex"} {
		call(t, "POST", s.url+"/api/admin/tenants", super, `{"id":"`+tn+`"}`)
	}
	adm := issue(t, s, "acme", `{"id":"ops","role":"admin"}`)
	mq := issue(t, s, "acme", `{"id":"m1","role":"device","sensors":["m1"]}`)
	web := issue(t, s, "acme", `{"id":"h1","role":"device","sensors":["h1"]}`)
	edge := issue(t, s, "acme", `{"id":"edge","role":"service","sensors":["*"]}`)
	issue(t, s, "acme", `{"id":"quiet","role":"device","sensors":["quiet"]}`)
	issue(t, s, "globex", `{"id":"m1","role":"device","sensors":["m1"]}`)

	before := time.Now().UnixMilli()
	c, err := mqttConnect(t, s, "acme/m1", mq)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Disconnect(10)
	c.Publish("acme/iot/other", 0, false, `{"v":1}`).Wait() // not its sensor: refused, no data
	time.Sleep(300 * time.Millisecond)
	last := func(id string) float64 {
		code, m, raw := call(t, "GET", s.url+"/api/twins/"+id, adm, "")
		if code != 200 {
			t.Fatalf("twin %s: %d %s", id, code, raw)
		}
		v, _ := m["lastDataTime"].(float64)
		return v
	}
	if last("m1") != 0 {
		t.Error("a refused publish counted as data")
	}
	c.Publish("acme/iot/m1", 0, false, `{"v":1}`).Wait()
	if code, _, _ := call(t, "POST", s.url+"/api/sensors/h1/data", web, `{"v":2}`); code != 202 {
		t.Fatalf("http ingest %d", code)
	}
	if code, _, raw := call(t, "POST", s.url+"/api/ingest/batch", edge, `{"s":"e1","v":{"v":3}}`+"\n"); code != 200 {
		t.Fatalf("batch %d %s", code, raw)
	}
	call(t, "POST", s.url+"/api/sensors/a1/data", adm, `{"v":4}`) // a person: no twin
	time.Sleep(300 * time.Millisecond)
	for _, id := range []string{"m1", "h1", "edge"} {
		if v := last(id); v < float64(before) {
			t.Errorf("%s last data %v, want ≥ %d", id, v, before)
		}
	}
	if last("quiet") != 0 {
		t.Error("a device that never sent has a last-data time")
	}
	_, _, raw := call(t, "GET", s.url+"/api/twins", adm, "")
	if strings.Contains(raw, `"deviceId":"ops"`) {
		t.Error("an admin's data created a twin")
	}
	// The other tenant's device with the same id is untouched.
	_, m, _ := call(t, "GET", s.url+"/api/twins/m1", super, "", "X-Tenant", "globex")
	if m["lastDataTime"] != nil {
		t.Error("data leaked across tenants")
	}
}
