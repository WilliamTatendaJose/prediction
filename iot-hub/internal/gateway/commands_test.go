package gateway_test

import (
	"encoding/json"
	"strings"
	"testing"
)

// Commands: admins define them, operators run the ones meant for them, a
// real MQTT device answers, and every run is kept.
func TestCommands(t *testing.T) {
	s := start(t, "")
	for _, tn := range []string{"acme", "globex"} {
		call(t, "POST", s.url+"/api/admin/tenants", super, `{"id":"`+tn+`"}`)
	}
	adm := issue(t, s, "acme", `{"id":"ops","role":"admin"}`)
	op := issue(t, s, "acme", `{"id":"shift","role":"operator"}`)
	view := issue(t, s, "acme", `{"id":"screen","role":"viewer"}`)
	devTok := issue(t, s, "acme", `{"id":"pump-1","role":"device","sensors":["pump-1"]}`)
	issue(t, s, "acme", `{"id":"valve-1","role":"device","sensors":["valve-1"]}`)
	gAdm := issue(t, s, "globex", `{"id":"ops","role":"admin"}`)

	define := func(tok, name, body string) int {
		code, _, raw := call(t, "PUT", s.url+"/api/commands/"+name, tok, body)
		if code >= 300 && tok == adm && !strings.Contains(strings.ToLower(name), "bad") {
			t.Fatalf("define %s: %d %s", name, code, raw)
		}
		return code
	}
	define(adm, "reboot", `{"label":"Reboot","devices":["pump-*"],"kind":"method","timeout":"3s","confirm":true,
		"params":[{"name":"delay","label":"Delay (s)","type":"number","min":0,"max":60,"default":5}]}`)
	define(adm, "wipe", `{"label":"Factory reset","kind":"method","role":"admin"}`)
	define(adm, "set-valve", `{"label":"Set valve","devices":["valve-*"],"kind":"message","payload":{"cmd":"valve"},
		"params":[{"name":"position","type":"choice","choices":["open","closed"],"required":true}]}`)
	for name, body := range map[string]string{
		"bad-kind":    `{"kind":"shell"}`,
		"bad-role":    `{"kind":"method","role":"root"}`,
		"bad-param":   `{"kind":"method","params":[{"name":"x","type":"date"}]}`,
		"bad-obj":     `{"kind":"method","payload":[1],"params":[{"name":"x","type":"text"}]}`,
		"bad-timeout": `{"kind":"method","timeout":"1h"}`,
		"Bad Name":    `{"kind":"method"}`,
	} {
		if code := define(adm, strings.ReplaceAll(name, " ", "%20"), body); code != 400 {
			t.Errorf("%s accepted: %d", name, code)
		}
	}
	if define(op, "reboot2", `{"kind":"method"}`) != 403 {
		t.Error("an operator defined a command")
	}

	if _, _, raw := call(t, "GET", s.url+"/api/commands", adm, ""); !strings.Contains(raw, `"history":[]`) {
		t.Errorf("no runs yet should be an empty list: %s", raw)
	}
	// What an operator sees: each device with the commands it may run.
	code, m, raw := call(t, "GET", s.url+"/api/commands", op, "")
	if code != 200 {
		t.Fatalf("list %d %s", code, raw)
	}
	offered := map[string]string{}
	for _, d := range m["devices"].([]any) {
		d := d.(map[string]any)
		b, _ := json.Marshal(d["commands"])
		offered[d["id"].(string)] = string(b)
	}
	if offered["pump-1"] != `["reboot"]` || offered["valve-1"] != `["set-valve"]` {
		t.Errorf("operator offered %v", offered)
	}
	if code, _, _ := call(t, "GET", s.url+"/api/commands", view, ""); code != 403 {
		t.Errorf("viewer listed commands: %d", code)
	}
	if code, m, _ := call(t, "GET", s.url+"/api/commands", gAdm, ""); code != 200 || len(m["commands"].([]any)) != 0 {
		t.Errorf("another tenant sees acme's catalog: %v", m)
	}

	// Offline: recorded as such, not an error.
	code, m, raw = call(t, "POST", s.url+"/api/devices/pump-1/commands/reboot", op, `{"params":{"delay":2}}`)
	if code != 200 || m["status"] != "offline" {
		t.Fatalf("offline run %d %s", code, raw)
	}

	dev, err := mqttConnect(t, s, "acme/pump-1", devTok)
	if err != nil {
		t.Fatal(err)
	}
	defer dev.Disconnect(10)
	box := &inbox{}
	if !subscribe(t, dev, "acme/devices/pump-1/#", box.handler) {
		t.Fatal("subscribe")
	}
	go func() {
		m := box.wait(t, "acme/devices/pump-1/methods/reboot/")
		rid := m.Topic()[strings.LastIndex(m.Topic(), "/")+1:]
		dev.Publish("acme/devices/pump-1/methods/res/200/"+rid, 0, false, `{"rebooting":true,"got":`+string(m.Payload())+`}`).Wait()
	}()
	code, m, raw = call(t, "POST", s.url+"/api/devices/pump-1/commands/reboot", op, `{"params":{"delay":7}}`)
	if code != 200 || m["status"] != "ok" || m["code"].(float64) != 200 || !strings.Contains(raw, `"got":{"delay":7}`) || m["by"] != "shift" {
		t.Fatalf("run %d %s", code, raw)
	}
	// Defaults fill missing parameters; ranges and unknown names are refused.
	go func() {
		m := box.wait(t, "acme/devices/pump-1/methods/reboot/")
		rid := m.Topic()[strings.LastIndex(m.Topic(), "/")+1:]
		dev.Publish("acme/devices/pump-1/methods/res/500/"+rid, 0, false, `{"error":"busy","got":`+string(m.Payload())+`}`).Wait()
	}()
	code, m, raw = call(t, "POST", s.url+"/api/devices/pump-1/commands/reboot", adm, ``)
	if code != 200 || m["status"] != "failed" || !strings.Contains(raw, `"got":{"delay":5}`) {
		t.Errorf("default/failed run %d %s", code, raw)
	}
	for body, want := range map[string]int{`{"params":{"delay":99}}`: 400, `{"params":{"speed":1}}`: 400, `{"params":{"delay":"x"}}`: 400} {
		if code, _, raw := call(t, "POST", s.url+"/api/devices/pump-1/commands/reboot", op, body); code != want {
			t.Errorf("%s: %d %s", body, code, raw)
		}
	}
	// Boundaries: admin-only command, command not offered here, viewer, other tenant.
	if code, _, _ := call(t, "POST", s.url+"/api/devices/pump-1/commands/wipe", op, ``); code != 403 {
		t.Errorf("operator ran an admin command: %d", code)
	}
	if code, _, _ := call(t, "POST", s.url+"/api/devices/valve-1/commands/reboot", adm, ``); code != 404 {
		t.Errorf("command run on a device it doesn't apply to: %d", code)
	}
	if code, _, _ := call(t, "POST", s.url+"/api/devices/pump-1/commands/reboot", view, ``); code != 403 {
		t.Errorf("viewer ran a command: %d", code)
	}
	if code, _, _ := call(t, "POST", s.url+"/api/devices/ops/commands/reboot", adm, ``); code != 404 {
		t.Errorf("command on a person: %d", code)
	}
	if code, _, _ := call(t, "POST", s.url+"/api/devices/pump-1/commands/reboot", gAdm, ``); code != 404 {
		t.Errorf("other tenant ran acme's command: %d", code)
	}

	// A message command: queued, required parameter, history follows the queue.
	if code, _, _ := call(t, "POST", s.url+"/api/devices/valve-1/commands/set-valve", op, `{"params":{}}`); code != 400 {
		t.Errorf("missing required parameter: %d", code)
	}
	code, m, raw = call(t, "POST", s.url+"/api/devices/valve-1/commands/set-valve", op, `{"params":{"position":"closed"}}`)
	if code != 200 || m["status"] != "queued" || !strings.Contains(raw, `"payload":{"cmd":"valve","position":"closed"}`) {
		t.Fatalf("message run %d %s", code, raw)
	}
	code, m, raw = call(t, "GET", s.url+"/api/devices/pump-1/commands", op, "")
	h := m["history"].([]any)
	if code != 200 || len(h) != 3 || h[0].(map[string]any)["status"] != "failed" || h[2].(map[string]any)["status"] != "offline" {
		t.Errorf("pump history %d %s", code, raw)
	}
	_, m, _ = call(t, "GET", s.url+"/api/commands", adm, "")
	if len(m["history"].([]any)) != 4 {
		t.Errorf("recent runs %v", m["history"])
	}
	_, _, raw = call(t, "GET", s.url+"/api/audit?limit=50", adm, "")
	if !strings.Contains(raw, `"command.run"`) || !strings.Contains(raw, `"command.define"`) {
		t.Error("commands not audited")
	}

	// Expected interval: admins set it, it shows on the twin, operators can't.
	if code, _, raw := call(t, "PUT", s.url+"/api/devices/pump-1/expected-interval", adm, `{"interval":"5m"}`); code != 204 {
		t.Fatalf("set expected %d %s", code, raw)
	}
	if _, m, _ := call(t, "GET", s.url+"/api/twins/pump-1", adm, ""); m["expectedIntervalSec"] != 300.0 {
		t.Errorf("twin %v", m["expectedIntervalSec"])
	}
	for body, want := range map[string]int{`{"interval":"10ms"}`: 400, `{"interval":"soon"}`: 400, `{"interval":""}`: 204} {
		if code, _, _ := call(t, "PUT", s.url+"/api/devices/pump-1/expected-interval", adm, body); code != want {
			t.Errorf("%s: %d", body, code)
		}
	}
	if code, _, _ := call(t, "PUT", s.url+"/api/devices/pump-1/expected-interval", op, `{"interval":"1m"}`); code != 403 {
		t.Errorf("operator set the interval: %d", code)
	}
	if code, _, _ := call(t, "DELETE", s.url+"/api/commands/wipe", adm, ""); code != 204 {
		t.Errorf("delete: %d", code)
	}
}
