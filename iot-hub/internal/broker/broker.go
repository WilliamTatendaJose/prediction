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
	"crypto/tls"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/listeners"
	"github.com/mochi-mqtt/server/v2/packets"

	"github.com/williamtatendajose/prediction/iot-hub/internal/anomaly"
	"github.com/williamtatendajose/prediction/iot-hub/internal/auth"
	"github.com/williamtatendajose/prediction/iot-hub/internal/ingest"
	"github.com/williamtatendajose/prediction/iot-hub/internal/store"
)

type Config struct {
	TCPAddr string // e.g. ":1883"; empty disables TCP
	WSAddr  string // e.g. ":1882"; MQTT over WebSocket, empty disables
	Prefix  string // topic prefix, default "iot"
	// TLSAddr serves MQTT over TLS (e.g. ":8883") using TLS; empty disables.
	TLSAddr string
	TLS     *tls.Config
	// Auth checks the MQTT password as a token and enforces topic ACLs.
	// The username must equal the identity id (any username for the admin
	// token). Nil or no credentials configured = open broker.
	Auth   *auth.Store
	Logger *slog.Logger
}

type Broker struct {
	srv    *mqtt.Server
	prefix string
	acl    *aclHook
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

	if cfg.Auth == nil {
		cfg.Auth = auth.New("", "")
	}
	acl := &aclHook{store: cfg.Auth, prefix: cfg.Prefix, ids: map[*mqtt.Client]*auth.Identity{}}
	if err := srv.AddHook(acl, nil); err != nil {
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
	if cfg.TLSAddr != "" {
		if cfg.TLS == nil {
			return nil, errors.New("mqtt TLS listener needs a certificate")
		}
		if err := srv.AddListener(listeners.NewTCP(listeners.Config{ID: "tls", Address: cfg.TLSAddr, TLSConfig: cfg.TLS})); err != nil {
			return nil, err
		}
	}
	if cfg.WSAddr != "" {
		if err := srv.AddListener(listeners.NewWebsocket(listeners.Config{ID: "ws", Address: cfg.WSAddr})); err != nil {
			return nil, err
		}
	}
	return &Broker{srv: srv, prefix: cfg.Prefix, acl: acl}, nil
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

// Kick disconnects every session authenticated as id (after revocation).
func (b *Broker) Kick(id string) int {
	return b.acl.kick(id)
}

var errRevoked = errors.New("credentials revoked")

// aclHook authenticates CONNECT and authorises every publish/subscribe.
type aclHook struct {
	mqtt.HookBase
	store  *auth.Store
	prefix string
	mu     sync.Mutex
	ids    map[*mqtt.Client]*auth.Identity
}

func (h *aclHook) ID() string { return "iot-acl" }

func (h *aclHook) Provides(b byte) bool {
	return b == mqtt.OnConnectAuthenticate || b == mqtt.OnACLCheck || b == mqtt.OnDisconnect
}

func (h *aclHook) OnConnectAuthenticate(cl *mqtt.Client, pk packets.Packet) bool {
	id, ok := h.store.Authenticate(string(pk.Connect.Password))
	if !ok {
		return false
	}
	// Bind the session to the identity: a device's username must be its id,
	// so logs and client lists show who is connected.
	if id.ID != "admin" && id != auth.Anonymous && string(pk.Connect.Username) != id.ID {
		return false
	}
	h.mu.Lock()
	h.ids[cl] = id
	h.mu.Unlock()
	return true
}

func (h *aclHook) OnDisconnect(cl *mqtt.Client, err error, expire bool) {
	h.mu.Lock()
	delete(h.ids, cl)
	h.mu.Unlock()
}

func (h *aclHook) OnACLCheck(cl *mqtt.Client, topic string, write bool) bool {
	h.mu.Lock()
	id := h.ids[cl]
	h.mu.Unlock()
	if id == nil {
		return false
	}
	if !write {
		return id.Can(auth.Subscribe, "")
	}
	if id.Role == auth.Admin || id.Role == auth.Superadmin {
		return true
	}
	// Others may publish only sensor data, and only for their sensors:
	// {prefix}/{sensor} or {prefix}/{sensor}/{field}.
	rest, ok := strings.CutPrefix(topic, h.prefix+"/")
	if !ok {
		return false
	}
	parts := strings.Split(rest, "/")
	if len(parts) > 2 || parts[0] == "" {
		return false
	}
	return id.Can(auth.Ingest, parts[0])
}

func (h *aclHook) kick(id string) int {
	h.mu.Lock()
	var victims []*mqtt.Client
	for cl, ident := range h.ids {
		if ident.ID == id {
			victims = append(victims, cl)
		}
	}
	h.mu.Unlock()
	for _, cl := range victims {
		cl.Stop(errRevoked)
	}
	return len(victims)
}

// PublishEvent sends an anomaly episode to {prefix}-events/anomaly/{sensor}.
// It sits outside the ingest prefix so subscribers to iot/# see only data.
func (b *Broker) PublishEvent(e anomaly.Event) {
	payload, err := json.Marshal(e)
	if err != nil {
		return
	}
	_ = b.srv.Publish(b.prefix+"-events/anomaly/"+e.Sensor, payload, false, 0)
}
