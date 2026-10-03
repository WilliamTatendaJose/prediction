// Command devicesim is a sample device for twins, direct methods and
// cloud-to-device messages, and a reference for firmware authors:
//
//	go run ./cmd/devicesim -cs "HostName=hub;TenantId=acme;DeviceId=pump-1;SharedAccessKey=…" [-mqtt-port 1883]
//	go run ./cmd/devicesim -broker tcp://localhost:1883 -id pump-1 -token iot_…   (single-tenant)
//
// It applies desired properties and reports them back (the usual twin
// pattern), answers the methods "ping" and "reboot", and completes every
// message it receives.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/williamtatendajose/prediction/iot-hub/internal/auth"
)

func main() {
	cs := flag.String("cs", "", "connection string (HostName may include :port; MQTT over TCP)")
	brokerURL := flag.String("broker", "tcp://localhost:1883", "MQTT broker (with -token)")
	id := flag.String("id", "", "device id (with -token)")
	tenant := flag.String("tenant", "", "tenant (multi-tenant hubs, with -token)")
	token := flag.String("token", "", "device token")
	mqttPort := flag.String("mqtt-port", "1883", "MQTT port on the connection string's host")
	flag.Parse()

	user, pass, root := *id, *token, ""
	url := *brokerURL
	if *cs != "" {
		f := map[string]string{}
		for _, p := range strings.Split(*cs, ";") {
			if k, v, ok := strings.Cut(p, "="); ok {
				f[k] = v
			}
		}
		*id, *tenant = f["DeviceId"], f["TenantId"]
		host := strings.TrimPrefix(strings.TrimPrefix(f["HostName"], "https://"), "http://")
		if h, _, ok := strings.Cut(host, ":"); ok { // HostName's port is the HTTP one
			host = h
		}
		url = "tcp://" + host + ":" + *mqttPort
		sas, err := auth.SignSAS(auth.ResourceURI(*tenant, *id), f["SharedAccessKey"], time.Now().Add(24*time.Hour))
		if err != nil {
			log.Fatal(err)
		}
		user, pass = *id, sas
	}
	if *tenant != "" {
		user, root = *tenant+"/"+*id, *tenant+"/"
	}
	base := root + "devices/" + *id + "/"
	opts := paho.NewClientOptions().AddBroker(url).SetClientID(*id).SetUsername(user).SetPassword(pass).SetAutoReconnect(true)
	var c paho.Client
	reported := map[string]any{"firmware": "1.0.0", "started": time.Now().UTC().Format(time.RFC3339)}
	report := func(patch map[string]any) {
		b, _ := json.Marshal(patch)
		c.Publish(base+"twin/reported/r"+fmt.Sprint(time.Now().UnixNano()), 0, false, b)
	}
	opts.SetOnConnectHandler(func(cl paho.Client) {
		cl.Subscribe(base+"#", 0, func(_ paho.Client, m paho.Message) {
			sub := strings.TrimPrefix(m.Topic(), base)
			parts := strings.Split(sub, "/")
			switch {
			case sub == "twin/desired":
				// Apply, then report what is now in effect.
				var d map[string]any
				json.Unmarshal(m.Payload(), &d)
				log.Printf("desired v%v: %s", d["$version"], m.Payload())
				delete(d, "$version")
				report(d)
			case parts[0] == "twin" && parts[1] == "res":
				log.Printf("twin %s: %s", strings.Join(parts[2:], " "), m.Payload())
			case parts[0] == "methods" && len(parts) == 3:
				name, rid := parts[1], parts[2]
				status, out := 200, any(map[string]any{"pong": time.Now().UTC().Format(time.RFC3339Nano)})
				switch name {
				case "ping":
				case "reboot":
					out = map[string]any{"rebooting": true, "request": json.RawMessage(m.Payload())}
				default:
					status, out = 404, map[string]string{"error": "unknown method " + name}
				}
				b, _ := json.Marshal(out)
				log.Printf("method %s(%s) → %d", name, m.Payload(), status)
				cl.Publish(fmt.Sprintf("%smethods/res/%d/%s", base, status, rid), 0, false, b)
			case parts[0] == "messages" && len(parts) == 2:
				log.Printf("message %s: %s", parts[1], m.Payload())
				cl.Publish(base+"messages/complete/"+parts[1], 0, false, "")
			}
		})
		cl.Publish(base+"twin/get/start", 0, false, "")
		report(reported)
	})
	c = paho.NewClient(opts)
	if t := c.Connect(); t.Wait() && t.Error() != nil {
		log.Fatal(t.Error())
	}
	log.Printf("connected as %s to %s; listening on %s#", user, url, base)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	<-sig
	c.Disconnect(100)
}
