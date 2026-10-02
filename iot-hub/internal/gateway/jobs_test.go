package gateway_test

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestStreamJobs(t *testing.T) {
	s := start(t, "")
	call(t, "POST", s.url+"/api/admin/tenants", super, `{"id":"acme","quota":{"maxJobs":3}}`)
	adm := issue(t, s, "acme", `{"id":"ops","role":"admin"}`)
	put := func(id, body string) (int, string) {
		code, _, raw := call(t, "PUT", s.url+"/api/jobs/"+id, adm, body)
		return code, raw
	}
	if code, raw := put("tank-avg", `{"enabled":true,"lateness":"0s","query":"SELECT avg(level) INTO [avg-{sensor}] FROM [tank-*] GROUP BY sensor, TumblingWindow(second, 10)"}`); code != 200 {
		t.Fatalf("put job: %d %s", code, raw)
	}
	if code, raw := put("door", `{"enabled":true,"lateness":"0s","query":"SELECT count(open) INTO alert FROM [door-1] WHERE value = 1 GROUP BY SlidingWindow(minute, 10) HAVING count >= 3"}`); code != 200 {
		t.Fatalf("put alert job: %d %s", code, raw)
	}
	// Jobs can't feed each other, and bad queries say why.
	if code, raw := put("chain", `{"enabled":true,"query":"SELECT max(avg_level) INTO [plant] FROM [avg-*] GROUP BY TumblingWindow(minute, 1)"}`); code != 400 || !strings.Contains(raw, "feed each other") {
		t.Errorf("chained job: %d %s", code, raw)
	}
	if code, raw := put("bad", `{"enabled":true,"query":"SELECT avg(level) FROM [tank-*]"}`); code != 400 || !strings.Contains(raw, "GROUP BY") {
		t.Errorf("bad query: %d %s", code, raw)
	}

	// Live readings: two 10 s windows for tank-1, then one that moves time on.
	base := time.Now().Add(-time.Minute).Truncate(10 * time.Second)
	for i, v := range []float64{10, 20, 30, 40} {
		call(t, "POST", s.url+"/api/sensors/tank-1/data", adm, fmt.Sprintf(`{"level":%v,"ts":%d}`, v, base.Add(time.Duration(i*3)*time.Second).UnixMilli()))
	}
	call(t, "POST", s.url+"/api/sensors/tank-1/data", adm, fmt.Sprintf(`{"level":0,"ts":%d}`, base.Add(25*time.Second).UnixMilli()))
	// window [0,10): 10,20,30,40 (0,3,6,9 s) → 25; window [10,20) is empty; the
	// 25 s reading closes [0,10).
	time.Sleep(200 * time.Millisecond)
	_, m, raw := call(t, "GET", s.url+"/api/sensors/avg-tank-1", adm, "")
	if last, _ := m["last"].(map[string]any); last == nil || last["avg_level"] != 25.0 {
		t.Fatalf("derived sensor: %s", raw)
	}

	// The alert job opens a "rule" alarm that operators handle like any other.
	for i := 0; i < 3; i++ {
		call(t, "POST", s.url+"/api/sensors/door-1/data", adm, `{"open":1}`)
	}
	_, _, raw = call(t, "GET", s.url+"/api/anomalies?active=1", adm, "")
	if !strings.Contains(raw, `"kind":"rule"`) || !strings.Contains(raw, "door: count(open) = 3") {
		t.Fatalf("rule alarm: %s", raw)
	}
	id := raw[strings.Index(raw, `"id":"`)+6:]
	id = id[:strings.Index(id, `"`)]
	if code, _, raw := call(t, "POST", s.url+"/api/anomalies/"+id+"/ack", adm, `{"verdict":"confirmed"}`); code != 200 {
		t.Fatalf("ack rule alarm %s: %d %s", id, code, raw)
	}

	// Status, then a dry run over history (no database: memory buffers).
	_, _, raw = call(t, "GET", s.url+"/api/jobs", adm, "")
	if !strings.Contains(raw, `"id":"tank-avg"`) || !strings.Contains(raw, `"out":1`) {
		t.Fatalf("job status: %s", raw)
	}
	code, m, raw := call(t, "POST", s.url+"/api/jobs/test?from=-10m", adm, `{"query":"SELECT max(level) INTO [peak] FROM [tank-1] GROUP BY TumblingWindow(second, 10)"}`)
	if code != 200 || m["readings"] != 5.0 || !strings.Contains(raw, `"value":40`) {
		t.Fatalf("dry run: %d %s", code, raw)
	}
	if _, _, raw := call(t, "GET", s.url+"/api/sensors/peak", adm, ""); !strings.Contains(raw, "not found") {
		t.Errorf("a dry run must not write: %s", raw)
	}

	// Quota: 3 jobs.
	put("j3", `{"enabled":false,"query":"SELECT max(x) INTO [m3] FROM [q] GROUP BY TumblingWindow(minute, 1)"}`)
	if code, _ := put("j4", `{"enabled":false,"query":"SELECT max(x) INTO [m4] FROM [q] GROUP BY TumblingWindow(minute, 1)"}`); code != 507 {
		t.Errorf("job quota: %d", code)
	}
	// Persisted across a restart.
	call(t, "PATCH", s.url+"/api/admin/tenants/acme", super, `{"status":"suspended"}`)
	call(t, "PATCH", s.url+"/api/admin/tenants/acme", super, `{"status":"active"}`)
	_, _, raw = call(t, "GET", s.url+"/api/jobs", adm, "")
	if strings.Count(raw, `"query"`) != 3 {
		t.Fatalf("after restart: %s", raw)
	}
	if code, _, _ := call(t, "DELETE", s.url+"/api/jobs/j3", adm, ""); code != 204 {
		t.Errorf("delete: %d", code)
	}
}
