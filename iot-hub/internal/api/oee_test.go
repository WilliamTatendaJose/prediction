package api_test

import (
	"fmt"
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
	var all []struct{ Sensor string }
	getJSON(t, fmt.Sprintf("%s/api/oee?from=%d&to=%d", e.url, start.UnixMilli(), start.Add(2*time.Hour).UnixMilli()), &all)
	if len(all) != 1 || all[0].Sensor != "press" {
		t.Fatalf("overview %+v", all)
	}
}
