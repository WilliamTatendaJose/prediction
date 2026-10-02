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
	// Twins finds a tenant's device twin service (device topics).
	Twins func(tenant string) DeviceHandler
}

// DeviceHandler processes device-to-cloud twin, method and message topics.
type DeviceHandler interface {
	HandleMQTT(id, sub string, payload []byte)
	Listening(id string)
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
	if err := srv.AddHook(&deviceHook{acl: acl, twins: cfg.Twins, multi: cfg.Tenants != nil}, nil); err != nil {
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
		} else if _, rest, ok := strings.Cut(topic, "/"); ok {
			topic = rest
		}
	}
	// Device topics: a device (or service acting as one) may use only its
	// own, and only these. Nothing on them is ever routed to subscribers:
	// the hub processes device publishes and writes to the device's own
	// sessions directly, so commands and twins can't be overheard.
	if topic == "devices" || strings.HasPrefix(topic, "devices/") {
		return deviceTopicAllowed(id, topic, write)
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

func deviceTopicAllowed(id *auth.Identity, topic string, write bool) bool {
	if id.Role != auth.Device && id.Role != auth.Service {
		return false
	}
	sub, ok := strings.CutPrefix(topic, "devices/"+id.ID+"/")
	if !ok {
		return false
	}
	if !write {
		return true // any filter under its own devices/{id}/
	}
	p := strings.Split(sub, "/")
	switch {
	case len(p) >= 2 && len(p) <= 3 && p[0] == "twin" && (p[1] == "get" || p[1] == "reported"):
		return true
	case len(p) == 4 && p[0] == "methods" && p[1] == "res":
		return true
	case len(p) == 3 && p[0] == "messages" && (p[1] == "complete" || p[1] == "reject" || p[1] == "abandon"):
		return true
	}
	return false
}

// deviceHook hands device publishes to the twin service and keeps them
// out of normal routing; it also tells the service when a device starts
// listening, so queued messages go out.
type deviceHook struct {
	mqtt.HookBase
	acl   *aclHook
	twins func(tenant string) DeviceHandler
	multi bool
}

func (h *deviceHook) ID() string { return "iot-devices" }

func (h *deviceHook) Provides(b byte) bool { return b == mqtt.OnPublish || b == mqtt.OnSubscribed }

// split returns tenant and the topic below it.
func (h *deviceHook) split(cl *mqtt.Client, topic string) (*auth.Identity, string, string, bool) {
	h.acl.mu.Lock()
	id := h.acl.ids[cl]
	h.acl.mu.Unlock()
	if id == nil {
		return nil, "", "", false
	}
	tenant := auth.DefaultTenant
	if h.multi {
		t, rest, ok := strings.Cut(topic, "/")
		if !ok {
			return nil, "", "", false
		}
		tenant, topic = t, rest
	}
	return id, tenant, topic, true
}

func (h *deviceHook) OnPublish(cl *mqtt.Client, pk packets.Packet) (packets.Packet, error) {
	if cl.Net.Inline {
		return pk, nil
	}
	id, tenant, topic, ok := h.split(cl, pk.TopicName)
	if !ok || !strings.HasPrefix(topic, "devices/") {
		return pk, nil
	}
	// The ACL has checked it is this identity's own device topic.
	if sub, ok := strings.CutPrefix(topic, "devices/"+id.ID+"/"); ok && h.twins != nil && tenant == id.Tenant {
		if d := h.twins(tenant); d != nil {
			d.HandleMQTT(id.ID, sub, bytes.Clone(pk.Payload))
		}
	}
	return pk, packets.CodeSuccessIgnore // acknowledged, never routed
}

func (h *deviceHook) OnSubscribed(cl *mqtt.Client, pk packets.Packet, codes []byte) {
	for i, f := range pk.Filters {
		if i < len(codes) && codes[i] >= 0x80 {
			continue
		}
		id, tenant, topic, ok := h.split(cl, f.Filter)
		if ok && strings.HasPrefix(topic, "devices/"+id.ID+"/") && h.twins != nil && tenant == id.Tenant {
			if d := h.twins(tenant); d != nil {
				go d.Listening(id.ID) // deliver queued messages
			}
		}
	}
}

// sessions returns a device's connected clients.
func (b *Broker) sessions(tenant, id string) []*mqtt.Client {
	b.acl.mu.Lock()
	defer b.acl.mu.Unlock()
	var out []*mqtt.Client
	for cl, ident := range b.acl.ids {
		if ident.Tenant == tenant && ident.ID == id {
			out = append(out, cl)
		}
	}
	return out
}

func subscribed(cl *mqtt.Client, topic string) bool {
	for f := range cl.State.Subscriptions.GetAll() {
		if match(f, topic) {
			return true
		}
	}
	return false
}

// match reports whether an MQTT topic filter matches a topic.
func match(filter, topic string) bool {
	fs, ts := strings.Split(filter, "/"), strings.Split(topic, "/")
	for i, f := range fs {
		if f == "#" {
			return true
		}
		if i >= len(ts) || f != "+" && f != ts[i] {
			return false
		}
	}
	return len(fs) == len(ts)
}

func (b *Broker) deviceTopic(tenant, id, sub string) string {
	return b.root(tenant) + "devices/" + id + "/" + sub
}

// DeviceConnected reports whether a device has a session.
func (b *Broker) DeviceConnected(tenant, id string) bool { return len(b.sessions(tenant, id)) > 0 }

// DeviceListening reports whether a device has a session subscribed to
// devices/{id}/{sub}.
func (b *Broker) DeviceListening(tenant, id, sub string) bool {
	t := b.deviceTopic(tenant, id, sub)
	for _, cl := range b.sessions(tenant, id) {
		if subscribed(cl, t) {
			return true
		}
	}
	return false
}

// SendToDevice writes a message to the device's subscribed sessions only
// (never through routing) and returns how many got it.
func (b *Broker) SendToDevice(tenant, id, sub string, payload []byte) int {
	t := b.deviceTopic(tenant, id, sub)
	n := 0
	for _, cl := range b.sessions(tenant, id) {
		if !subscribed(cl, t) {
			continue
		}
		pk := packets.Packet{FixedHeader: packets.FixedHeader{Type: packets.Publish}, TopicName: t, Payload: payload}
		if cl.WritePacket(pk) == nil {
			n++
		}
	}
	return n
}
