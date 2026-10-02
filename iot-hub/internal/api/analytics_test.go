package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/williamtatendajose/prediction/iot-hub/internal/analytics"
	"github.com/williamtatendajose/prediction/iot-hub/internal/anomaly"
	"github.com/williamtatendajose/prediction/iot-hub/internal/api"
	"github.com/williamtatendajose/prediction/iot-hub/internal/broker"
	"github.com/williamtatendajose/prediction/iot-hub/internal/ingest"
	"github.com/williamtatendajose/prediction/iot-hub/internal/store"
	"github.com/williamtatendajose/prediction/iot-hub/internal/stream"
	"github.com/williamtatendajose/prediction/iot-hub/internal/tsdb"
)

type fullEnv struct {
	url    string
	mqtt   string
	writer *tsdb.Writer
	stop   func()
}

// full wires everything main does: optional SQLite, detector, broker.
func full(t *testing.T, withDB bool) fullEnv {
	ctx, cancel := context.WithCancel(context.Background())
	st := store.New(store.Options{AutoRegister: true, Capacity: 4096})
	hub := stream.NewHub(256)
	det := anomaly.New(anomaly.Config{Warmup: 20, Persist: 1})
	pipe := &ingest.Pipeline{Store: st, Hub: hub, Detector: det}
	an := &analytics.Service{Store: st, Detector: det}
	srv := &api.Server{Store: st, Hub: hub, Pipeline: pipe, Analytics: an, Detector: det}
	var w *tsdb.Writer
	var db tsdb.DB
	if withDB {
		var err error
		db, err = tsdb.Open(ctx, filepath.Join(t.TempDir(), "r.db"))
		if err != nil {
			t.Fatal(err)
		}
		w = tsdb.NewWriter(db, 1024)
		w.Interval = 50 * time.Millisecond
		go w.Run(ctx)
		pipe.Writer, an.DB, srv.Writer = w, db, w
	}
	addr := freeAddr(t)
	b, err := broker.New(broker.Config{TCPAddr: addr}, pipe)
	if err != nil {
		t.Fatal(err)
	}
	pipe.OnEvent = b.PublishEvent
	srv.OnIngest = b.Republish
	if err := b.Serve(); err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(srv.Handler())
	stop := func() {
		h.Close()
		b.Close()
		cancel()
		if w != nil {
			<-w.Done()
			db.Close()
		}
	}
	t.Cleanup(stop)
	return fullEnv{url: h.URL, mqtt: "tcp://" + addr, writer: w}
}

func getJSON(t *testing.T, url string, v any) int {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if v != nil {
		json.NewDecoder(res.Body).Decode(v)
	}
	return res.StatusCode
}

func post(t *testing.T, url, body string) {
	t.Helper()
	res, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil || res.StatusCode != http.StatusAccepted {
		t.Fatalf("post %s: %v %v", body, err, res.Status)
	}
	res.Body.Close()
}

func TestAnalyticsMemoryAndDB(t *testing.T) {
	for _, withDB := range []bool{false, true} {
		t.Run(fmt.Sprintf("db=%v", withDB), func(t *testing.T) {
			e := full(t, withDB)
			base := time.Now().Add(-10 * time.Minute).UnixMilli()
			base -= base % 60000
			for i := 0; i < 300; i++ { // 5 minutes at 1 Hz, values 0..299
				post(t, e.url+"/api/sensors/p1/data", fmt.Sprintf(`{"kw":%d,"ts":%d}`, i, base+int64(i)*1000))
			}
			if withDB { // wait for the writer to flush
				deadline := time.Now().Add(5 * time.Second)
				for e.writer.Written.Load() < 300 && time.Now().Before(deadline) {
					time.Sleep(20 * time.Millisecond)
				}
			}
			var st struct {
				N         int64
				Mean, Max float64
				Source    string
			}
			code := getJSON(t, fmt.Sprintf("%s/api/sensors/p1/stats?field=kw&from=%d&to=%d", e.url, base, base+300_000), &st)
			wantSrc := map[bool]string{false: "memory", true: "db"}[withDB]
			if code != 200 || st.N != 300 || st.Mean != 149.5 || st.Max != 299 || st.Source != wantSrc {
				t.Fatalf("stats %d %+v", code, st)
			}
			var se struct {
				Bucket int64
				T      []int64
				Avg    []float64
				N      []int64
			}
			getJSON(t, fmt.Sprintf("%s/api/sensors/p1/series?field=kw&from=%d&to=%d&bucket=1m", e.url, base, base+300_000), &se)
			if se.Bucket != 60000 || len(se.T) != 5 || se.N[0] != 60 || se.Avg[0] != 29.5 {
				t.Fatalf("series %+v", se)
			}
			// relative time + auto bucket
			if code := getJSON(t, e.url+"/api/sensors/p1/series?field=kw&from=-1h", &se); code != 200 || se.Bucket != 10000 {
				t.Fatalf("auto bucket %d %+v", code, se.Bucket)
			}
			for _, bad := range []string{"from=-1x", "bucket=10ms", "from=-1h&to=-2h", "from=-30d&bucket=1s"} {
				if code := getJSON(t, e.url+"/api/sensors/p1/series?field=kw&"+bad, nil); code != 400 {
					t.Errorf("%s: want 400, got %d", bad, code)
				}
			}
		})
	}
}

func TestAnomalyFlow(t *testing.T) {
	for _, withDB := range []bool{false, true} {
		t.Run(fmt.Sprintf("db=%v", withDB), func(t *testing.T) {
			e := full(t, withDB)
			// SSE subscriber
			res, err := http.Get(e.url + "/api/stream?sensors=m1")
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			anomalies := make(chan string, 4)
			go func() {
				sc := bufio.NewScanner(res.Body)
				isAnomaly := false
				for sc.Scan() {
					l := sc.Text()
					if l == "event: anomaly" {
						isAnomaly = true
					} else if strings.HasPrefix(l, "data: ") && isAnomaly {
						anomalies <- strings.TrimPrefix(l, "data: ")
						isAnomaly = false
					}
				}
			}()
			// MQTT subscriber for anomaly events
			c := paho.NewClient(paho.NewClientOptions().AddBroker(e.mqtt).SetClientID("sub" + t.Name()))
			if tk := c.Connect(); tk.Wait() && tk.Error() != nil {
				t.Fatal(tk.Error())
			}
			defer c.Disconnect(0)
			mq := make(chan string, 4)
			c.Subscribe("iot-events/anomaly/#", 0, func(_ paho.Client, m paho.Message) { mq <- m.Topic() }).Wait()

			for i := 0; i < 60; i++ {
				post(t, e.url+"/api/sensors/m1/data", fmt.Sprintf(`{"vib":%g}`, 1+0.01*float64(i%7)))
			}
			post(t, e.url+"/api/sensors/m1/data", `{"vib":50}`)

			var ev anomaly.Event
			select {
			case s := <-anomalies:
				json.Unmarshal([]byte(s), &ev)
			case <-time.After(3 * time.Second):
				t.Fatal("no anomaly on SSE")
			}
			if ev.Kind != "spike" || ev.Sensor != "m1" || ev.Field != "vib" || ev.Value != 50 {
				t.Fatalf("event %+v", ev)
			}
			select {
			case topic := <-mq:
				if topic != "iot-events/anomaly/m1" {
					t.Fatalf("topic %s", topic)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("no anomaly on MQTT")
			}
			var active []anomaly.Event
			getJSON(t, e.url+"/api/anomalies?active=1", &active)
			if len(active) != 1 || active[0].ID != ev.ID {
				t.Fatalf("active %+v", active)
			}
			post(t, e.url+"/api/sensors/m1/data", `{"vib":1.02}`) // back to normal closes it
			time.Sleep(200 * time.Millisecond)                    // DB writer interval is 50ms
			var hist []anomaly.Event
			getJSON(t, e.url+"/api/anomalies?sensor=m1", &hist)
			if len(hist) != 1 || hist[0].End == 0 {
				t.Fatalf("history %+v", hist)
			}
			getJSON(t, e.url+"/api/anomalies?active=1", &active)
			if len(active) != 0 {
				t.Fatalf("still active %+v", active)
			}
		})
	}
}

func TestRangeRuleFromSensorDefinition(t *testing.T) {
	e := full(t, false)
	req, _ := http.NewRequest("PUT", e.url+"/api/sensors/tank", strings.NewReader(
		`{"fields":{"level":{"unit":"%","detect":{"low":10,"high":95}}}}`))
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != 200 {
		t.Fatalf("put: %v", err)
	}
	post(t, e.url+"/api/sensors/tank/data", `{"level":5}`)
	var active []anomaly.Event
	getJSON(t, e.url+"/api/anomalies?active=1&sensor=tank", &active)
	if len(active) != 1 || active[0].Kind != "range" {
		t.Fatalf("active %+v", active)
	}
}

func TestForecastETA(t *testing.T) {
	e := full(t, true)
	putReq, _ := http.NewRequest("PUT", e.url+"/api/sensors/tank", strings.NewReader(`{"fields":{"level":{"unit":"%","detect":{"low":15,"z":-1}}}}`))
	if res, err := http.DefaultClient.Do(putReq); err != nil || res.StatusCode != 200 {
		t.Fatalf("put: %v", err)
	}
	// 12 h of history every minute: 95 % falling 4 %/h.
	now := time.Now()
	start := now.Add(-12 * time.Hour)
	var lines []string
	for i := 0; i <= 720; i++ {
		ts := start.Add(time.Duration(i) * time.Minute)
		lines = append(lines, fmt.Sprintf(`{"level":%.3f,"ts":%d}`, 95-4*float64(i)/60, ts.UnixMilli()))
	}
	for _, l := range lines {
		post(t, e.url+"/api/sensors/tank/data", l)
	}
	deadline := time.Now().Add(5 * time.Second)
	for e.writer.Written.Load() < 721 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	var f struct {
		Method    string
		Skill     float64
		Crossings []struct {
			Side          string
			ETA, ETAEarly int64
			ETALate       int64
		}
		Yhat []float64
	}
	code := getJSON(t, e.url+"/api/sensors/tank/forecast?field=level&horizon=6h&history=12h", &f)
	if code != 200 || len(f.Crossings) != 1 || f.Crossings[0].Side != "below" {
		t.Fatalf("forecast %d %+v", code, f)
	}
	// Now at 47 %; 15 % is (47-15)/4 = 8 h away: beyond a 6 h horizon.
	if f.Crossings[0].ETA != 0 {
		t.Fatalf("crossing should be beyond 6 h, got eta in %v", time.Until(time.UnixMilli(f.Crossings[0].ETA)))
	}
	getJSON(t, e.url+"/api/sensors/tank/forecast?field=level&horizon=12h&history=12h", &f)
	eta := time.Until(time.UnixMilli(f.Crossings[0].ETA))
	if eta < 7*time.Hour+30*time.Minute || eta > 8*time.Hour+30*time.Minute {
		t.Fatalf("ETA %v, want ~8h (%s skill %.2f)", eta, f.Method, f.Skill)
	}
	if code := getJSON(t, e.url+"/api/sensors/nope/forecast?field=x", nil); code != 400 && code != 404 {
		t.Fatalf("unknown sensor: %d", code)
	}
}

func TestForecastClampedToFieldRange(t *testing.T) {
	e := full(t, false)
	putReq, _ := http.NewRequest("PUT", e.url+"/api/sensors/tank", strings.NewReader(`{"fields":{"level":{"min":0,"max":100}}}`))
	if res, err := http.DefaultClient.Do(putReq); err != nil || res.StatusCode != 200 {
		t.Fatal(err)
	}
	start := time.Now().Add(-2 * time.Hour)
	for i := 0; i <= 120; i++ { // 20 % falling 10 %/h: hits 0 in 2 h
		post(t, e.url+"/api/sensors/tank/data", fmt.Sprintf(`{"level":%.2f,"ts":%d}`, 40-10*float64(i)/60, start.Add(time.Duration(i)*time.Minute).UnixMilli()))
	}
	var f struct{ Yhat, Lo []float64 }
	if code := getJSON(t, e.url+"/api/sensors/tank/forecast?field=level&horizon=6h&history=2h", &f); code != 200 {
		t.Fatalf("code %d", code)
	}
	for i := range f.Yhat {
		if f.Yhat[i] < 0 || f.Lo[i] < 0 {
			t.Fatalf("forecast below the field minimum at %d: %v / %v", i, f.Yhat[i], f.Lo[i])
		}
	}
}
