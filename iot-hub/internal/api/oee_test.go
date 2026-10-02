package api_test

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestCalculatedFieldsAndOEE(t *testing.T) {
	e := full(t, false)
	put := func(id, body string) {
		req, _ := http.NewRequest("PUT", e.url+"/api/sensors/"+id, strings.NewReader(body))
		res, err := http.DefaultClient.Do(req)
		if err != nil || res.StatusCode != 200 {
			t.Fatalf("put %s: %v %v", id, err, res.Status)
		}
	}
	// Bad definitions are refused with a reason.
	for _, bad := range []string{
		`{"fields":{"p":{"calc":{"formula":"v*"}}}}`,
		`{"fields":{"p":{"calc":{"formula":"p+1"}}}}`,
		`{"fields":{},"oee":{"running":"run","total":"n","idealCycleSec":0}}`,
	} {
		req, _ := http.NewRequest("PUT", e.url+"/api/sensors/bad", strings.NewReader(bad))
		if res, _ := http.DefaultClient.Do(req); res.StatusCode != 400 {
			t.Errorf("%s: want 400, got %d", bad, res.StatusCode)
		}
	}

	// Energy meter: power from V x I, energy integrated.
	put("meter", `{"fields":{"power_kw":{"unit":"kW","calc":{"formula":"voltage * current / 1000"}},"energy_kwh":{"unit":"kWh","calc":{"integrate":"power_kw"}}}}`)
	start := time.Now().Add(-3 * time.Hour).Truncate(time.Hour)
	for i := 0; i <= 60; i++ { // 1 kW for 1 h, a device also tries to write energy_kwh
		post(t, e.url+"/api/sensors/meter/data", fmt.Sprintf(`{"voltage":230,"current":%.6f,"energy_kwh":999,"ts":%d}`, 1000/230.0, start.Add(time.Duration(i)*time.Minute).UnixMilli()))
	}
	var m struct{ Last map[string]any }
	getJSON(t, e.url+"/api/sensors/meter", &m)
	if p, en := m.Last["power_kw"].(float64), m.Last["energy_kwh"].(float64); math.Abs(p-1) > 1e-6 || math.Abs(en-1) > 1e-6 { // current sent rounded to 6 decimals
		t.Fatalf("power %v kW, energy %v kWh (want 1, 1)", p, en)
	}

	// Machine: 2 h window, stopped 0:30-1:00 (A = 0.75), 81 parts at an
	// ideal 60 s (P = 81/90 = 0.9), 9 rejects (Q = 72/81). OEE = 0.6.
	put("press", `{"fields":{"run":{},"count":{},"rejects":{}},"oee":{"running":"run","total":"count","reject":"rejects","idealCycleSec":60}}`)
	count, rej, made := 0, 0, 0
	// Counters need a baseline before the window: a counter's first-ever
	// value may hold years of history, so it is never counted as production.
	post(t, e.url+"/api/sensors/press/data", fmt.Sprintf(`{"run":false,"count":0,"rejects":0,"ts":%d}`, start.Add(-time.Minute).UnixMilli()))
	for i := 0; i < 120; i++ {
		ts := start.Add(time.Duration(i) * time.Minute)
		running := i < 30 || i >= 60
		if running && made < 81 && i%10 != 9 { // skip a few minutes: 81 parts in 90 running minutes
			count++
			made++
			if made%9 == 0 {
				rej++
			}
		}
		post(t, e.url+"/api/sensors/press/data", fmt.Sprintf(`{"run":%v,"count":%d,"rejects":%d,"ts":%d}`, running, count, rej, ts.UnixMilli()))
	}
	var o struct {
		Result struct {
			OEE, Availability, Performance, Quality *float64
			Total, Reject                           float64
			Warnings                                []string
		}
		Buckets []struct{ OEE *float64 }
	}
	url := fmt.Sprintf("%s/api/sensors/press/oee?from=%d&to=%d&bucket=1h", e.url, start.UnixMilli(), start.Add(2*time.Hour).UnixMilli())
	if code := getJSON(t, url, &o); code != 200 {
		t.Fatalf("oee %d", code)
	}
	r := o.Result
	if made != 81 || r.Total != 81 || r.Reject != 9 || math.Abs(*r.Availability-0.75) > 1e-9 || math.Abs(*r.Performance-0.9) > 1e-9 ||
		math.Abs(*r.OEE-0.6) > 1e-9 || len(r.Warnings) != 0 {
		t.Fatalf("oee %+v A %v P %v Q %v OEE %v made %d", r, *r.Availability, *r.Performance, *r.Quality, *r.OEE, made)
	}
	if len(o.Buckets) != 2 || *o.Buckets[0].OEE >= *o.Buckets[1].OEE {
		t.Fatalf("hourly buckets: the first hour had the stop, so lower OEE: %+v", o.Buckets)
	}
	// The shift report over the same window agrees with the OEE endpoint
	// and picks up the energy meter.
	rurl := fmt.Sprintf("%s/api/reports?from=%d&to=%d", e.url, start.UnixMilli(), start.Add(2*time.Hour).UnixMilli())
	var rep struct {
		Machines []struct {
			Sensor string
			Result struct{ OEE *float64 }
		}
		Energy []struct {
			Sensor, Field string
			Consumed      float64
		}
		Period string
	}
	if code := getJSON(t, rurl, &rep); code != 200 || len(rep.Machines) != 1 || math.Abs(*rep.Machines[0].Result.OEE-0.6) > 1e-9 ||
		len(rep.Energy) != 1 || rep.Energy[0].Field != "energy_kwh" || math.Abs(rep.Energy[0].Consumed-1) > 1e-6 || rep.Period == "" {
		t.Fatalf("report %d %+v", code, rep)
	}
	for format, want := range map[string]string{"html": "<td>press</td>", "text": "• press: OEE 60.0% (typical) — A 75.0%, P 90.0%, Q 88.9%; 81 parts, 9 rejected; ran 1 h 30 min of 2 h 00 min"} {
		res, err := http.Get(rurl + "&format=" + format)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if !strings.Contains(string(b), want) || !strings.HasPrefix(res.Header.Get("Content-Type"), map[string]string{"html": "text/html", "text": "text/plain"}[format]) {
			t.Errorf("%s report missing %q (%s):\n%s", format, want, res.Header.Get("Content-Type"), b)
		}
	}
	res, _ := http.Get(rurl + "&format=json")
	if b, _ := io.ReadAll(res.Body); !strings.Contains(string(b), `"longest":[]`) {
		t.Errorf("empty lists must be [] not null: %s", b)
	}
	if res, _ := http.Get(e.url + "/api/reports?shift=yesterday"); res.StatusCode != 400 {
		t.Errorf("bad shift: %d", res.StatusCode)
	}
	if res, _ := http.Post(e.url+"/api/reports/send", "", nil); res.StatusCode != 409 {
		t.Errorf("send without targets: %d", res.StatusCode)
	}

	var all []struct{ Sensor string }
	getJSON(t, fmt.Sprintf("%s/api/oee?from=%d&to=%d", e.url, start.UnixMilli(), start.Add(2*time.Hour).UnixMilli()), &all)
	if len(all) != 1 || all[0].Sensor != "press" {
		t.Fatalf("overview %+v", all)
	}
}
