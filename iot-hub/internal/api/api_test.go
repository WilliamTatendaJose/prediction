package api_test

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/williamtatendajose/prediction/iot-hub/internal/api"
	"github.com/williamtatendajose/prediction/iot-hub/internal/broker"
	"github.com/williamtatendajose/prediction/iot-hub/internal/ingest"
	"github.com/williamtatendajose/prediction/iot-hub/internal/store"
	"github.com/williamtatendajose/prediction/iot-hub/internal/stream"
)

func freeAddr(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

type env struct {
	http   *httptest.Server
	mqtt   string
	broker *broker.Broker
}

func setup(t *testing.T, token string) env {
	st := store.New(store.Options{AutoRegister: true, Capacity: 16})
	hub := stream.NewHub(16)
	pipe := &ingest.Pipeline{Store: st, Hub: hub}
	addr := freeAddr(t)
	b, err := broker.New(broker.Config{TCPAddr: addr, Token: token}, pipe)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Serve(); err != nil {
		t.Fatal(err)
	}
	srv := &api.Server{Store: st, Hub: hub, Pipeline: pipe, Token: token, OnIngest: b.Republish}
	h := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { h.Close(); b.Close() })
	return env{http: h, mqtt: "tcp://" + addr, broker: b}
}

func mqttClient(t *testing.T, url, pass string) (paho.Client, error) {
	c := paho.NewClient(paho.NewClientOptions().AddBroker(url).SetClientID(t.Name() + pass).SetUsername("u").SetPassword(pass))
	tok := c.Connect()
	tok.WaitTimeout(3 * time.Second)
	return c, tok.Error()
}

func TestMQTTToSSEAndHistory(t *testing.T) {
	e := setup(t, "")
	res, err := http.Get(e.http.URL + "/api/stream?sensors=env-1")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	rd := bufio.NewReader(res.Body)
	rd.ReadString('\n') // retry line
	rd.ReadString('\n')

	c, err := mqttClient(t, e.mqtt, "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Disconnect(0)
	c.Publish("iot/other", 0, false, `{"x":1}`).Wait() // filtered out of the stream
	c.Publish("iot/env-1/temperature", 0, false, "21.5").Wait()

	line := make(chan string, 1)
	go func() {
		for {
			l, err := rd.ReadString('\n')
			if err != nil {
				return
			}
			if strings.HasPrefix(l, "data: ") {
				line <- l
				return
			}
		}
	}()
	select {
	case l := <-line:
		var r store.Reading
		if err := json.Unmarshal([]byte(strings.TrimPrefix(l, "data: ")), &r); err != nil {
			t.Fatal(err)
		}
		if r.Sensor != "env-1" || r.Values["temperature"] != 21.5 {
			t.Fatalf("reading %+v", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no SSE message")
	}

	hres, _ := http.Get(e.http.URL + "/api/sensors/env-1/history?field=temperature")
	var h struct {
		T []int64
		V []float32
	}
	json.NewDecoder(hres.Body).Decode(&h)
	hres.Body.Close()
	if len(h.V) != 1 || h.V[0] != 21.5 {
		t.Fatalf("history %+v", h)
	}
}

func TestRESTRepublishesToMQTT(t *testing.T) {
	e := setup(t, "")
	c, err := mqttClient(t, e.mqtt, "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Disconnect(0)
	got := make(chan []byte, 1)
	c.Subscribe("iot/#", 0, func(_ paho.Client, m paho.Message) { got <- m.Payload() }).Wait()

	res, err := http.Post(e.http.URL+"/api/sensors/power-1/data", "application/json", strings.NewReader(`{"voltage":230.1,"state":"ok"}`))
	if err != nil || res.StatusCode != http.StatusAccepted {
		t.Fatalf("post: %v %v", err, res.Status)
	}
	select {
	case p := <-got:
		var m map[string]any
		json.Unmarshal(p, &m)
		if m["voltage"] != 230.1 || m["state"] != "ok" || m["ts"] == nil {
			t.Fatalf("payload %s", p)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no republish")
	}
	// The republish must not be ingested a second time.
	time.Sleep(100 * time.Millisecond)
	hres, _ := http.Get(e.http.URL + "/api/sensors/power-1/history?field=voltage")
	b, _ := io.ReadAll(hres.Body)
	if strings.Count(string(b), "230.1") != 1 {
		t.Fatalf("duplicate ingest: %s", b)
	}
}

func TestTokenAuth(t *testing.T) {
	e := setup(t, "s3cret")
	res, _ := http.Post(e.http.URL+"/api/sensors/a/data", "application/json", strings.NewReader(`1`))
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %s", res.Status)
	}
	req, _ := http.NewRequest("POST", e.http.URL+"/api/sensors/a/data", strings.NewReader(`1`))
	req.Header.Set("Authorization", "Bearer s3cret")
	res, _ = http.DefaultClient.Do(req)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %s", res.Status)
	}
	if res, _ := http.Get(e.http.URL + "/api/sensors"); res.StatusCode != 200 {
		t.Fatal("reads stay open")
	}
	if _, err := mqttClient(t, e.mqtt, "wrong"); err == nil {
		t.Fatal("mqtt should reject wrong password")
	}
	c, err := mqttClient(t, e.mqtt, "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	c.Disconnect(0)
}

func TestValidation(t *testing.T) {
	e := setup(t, "")
	for _, c := range []struct{ path, body string }{
		{"/api/sensors/a/data", `{bad`},
		{"/api/sensors/a/data", `{"x":{"nested":1}}`},
		{"/api/sensors/a%20b/data", `1`},
	} {
		res, _ := http.Post(e.http.URL+c.path, "application/json", strings.NewReader(c.body))
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("%s %s: got %s", c.path, c.body, res.Status)
		}
	}
	big := strings.Repeat("1", 20<<10)
	res, _ := http.Post(e.http.URL+"/api/sensors/a/data", "application/json", strings.NewReader(big))
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("big body: %s", res.Status)
	}
}
