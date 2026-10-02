// Package broker embeds an MQTT broker so devices can publish without any
// external infrastructure. Topics (prefix configurable, default "iot"):
//
//	iot/{sensor}          JSON object of fields, or a bare value stored as "value"
//	iot/{sensor}/{field}  bare value for one field
//
// Messages are still routed to MQTT subscribers as usual, so other services
// (e.g. the ML prediction server) can listen on the same topics.
package broker

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"strings"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/hooks/auth"
	"github.com/mochi-mqtt/server/v2/listeners"
	"github.com/mochi-mqtt/server/v2/packets"

	"github.com/williamtatendajose/prediction/iot-hub/internal/ingest"
	"github.com/williamtatendajose/prediction/iot-hub/internal/store"
)

type Config struct {
	TCPAddr string // e.g. ":1883"; empty disables TCP
	WSAddr  string // e.g. ":1882"; MQTT over WebSocket, empty disables
	Prefix  string // topic prefix, default "iot"
	Token   string // if set, clients must send it as the MQTT password
	Logger  *slog.Logger
}

type Broker struct {
	srv    *mqtt.Server
	prefix string
}

func New(cfg Config, p *ingest.Pipeline) (*Broker, error) {
	if cfg.Prefix == "" {
		cfg.Prefix = "iot"
	}
	caps := mqtt.NewDefaultServerCapabilities()
	caps.MaximumPacketSize = 64 * 1024
	caps.MaximumSessionExpiryInterval = 3600
	caps.MaximumMessageExpiryInterval = 3600
	srv := mqtt.New(&mqtt.Options{
		Capabilities:             caps,
		ClientNetWriteBufferSize: 1024,
		ClientNetReadBufferSize:  1024,
		InlineClient:             true,
		Logger:                   cfg.Logger,
	})

	if cfg.Token == "" {
		if err := srv.AddHook(new(auth.AllowHook), nil); err != nil {
			return nil, err
		}
	} else if err := srv.AddHook(&tokenAuth{token: []byte(cfg.Token)}, nil); err != nil {
		return nil, err
	}
	if err := srv.AddHook(&ingestHook{p: p, prefix: cfg.Prefix + "/", log: srv.Log}, nil); err != nil {
		return nil, err
	}
	if cfg.TCPAddr != "" {
		if err := srv.AddListener(listeners.NewTCP(listeners.Config{ID: "tcp", Address: cfg.TCPAddr})); err != nil {
			return nil, err
		}
	}
	if cfg.WSAddr != "" {
		if err := srv.AddListener(listeners.NewWebsocket(listeners.Config{ID: "ws", Address: cfg.WSAddr})); err != nil {
			return nil, err
		}
	}
	return &Broker{srv: srv, prefix: cfg.Prefix}, nil
}

func (b *Broker) Serve() error { return b.srv.Serve() }
func (b *Broker) Close() error { return b.srv.Close() }

// Republish forwards a reading that arrived over REST onto MQTT so
// subscribers see every reading regardless of transport. Inline publishes are
// skipped by ingestHook, so this cannot loop.
func (b *Broker) Republish(r store.Reading) {
	m := make(map[string]any, len(r.Values)+1)
	for k, v := range r.Values {
		m[k] = v
	}
	m["ts"] = r.TS
	payload, err := json.Marshal(m)
	if err != nil {
		return
	}
	_ = b.srv.Publish(b.prefix+"/"+r.Sensor, payload, false, 0)
}

type ingestHook struct {
	mqtt.HookBase
	p      *ingest.Pipeline
	prefix string
	log    *slog.Logger
}

func (h *ingestHook) ID() string { return "iot-ingest" }

func (h *ingestHook) Provides(b byte) bool { return b == mqtt.OnPublished }

func (h *ingestHook) OnPublished(cl *mqtt.Client, pk packets.Packet) {
	if cl.Net.Inline || !strings.HasPrefix(pk.TopicName, h.prefix) {
		return
	}
	parts := strings.Split(pk.TopicName[len(h.prefix):], "/")
	var sensor, field string
	switch len(parts) {
	case 1:
		sensor = parts[0]
	case 2:
		sensor, field = parts[0], parts[1]
	default:
		return
	}
	if _, err := h.p.Handle(sensor, field, bytes.Clone(pk.Payload)); err != nil {
		h.log.Debug("mqtt ingest rejected", "topic", pk.TopicName, "err", err)
	}
}

type tokenAuth struct {
	mqtt.HookBase
	token []byte
}

func (a *tokenAuth) ID() string { return "token-auth" }

func (a *tokenAuth) Provides(b byte) bool {
	return b == mqtt.OnConnectAuthenticate || b == mqtt.OnACLCheck
}

func (a *tokenAuth) OnConnectAuthenticate(cl *mqtt.Client, pk packets.Packet) bool {
	return subtle.ConstantTimeCompare(pk.Connect.Password, a.token) == 1
}

func (a *tokenAuth) OnACLCheck(cl *mqtt.Client, topic string, write bool) bool { return true }
