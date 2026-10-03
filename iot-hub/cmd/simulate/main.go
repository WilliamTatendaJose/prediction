// Command simulate publishes fake sensor data to an iothub over MQTT and/or
// REST, for demos and load testing.
//
//	go run ./cmd/simulate -mqtt tcp://localhost:1883 -http http://localhost:8080 -every 1s
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"math/rand/v2"
	"net/http"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
)

func main() {
	mqttURL := flag.String("mqtt", "tcp://localhost:1883", "MQTT broker (empty to skip)")
	httpURL := flag.String("http", "http://localhost:8080", "hub REST base URL (empty to skip)")
	token := flag.String("token", "", "hub token (MQTT password and HTTP bearer)")
	user := flag.String("user", "sim", "MQTT username: must equal the device id when the hub uses per-device tokens")
	every := flag.Duration("every", time.Second, "publish interval")
	extra := flag.Int("extra", 0, "additional generic sensors (load test)")
	faults := flag.Bool("faults", true, "inject occasional spikes and a sensor dropout to exercise anomaly detection")
	flag.Parse()

	var mc paho.Client
	if *mqttURL != "" {
		o := paho.NewClientOptions().AddBroker(*mqttURL).SetClientID(fmt.Sprintf("sim-%d", rand.IntN(1e6))).
			SetPassword(*token).SetUsername(*user).SetAutoReconnect(true)
		mc = paho.NewClient(o)
		if t := mc.Connect(); t.Wait() && t.Error() != nil {
			log.Fatalf("mqtt: %v", t.Error())
		}
	}
	post := func(sensor string, v map[string]any) {
		b, _ := json.Marshal(v)
		req, _ := http.NewRequest("POST", *httpURL+"/api/sensors/"+sensor+"/data", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		if *token != "" {
			req.Header.Set("Authorization", "Bearer "+*token)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			log.Printf("http %s: %v", sensor, err)
			return
		}
		res.Body.Close()
	}
	pub := func(topic string, v any) {
		b, _ := json.Marshal(v)
		mc.Publish(topic, 0, false, b)
	}

	door, locked, level := "Closed", true, 60.0
	inrush := 0
	start := time.Now()
	for i := 0; ; i++ {
		t := float64(i) * every.Seconds()
		env := map[string]any{
			"temperature": round(22+3*math.Sin(t/60)+rand.NormFloat64()*0.2, 2),
			"humidity":    round(50+8*math.Cos(t/90)+rand.NormFloat64()*0.5, 1),
		}
		voltage := 230 + rand.NormFloat64()*1.5
		current := 4 + 2*math.Sin(t/30) + rand.Float64()*0.3
		if *faults && inrush == 0 && rand.Float64() < 0.003 {
			inrush = 3 // motor start: 3 samples of high current
		}
		if inrush > 0 {
			current *= 4
			inrush--
		}
		power := map[string]any{"voltage": round(voltage, 1), "current": round(current, 2), "power": round(voltage*current, 0)}
		if rand.Float64() < 0.05 {
			if door == "Closed" {
				door, locked = "Open", false
			} else {
				door, locked = "Closed", true
			}
		}
		level -= 0.3 + rand.Float64()*0.2
		if level < 10 {
			level = 95 // refilled
		}

		// Show both transports: env and door via MQTT, power and tank via REST.
		if mc != nil {
			pub("iot/env-1", env)
			pub("iot/door-1", map[string]any{"door": door, "locked": locked})
			pub("iot/tank-1/level", round(level, 1)) // single-field topic, bare value
		}
		if *httpURL != "" {
			post("power-1", power)
			if mc == nil {
				post("env-1", env)
				post("door-1", map[string]any{"door": door, "locked": locked})
				post("tank-1", map[string]any{"level": round(level, 1)})
			}
		}
		// pump-1 goes silent for 90 s out of every 6 minutes (stale detection).
		if !*faults || time.Since(start)%(6*time.Minute) < 4*time.Minute+30*time.Second {
			pump := map[string]any{"vibration": round(1.2+0.1*rand.NormFloat64(), 3)}
			if mc != nil {
				pub("iot/pump-1", pump)
			} else if *httpURL != "" {
				post("pump-1", pump)
			}
		}
		for k := 0; k < *extra; k++ {
			v := map[string]any{"value": round(rand.Float64()*100, 2)}
			if mc != nil {
				pub(fmt.Sprintf("iot/load-%d", k), v)
			} else {
				post(fmt.Sprintf("load-%d", k), v)
			}
		}
		time.Sleep(*every)
	}
}

func round(v float64, d int) float64 {
	p := math.Pow(10, float64(d))
	return math.Round(v*p) / p
}
