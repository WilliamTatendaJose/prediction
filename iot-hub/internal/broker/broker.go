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
	// Tenants switches to multi-tenant topics: {tenant}/{prefix}/{sensor}.
	// Usernames are {tenant}/{id}; Auth must then be the platform view.
	Tenants Resolver
}

// Resolver finds an active tenant's pipeline.
type Resolver interface {
	Pipeline(tenant string) (*ingest.Pipeline, error)
}

type Broker struct {
	srv    *mqtt.Server
	prefix string
	acl    *aclHook
	multi  bool
}

// root is a tenant's topic namespace.
func (b *Broker) root(tenant string) string {
	if b.multi {
		return tenant + "/"
	}
	return ""
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
	acl := &aclHook{store: cfg.Auth, prefix: cfg.Prefix, ids: map[*mqtt.Client]*auth.Identity{}, tenants: cfg.Tenants}
	if err := srv.AddHook(acl, nil); err != nil {
		return nil, err
	}
	if err := srv.AddHook(&ingestHook{p: p, prefix: cfg.Prefix + "/", log: srv.Log, tenants: cfg.Tenants}, nil); err != nil {
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
	return &Broker{srv: srv, prefix: cfg.Prefix, acl: acl, multi: cfg.Tenants != nil}, nil
}

func (b *Broker) Serve() error { return b.srv.Serve() }
func (b *Broker) Close() error { return b.srv.Close() }

// Republish forwards a reading that arrived over REST onto MQTT so
// subscribers see every reading regardless of transport. Inline publishes are
// skipped by ingestHook, so this cannot loop.
func (b *Broker) Republish(r store.Reading) { b.RepublishTo(auth.DefaultTenant, r) }

// RepublishTo publishes a reading in a tenant's namespace.
func (b *Broker) RepublishTo(tenant string, r store.Reading) {
	m := make(map[string]any, len(r.Values)+1)
	for k, v := range r.Values {
		m[k] = v
	}
	m["ts"] = r.TS
	payload, err := json.Marshal(m)
	if err != nil {
		return
	}
	_ = b.srv.Publish(b.root(tenant)+b.prefix+"/"+r.Sensor, payload, false, 0)
}

type ingestHook struct {
	mqtt.HookBase
	p       *ingest.Pipeline
	prefix  string
	log     *slog.Logger
	tenants Resolver
}

func (h *ingestHook) ID() string { return "iot-ingest" }

func (h *ingestHook) Provides(b byte) bool { return b == mqtt.OnPublished }

func (h *ingestHook) OnPublished(cl *mqtt.Client, pk packets.Packet) {
	if cl.Net.Inline {
		return
	}
	topic, p := pk.TopicName, h.p
	if h.tenants != nil { // {tenant}/{prefix}/...; the ACL already checked the tenant
		tenant, rest, ok := strings.Cut(topic, "/")
		if !ok {
			return
		}
		pp, err := h.tenants.Pipeline(tenant)
		if err != nil {
			return
		}
		topic, p = rest, pp
	}
	if !strings.HasPrefix(topic, h.prefix) {
		return
	}
	parts := strings.Split(topic[len(h.prefix):], "/")
	var sensor, field string
	switch len(parts) {
	case 1:
		sensor = parts[0]
	case 2:
		sensor, field = parts[0], parts[1]
	default:
		return
	}
	if _, err := p.Handle(sensor, field, bytes.Clone(pk.Payload)); err != nil {
		h.log.Debug("mqtt ingest rejected", "topic", pk.TopicName, "err", err)
	}
}

// Kick disconnects every session authenticated as id (after revocation).
func (b *Broker) Kick(id string) int { return b.acl.kick(auth.DefaultTenant, id) }

// KickTenant disconnects a tenant's sessions of id ("" = all of them, when
// the tenant is suspended or deleted).
func (b *Broker) KickTenant(tenant, id string) int { return b.acl.kick(tenant, id) }

var errRevoked = errors.New("credentials revoked")

// aclHook authenticates CONNECT and authorises every publish/subscribe.
type aclHook struct {
	mqtt.HookBase
	store   *auth.Store
	prefix  string
	tenants Resolver
	mu      sync.Mutex
	ids     map[*mqtt.Client]*auth.Identity
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
	user := string(pk.Connect.Username)
	if h.tenants != nil {
		// Multi-tenant: the username is {tenant}/{id}, as in Azure IoT Hub,
		// and the tenant must be active.
		if id.Role != auth.Superadmin {
			if user != id.Tenant+"/"+id.ID {
				return false
			}
			if _, err := h.tenants.Pipeline(id.Tenant); err != nil {
				return false
			}
		}
	} else if id.ID != "admin" && id != auth.Anonymous && user != id.ID {
		// Bind the session to the identity: a device's username must be its
		// id, so logs and client lists show who is connected.
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
	if h.tenants != nil {
		// Every topic lives under a literal tenant segment. A subscription
		// filter must start with the caller's own tenant, so no wildcard
		// can reach another tenant's topics.
		if id.Role != auth.Superadmin {
			rest, ok := strings.CutPrefix(topic, id.Tenant+"/")
			if !ok {
				return false
			}
			topic = rest
		} else if _, rest, ok := strings.Cut(topic, "/"); ok && write {
			topic = rest
		}
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

func (h *aclHook) kick(tenant, id string) int {
	h.mu.Lock()
	var victims []*mqtt.Client
	for cl, ident := range h.ids {
		if ident.Tenant == tenant && (id == "" || ident.ID == id) {
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
func (b *Broker) PublishEvent(e anomaly.Event) { b.PublishEventTo(auth.DefaultTenant, e) }

func (b *Broker) PublishEventTo(tenant string, e anomaly.Event) {
	payload, err := json.Marshal(e)
	if err != nil {
		return
	}
	_ = b.srv.Publish(b.root(tenant)+b.prefix+"-events/anomaly/"+e.Sensor, payload, false, 0)
}
