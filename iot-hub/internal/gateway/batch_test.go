package gateway_test

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// One command to many devices: runs in the background, reports each
// device, skips what the caller may not command, records every run.
func TestCommandBatch(t *testing.T) {
	s := start(t, "")
	call(t, "POST", s.url+"/api/admin/tenants", super, `{"id":"acme"}`)
	adm := issue(t, s, "acme", `{"id":"ops","role":"admin"}`)
	op := issue(t, s, "acme", `{"id":"shift","role":"operator"}`)
	view := issue(t, s, "acme", `{"id":"screen","role":"viewer"}`)
	tok := issue(t, s, "acme", `{"id":"pump-1","role":"device","sensors":["pump-1"]}`)
	tok2 := issue(t, s, "acme", `{"id":"pump-2","role":"device","sensors":["pump-2"]}`) // offline until the resend
	valveTok := issue(t, s, "acme", `{"id":"valve-1","role":"device","sensors":["valve-1"]}`)
	issue(t, s, "acme", `{"id":"valve-2","role":"device","sensors":["valve-2"]}`)
	call(t, "PUT", s.url+"/api/commands/reboot", adm, `{"devices":["pump-*"],"kind":"method","timeout":"2s",
		"params":[{"name":"delay","type":"number","min":0,"max":60,"default":5}]}`)
	call(t, "PUT", s.url+"/api/commands/close", adm, `{"devices":["valve-*"],"kind":"message","payload":{"cmd":"close"}}`)
	call(t, "PUT", s.url+"/api/commands/wipe", adm, `{"kind":"method","role":"admin"}`)

	dev, err := mqttConnect(t, s, "acme/pump-1", tok)
	if err != nil {
		t.Fatal(err)
	}
	defer dev.Disconnect(10)
	box := &inbox{}
	subscribe(t, dev, "acme/devices/pump-1/#", box.handler)
	go func() {
		m := box.wait(t, "acme/devices/pump-1/methods/reboot/")
		time.Sleep(300 * time.Millisecond) // answers while the batch is still running
		dev.Publish("acme/devices/pump-1/methods/res/200/"+m.Topic()[strings.LastIndex(m.Topic(), "/")+1:], 0, false, `{"ok":true}`).Wait()
	}()

	code, m, raw := call(t, "POST", s.url+"/api/commands/reboot/run", op,
		`{"devices":["pump-1","pump-2","valve-1","ops","ghost","pump-1"],"params":{"delay":1}}`)
	if code != 202 || m["total"].(float64) != 5 {
		t.Fatalf("start %d %s", code, raw)
	}
	id := m["id"].(string)
	// Started at once: the reboot calls are still in flight.
	if !strings.Contains(raw, `"pending"`) || m["finished"] != nil {
		t.Errorf("batch should still be running: %s", raw)
	}
	var b map[string]any
	for i := 0; i < 50; i++ {
		_, b, raw = call(t, "GET", s.url+"/api/commands/batches/"+id, op, "")
		if b["finished"] != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if b["finished"] == nil {
		t.Fatalf("batch never finished: %s", raw)
	}
	got := map[string]string{}
	for _, r := range b["results"].([]any) {
		r := r.(map[string]any)
		got[r["device"].(string)] = fmt.Sprint(r["status"], " ", r["error"])
	}
	want := map[string]string{"pump-1": "ok", "pump-2": "offline", "valve-1": "skipped", "ops": "skipped", "ghost": "skipped"}
	for d, st := range want {
		if !strings.HasPrefix(got[d], st) {
			t.Errorf("%s: %q, want %s", d, got[d], st)
		}
	}
	if c := b["counts"].(map[string]any); c["ok"] != 1.0 || c["offline"] != 1.0 || c["skipped"] != 3.0 || b["done"] != 5.0 {
		t.Errorf("counts %v done %v", c, b["done"])
	}
	// Each run is in the device's own history, tagged with the batch.
	_, _, raw = call(t, "GET", s.url+"/api/devices/pump-1/commands", op, "")
	if !strings.Contains(raw, `"batch":"`+id+`"`) || !strings.Contains(raw, `"payload":{"delay":1}`) {
		t.Errorf("pump-1 history %s", raw)
	}
	_, _, raw = call(t, "GET", s.url+"/api/commands", op, "")
	if !strings.Contains(raw, `"batches":[{"id":"`+id+`"`) {
		t.Errorf("recent batches %s", raw)
	}

	// Resend to the failed: only pump-2 (offline) goes again, with the same
	// parameters; this time it is connected and answers.
	if code, _, _ := call(t, "POST", s.url+"/api/commands/batches/"+id+"/resend", view, ""); code != 403 {
		t.Errorf("viewer resent: %d", code)
	}
	dev2, err := mqttConnect(t, s, "acme/pump-2", tok2)
	if err != nil {
		t.Fatal(err)
	}
	defer dev2.Disconnect(10)
	box2 := &inbox{}
	subscribe(t, dev2, "acme/devices/pump-2/#", box2.handler)
	go func() {
		m := box2.wait(t, "acme/devices/pump-2/methods/reboot/")
		dev2.Publish("acme/devices/pump-2/methods/res/200/"+m.Topic()[strings.LastIndex(m.Topic(), "/")+1:], 0, false, `{"got":`+string(m.Payload())+`}`).Wait()
	}()
	code, m2, raw := call(t, "POST", s.url+"/api/commands/batches/"+id+"/resend", op, "")
	if code != 202 || m2["total"].(float64) != 1 || m2["retryOf"] != id || !strings.Contains(raw, `"device":"pump-2"`) {
		t.Fatalf("resend %d %s", code, raw)
	}
	rid := m2["id"].(string)
	for i := 0; i < 50; i++ {
		if _, m2, raw = call(t, "GET", s.url+"/api/commands/batches/"+rid, op, ""); m2["finished"] != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if c := m2["counts"].(map[string]any); c["ok"] != 1.0 || m2["by"] != "shift" {
		t.Errorf("resent batch %s", raw)
	}
	_, _, raw = call(t, "GET", s.url+"/api/devices/pump-2/commands", op, "")
	if !strings.Contains(raw, `"batch":"`+rid+`"`) || !strings.Contains(raw, `"payload":{"delay":1}`) {
		t.Errorf("pump-2 history after resend %s", raw)
	}
	_, orig, _ := call(t, "GET", s.url+"/api/commands/batches/"+id, op, "")
	if fmt.Sprint(orig["retries"]) != "["+rid+"]" {
		t.Errorf("original batch doesn't link its resend: %v", orig["retries"])
	}
	// The resend has nothing left that failed.
	if code, _, raw := call(t, "POST", s.url+"/api/commands/batches/"+rid+"/resend", op, ""); code != 409 {
		t.Errorf("resend of a batch with no failures: %d %s", code, raw)
	}

	// A message batch: queued for every valve.
	code, m, raw = call(t, "POST", s.url+"/api/commands/close/run", op, `{"devices":["valve-1","valve-2"]}`)
	if code != 202 {
		t.Fatalf("message batch %d %s", code, raw)
	}
	time.Sleep(300 * time.Millisecond)
	_, b, raw = call(t, "GET", s.url+"/api/commands/batches/"+m["id"].(string), op, "")
	if c := b["counts"].(map[string]any); c["queued"] != 2.0 {
		t.Errorf("message batch %s", raw)
	}
	// Queued messages haven't failed: nothing to resend.
	if code, _, _ := call(t, "POST", s.url+"/api/commands/batches/"+m["id"].(string)+"/resend", op, ""); code != 409 {
		t.Errorf("resend of queued messages: %d", code)
	}
	// valve-1 picks its message up over HTTP and completes it: the batch
	// follows the queue.
	code, msg, raw := call(t, "GET", s.url+"/api/device/messages", valveTok, "")
	if code != 200 || msg["id"] == nil {
		t.Fatalf("receive %d %s", code, raw)
	}
	if code, _, raw := call(t, "POST", s.url+"/api/device/messages/"+msg["id"].(string)+"/complete", valveTok, ""); code >= 300 {
		t.Fatalf("complete %d %s", code, raw)
	}
	_, b, raw = call(t, "GET", s.url+"/api/commands/batches/"+m["id"].(string), op, "")
	if c := b["counts"].(map[string]any); c["completed"] != 1.0 || c["queued"] != 1.0 {
		t.Errorf("batch after completion %s", raw)
	}

	// Refused before anything is sent.
	for _, tc := range []struct {
		tok, path, body string
		want            int
	}{
		{op, "reboot", `{"devices":["pump-1"],"params":{"delay":99}}`, 400}, // bad parameter
		{op, "reboot", `{"devices":[]}`, 400},                               // nothing chosen
		{op, "reboot", `{"devices":["valve-1"]}`, 400},                      // nothing it applies to
		{op, "wipe", `{"devices":["pump-1","valve-1"]}`, 400},               // admin-only: all skipped
		{view, "reboot", `{"devices":["pump-1"]}`, 403},
		{op, "nope", `{"devices":["pump-1"]}`, 404},
	} {
		if code, _, raw := call(t, "POST", s.url+"/api/commands/"+tc.path+"/run", tc.tok, tc.body); code != tc.want {
			t.Errorf("%s %s: %d %s", tc.path, tc.body, code, raw)
		}
	}
	if code, _, _ := call(t, "GET", s.url+"/api/commands/batches/"+id, view, ""); code != 403 {
		t.Errorf("viewer read a batch: %d", code)
	}
	_, _, raw = call(t, "GET", s.url+"/api/audit?limit=50", adm, "")
	if !strings.Contains(raw, `"command.batch"`) {
		t.Error("batch not audited")
	}
}
