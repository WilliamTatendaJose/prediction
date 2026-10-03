// Package twin gives every device identity a device twin, direct methods
// and a cloud-to-device message queue, after Azure IoT Hub:
//
//   - Twin: tags (cloud only, for grouping and queries), desired
//     properties (written by the cloud, pushed to the device) and reported
//     properties (written by the device). Updates are JSON merge patches
//     (null deletes); desired and reported each carry a $version so a
//     device that reconnects can tell what it missed.
//   - Direct methods: a synchronous call to a connected device, answered
//     within a timeout. A device that isn't listening fails at once, as in
//     Azure, so the caller knows to retry or queue.
//   - Cloud-to-device messages: queued per device with a TTL, delivered
//     when it listens, locked while it works and redelivered if it doesn't
//     complete them; dead-lettered after MaxDeliveries.
//
// The package is transport-independent: the broker and the HTTP API feed
// it and deliver what it sends.
package twin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/williamtatendajose/prediction/iot-hub/internal/store"
)

var (
	ErrNotFound     = store.ErrNotFound
	ErrPrecondition = errors.New("twin changed since you read it (etag mismatch)")
	ErrOffline      = errors.New("device is not connected and listening")
	ErrTimeout      = errors.New("device did not answer in time")
	ErrQueueFull    = errors.New("message queue for this device is full")
)

func invalid(f string, a ...any) error {
	return fmt.Errorf("%w: %s", store.ErrInvalid, fmt.Sprintf(f, a...))
}

// Limits, as in Azure IoT Hub.
const (
	MaxTags         = 8 << 10
	MaxDesired      = 32 << 10
	MaxReported     = 32 << 10
	MaxDepth        = 5
	MaxPayload      = 128 << 10 // method request/response
	MaxMessageBody  = 64 << 10
	MaxQueued       = 50
	MaxDeliveries   = 10
	LockTime        = time.Minute
	DefaultTTL      = time.Hour
	MaxTTL          = 48 * time.Hour
	MaxMethodWait   = 5 * time.Minute
	keepHistory     = 100
	keepCompletedMs = 24 * 3600 * 1000
)

var (
	keyRe    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	methodRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
)

// Message is a cloud-to-device message.
type Message struct {
	ID         string          `json:"id"`
	Body       json.RawMessage `json:"body"`
	Created    int64           `json:"created"`
	Expires    int64           `json:"expires"`
	Status     string          `json:"status"` // queued | delivered | completed | rejected | expired | deadlettered
	Deliveries int             `json:"deliveries"`
	Done       int64           `json:"done,omitempty"`
	Locked     int64           `json:"-"`
}

func (m *Message) active() bool { return m.Status == "queued" || m.Status == "delivered" }

type rec struct {
	Tags        map[string]any `json:"tags"`
	Desired     map[string]any `json:"desired"`
	Reported    map[string]any `json:"reported"`
	Version     int64          `json:"version"`
	DesiredVer  int64          `json:"desiredVersion"`
	ReportedVer int64          `json:"reportedVersion"`
	DesiredAt   int64          `json:"desiredAt,omitempty"`
	ReportedAt  int64          `json:"reportedAt,omitempty"`
	Activity    int64          `json:"lastActivity,omitempty"`
	LastData    int64          `json:"lastData,omitempty"`    // last accepted reading from this identity
	ExpectedSec int64          `json:"expectedSec,omitempty"` // how often it should send data; 0 = not set
	Runs        []Run          `json:"runs,omitempty"`        // command history, oldest first
	Messages    []*Message     `json:"messages,omitempty"`
}

// View is a twin as the API shows it (Azure's shape).
type View struct {
	DeviceID        string         `json:"deviceId"`
	ETag            string         `json:"etag"`
	Version         int64          `json:"version"`
	ConnectionState string         `json:"connectionState"`
	LastActivity    int64          `json:"lastActivityTime,omitempty"`
	LastData        int64          `json:"lastDataTime,omitempty"`
	Expected        int64          `json:"expectedIntervalSec,omitempty"`
	Tags            map[string]any `json:"tags"`
	Properties      struct {
		Desired  map[string]any `json:"desired"`
		Reported map[string]any `json:"reported"`
	} `json:"properties"`
}

type call struct {
	device string
	ch     chan reply
}

type reply struct {
	status  int
	payload json.RawMessage
}

// Transport is how the service reaches devices (the broker).
type Transport struct {
	// Listening: does the device have a session subscribed to devices/{id}/{sub}?
	Listening func(id, sub string) bool
	// Connected: does the device have any session?
	Connected func(id string) bool
	// Send writes to the device's listening sessions; returns how many.
	Send func(id, sub string, payload []byte) int
}

type Service struct {
	Exists func(id string) bool // the identity exists in this tenant
	tr     Transport

	mu      sync.Mutex
	path    string
	devs    map[string]*rec
	pending map[string]*call
	dirty   chan struct{}
	now     func() time.Time
	flushed int64 // when Data last asked for a save (ms)

	// OnData, if set, is told about every accepted reading (after Data
	// records it): the overdue check clears its alarm at once.
	OnData func(id string)

	cmds    map[string]Command // the command catalog (commands.go)
	cmdSave sync.Mutex
	batches []*Batch // recent batches, newest last (batch.go)
}

func New(path string, exists func(string) bool) *Service {
	return &Service{Exists: exists, path: path, devs: map[string]*rec{}, pending: map[string]*call{},
		dirty: make(chan struct{}, 1), now: time.Now,
		tr: Transport{Listening: func(string, string) bool { return false }, Connected: func(string) bool { return false },
			Send: func(string, string, []byte) int { return 0 }}}
}

// SetTransport connects the service to the broker.
func (s *Service) SetTransport(t Transport) {
	s.mu.Lock()
	s.tr = t
	s.mu.Unlock()
}

func (s *Service) transport() Transport {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tr
}

func (s *Service) markDirty() {
	select {
	case s.dirty <- struct{}{}:
	default:
	}
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// rec returns (creating) a device's record; callers hold s.mu.
func (s *Service) recLocked(id string) *rec {
	r := s.devs[id]
	if r == nil {
		r = &rec{Tags: map[string]any{}, Desired: map[string]any{}, Reported: map[string]any{}, Version: 1}
		s.devs[id] = r
	}
	return r
}

func (s *Service) check(id string) error {
	if s.Exists != nil && !s.Exists(id) {
		return fmt.Errorf("%w: device %s", ErrNotFound, id)
	}
	return nil
}

// ---- merge patches ----------------------------------------------------------

func validate(v any, depth int) error {
	switch x := v.(type) {
	case nil, bool, float64, string:
		return nil
	case []any:
		if len(x) > 100 {
			return invalid("arrays hold at most 100 items")
		}
		for _, e := range x {
			if _, ok := e.(map[string]any); ok {
				return invalid("arrays hold values, not objects")
			}
			if err := validate(e, depth); err != nil {
				return err
			}
		}
		return nil
	case map[string]any:
		if depth > MaxDepth {
			return invalid("nested deeper than %d levels", MaxDepth)
		}
		for k, e := range x {
			if !keyRe.MatchString(k) {
				return invalid("property name %q: letters, digits, _ and - (names starting with $ are reserved)", k)
			}
			if err := validate(e, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	return invalid("unsupported value %T", v)
}

// merge applies an RFC 7386 merge patch, returning a new map.
func merge(dst, patch map[string]any) map[string]any {
	out := make(map[string]any, len(dst)+len(patch))
	for k, v := range dst {
		out[k] = v
	}
	for k, v := range patch {
		switch pv := v.(type) {
		case nil:
			delete(out, k)
		case map[string]any:
			dv, _ := out[k].(map[string]any)
			out[k] = merge(dv, pv)
		default:
			out[k] = v
		}
	}
	return out
}

func size(m map[string]any) int {
	b, _ := json.Marshal(m)
	return len(b)
}

func (s *Service) viewLocked(id string, r *rec) View {
	v := View{DeviceID: id, Version: r.Version, ETag: strconv.Quote(strconv.FormatInt(r.Version, 10)), Tags: r.Tags, LastActivity: r.Activity, LastData: r.LastData, Expected: r.ExpectedSec}
	v.ConnectionState = "Disconnected"
	if s.tr.Connected(id) {
		v.ConnectionState = "Connected"
	}
	v.Properties.Desired = merge(r.Desired, map[string]any{"$version": r.DesiredVer, "$lastUpdated": r.DesiredAt})
	v.Properties.Reported = merge(r.Reported, map[string]any{"$version": r.ReportedVer, "$lastUpdated": r.ReportedAt})
	return v
}

// Get returns a device's twin.
func (s *Service) Get(id string) (View, error) {
	if err := s.check(id); err != nil {
		return View{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.viewLocked(id, s.recLocked(id)), nil
}

// Patch is a backend update: tags and desired properties.
type Patch struct {
	Tags       map[string]any `json:"tags,omitempty"`
	Properties *struct {
		Desired map[string]any `json:"desired,omitempty"`
	} `json:"properties,omitempty"`
}

func parsePatch(raw []byte) (Patch, error) {
	var p Patch
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return p, invalid("patch: %v (want {\"tags\":{…},\"properties\":{\"desired\":{…}}})", err)
	}
	if err := validate(p.Tags, 1); err != nil {
		return p, err
	}
	if p.Properties != nil {
		if err := validate(p.Properties.Desired, 1); err != nil {
			return p, err
		}
	}
	return p, nil
}

// applyLocked updates one twin and returns the desired patch to push.
func (s *Service) applyLocked(id string, p Patch, ifMatch string) (View, []byte, error) {
	r := s.recLocked(id)
	if ifMatch != "" && ifMatch != "*" && ifMatch != strconv.Quote(strconv.FormatInt(r.Version, 10)) {
		return View{}, nil, ErrPrecondition
	}
	tags, desired := r.Tags, r.Desired
	if p.Tags != nil {
		tags = merge(r.Tags, p.Tags)
		if size(tags) > MaxTags {
			return View{}, nil, invalid("tags over %d bytes", MaxTags)
		}
	}
	var push []byte
	if p.Properties != nil && p.Properties.Desired != nil {
		desired = merge(r.Desired, p.Properties.Desired)
		if size(desired) > MaxDesired {
			return View{}, nil, invalid("desired properties over %d bytes", MaxDesired)
		}
	}
	if p.Tags == nil && (p.Properties == nil || p.Properties.Desired == nil) {
		return s.viewLocked(id, r), nil, nil
	}
	r.Tags = tags
	if p.Properties != nil && p.Properties.Desired != nil {
		r.Desired = desired
		r.DesiredVer++
		r.DesiredAt = s.now().UnixMilli()
		// The device gets the patch itself, nulls included (they delete).
		out := make(map[string]any, len(p.Properties.Desired)+1)
		for k, v := range p.Properties.Desired {
			out[k] = v
		}
		out["$version"] = r.DesiredVer
		push, _ = json.Marshal(out)
	}
	r.Version++
	s.markDirty()
	return s.viewLocked(id, r), push, nil
}

// Update applies a backend patch; ifMatch (an etag, "*" or "") guards
// against overwriting a change made since it was read. A desired change is
// pushed to the device at once.
func (s *Service) Update(id, ifMatch string, raw []byte) (View, error) {
	if err := s.check(id); err != nil {
		return View{}, err
	}
	p, err := parsePatch(raw)
	if err != nil {
		return View{}, err
	}
	s.mu.Lock()
	v, push, err := s.applyLocked(id, p, ifMatch)
	tr := s.tr
	s.mu.Unlock()
	if push != nil {
		tr.Send(id, "twin/desired", push)
	}
	return v, err
}

// Filter matches a twin field (dotted path) to a value, e.g.
// tags.site=plant1, properties.reported.firmware=1.2, connectionState=Connected.
type Filter struct {
	Path  []string
	Value string
}

func ParseFilters(qs []string) ([]Filter, error) {
	var out []Filter
	for _, q := range qs {
		k, v, ok := strings.Cut(q, "=")
		if !ok || k == "" {
			return nil, invalid("where %q: want path=value", q)
		}
		out = append(out, Filter{Path: strings.Split(k, "."), Value: v})
	}
	return out, nil
}

func (v View) lookup(path []string) (any, bool) {
	var cur any
	b, _ := json.Marshal(v)
	_ = json.Unmarshal(b, &cur)
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[p]; !ok {
			return nil, false
		}
	}
	return cur, true
}

func (v View) matches(fs []Filter) bool {
	for _, f := range fs {
		x, ok := v.lookup(f.Path)
		if !ok {
			return false
		}
		switch t := x.(type) {
		case string:
			if t != f.Value {
				return false
			}
		case map[string]any, []any:
			return false
		default:
			if fmt.Sprint(t) != f.Value {
				return false
			}
		}
	}
	return true
}

// Query lists the twins of the given devices that match every filter.
func (s *Service) Query(ids []string, fs []Filter) []View {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []View{}
	for _, id := range ids {
		v := s.viewLocked(id, s.recLocked(id))
		if v.matches(fs) {
			out = append(out, v)
		}
	}
	return out
}

// UpdateMany applies one patch to every listed device matching the filters
// (Azure's twin update jobs) and returns the devices changed.
func (s *Service) UpdateMany(ids []string, fs []Filter, raw []byte) ([]string, error) {
	p, err := parsePatch(raw)
	if err != nil {
		return nil, err
	}
	type push struct {
		id string
		b  []byte
	}
	var pushes []push
	var changed []string
	s.mu.Lock()
	for _, id := range ids {
		if !s.viewLocked(id, s.recLocked(id)).matches(fs) {
			continue
		}
		_, b, err := s.applyLocked(id, p, "")
		if err != nil {
			s.mu.Unlock()
			return changed, fmt.Errorf("%s: %w", id, err)
		}
		changed = append(changed, id)
		if b != nil {
			pushes = append(pushes, push{id, b})
		}
	}
	tr := s.tr
	s.mu.Unlock()
	for _, p := range pushes {
		tr.Send(p.id, "twin/desired", p.b)
	}
	return changed, nil
}

// DeviceView is what a device gets: desired and reported, not tags.
func (s *Service) DeviceView(id string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.viewLocked(id, s.recLocked(id))
	return map[string]any{"desired": v.Properties.Desired, "reported": v.Properties.Reported}
}

// Report applies a device's reported-properties patch.
func (s *Service) Report(id string, raw []byte) (int64, error) {
	var p map[string]any
	if err := json.Unmarshal(raw, &p); err != nil || p == nil {
		return 0, invalid("reported properties must be a JSON object")
	}
	if err := validate(p, 1); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.recLocked(id)
	next := merge(r.Reported, p)
	if size(next) > MaxReported {
		return 0, invalid("reported properties over %d bytes", MaxReported)
	}
	r.Reported = next
	r.ReportedVer++
	r.Version++
	r.ReportedAt = s.now().UnixMilli()
	r.Activity = r.ReportedAt
	s.markDirty()
	return r.ReportedVer, nil
}

// Touch records device activity (connect, any message).
func (s *Service) Touch(id string) {
	s.mu.Lock()
	if r := s.devs[id]; r != nil {
		r.Activity = s.now().UnixMilli()
	}
	s.mu.Unlock()
}

// Data records that a reading from id was accepted (any transport). It is
// called per reading, so it saves at most once a minute: after a restart
// the time is at most a minute old.
func (s *Service) Data(id string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	r := s.devs[id]
	s.mu.Unlock()
	if r == nil && s.Exists != nil && !s.Exists(id) {
		return
	}
	now := s.now().UnixMilli()
	s.mu.Lock()
	r = s.recLocked(id)
	r.LastData, r.Activity = now, now
	save := now-s.flushed >= 60_000
	if save {
		s.flushed = now
	}
	s.mu.Unlock()
	if save {
		s.markDirty()
	}
	if s.OnData != nil {
		s.OnData(id)
	}
}

// Delete forgets a device (its identity was removed).
func (s *Service) Delete(id string) {
	s.mu.Lock()
	delete(s.devs, id)
	s.mu.Unlock()
	s.markDirty()
}

// ---- direct methods -------------------------------------------------------------

// Result is a direct method's answer.
type Result struct {
	Status  int             `json:"status"`
	Payload json.RawMessage `json:"payload"`
}

// Invoke calls a method on a device and waits for its answer.
func (s *Service) Invoke(ctx context.Context, id, name string, payload json.RawMessage, timeout time.Duration) (Result, error) {
	if err := s.check(id); err != nil {
		return Result{}, err
	}
	if !methodRe.MatchString(name) {
		return Result{}, invalid("method name: letters, digits, _ . - (up to 64)")
	}
	if len(payload) == 0 {
		payload = json.RawMessage("null")
	}
	if len(payload) > MaxPayload || !json.Valid(payload) {
		return Result{}, invalid("payload must be JSON of at most %d bytes", MaxPayload)
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	timeout = min(timeout, MaxMethodWait)
	tr := s.transport()
	rid := newID()
	if !tr.Listening(id, "methods/"+name+"/"+rid) {
		return Result{}, ErrOffline
	}
	c := &call{device: id, ch: make(chan reply, 1)}
	s.mu.Lock()
	s.pending[rid] = c
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, rid)
		s.mu.Unlock()
	}()
	if tr.Send(id, "methods/"+name+"/"+rid, payload) == 0 {
		return Result{}, ErrOffline
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case r := <-c.ch:
		return Result{Status: r.status, Payload: r.payload}, nil
	case <-t.C:
		return Result{}, ErrTimeout
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
}

// answer resolves a pending call; only the device it was sent to can.
func (s *Service) answer(id, rid string, status int, payload []byte) {
	s.mu.Lock()
	c := s.pending[rid]
	s.mu.Unlock()
	if c == nil || c.device != id {
		return
	}
	if len(payload) == 0 {
		payload = []byte("null")
	}
	if len(payload) > MaxPayload {
		status, payload = 502, []byte(`"device answer too large"`)
	} else if !json.Valid(payload) {
		payload, _ = json.Marshal(string(payload))
	}
	select {
	case c.ch <- reply{status, payload}:
	default:
	}
}

// ---- cloud-to-device messages ---------------------------------------------------

// Send queues a message for a device and delivers it if it is listening.
func (s *Service) Send(id string, body json.RawMessage, ttl time.Duration) (*Message, error) {
	if err := s.check(id); err != nil {
		return nil, err
	}
	if len(body) == 0 || len(body) > MaxMessageBody {
		return nil, invalid("message body: 1 byte to %d KiB", MaxMessageBody>>10)
	}
	if !json.Valid(body) {
		body, _ = json.Marshal(string(body))
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if ttl > MaxTTL {
		return nil, invalid("ttl at most %s", MaxTTL)
	}
	now := s.now()
	s.mu.Lock()
	r := s.recLocked(id)
	n := 0
	for _, m := range r.Messages {
		if m.active() {
			n++
		}
	}
	if n >= MaxQueued {
		s.mu.Unlock()
		return nil, ErrQueueFull
	}
	m := &Message{ID: newID(), Body: body, Created: now.UnixMilli(), Expires: now.Add(ttl).UnixMilli(), Status: "queued"}
	r.Messages = append(r.Messages, m)
	cp := *m
	s.markDirty()
	s.mu.Unlock()
	s.deliver(id)
	return &cp, nil
}

// deliver sends a device's queued messages if it is listening.
func (s *Service) deliver(id string) {
	tr := s.transport()
	if !tr.Listening(id, "messages/x") {
		return
	}
	type out struct {
		id   string
		body []byte
	}
	var send []out
	now := s.now().UnixMilli()
	s.mu.Lock()
	if r := s.devs[id]; r != nil {
		for _, m := range r.Messages {
			if m.Status == "queued" && m.Expires > now {
				m.Status, m.Locked = "delivered", now+LockTime.Milliseconds()
				m.Deliveries++
				send = append(send, out{m.ID, m.Body})
			}
		}
	}
	if len(send) > 0 {
		s.markDirty()
	}
	s.mu.Unlock()
	for _, o := range send {
		tr.Send(id, "messages/"+o.id, o.body)
	}
}

// Messages lists a device's messages, newest first.
func (s *Service) Messages(id string) ([]Message, error) {
	if err := s.check(id); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Message{}
	if r := s.devs[id]; r != nil {
		for i := len(r.Messages) - 1; i >= 0; i-- {
			out = append(out, *r.Messages[i])
		}
	}
	return out, nil
}

// Receive hands the next queued message to a device polling over HTTP,
// locking it until completed (or the lock runs out).
func (s *Service) Receive(id string) (*Message, error) {
	now := s.now().UnixMilli()
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.devs[id]
	if r == nil {
		return nil, nil
	}
	for _, m := range r.Messages {
		if m.Status == "queued" && m.Expires > now {
			m.Status, m.Locked = "delivered", now+LockTime.Milliseconds()
			m.Deliveries++
			r.Activity = now
			s.markDirty()
			cp := *m
			return &cp, nil
		}
	}
	return nil, nil
}

// Settle completes, rejects or abandons (requeues) a delivered message.
func (s *Service) Settle(id, mid, how string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.devs[id]
	if r == nil {
		return ErrNotFound
	}
	for _, m := range r.Messages {
		if m.ID != mid {
			continue
		}
		if m.Status != "delivered" {
			return invalid("message %s is %s", mid, m.Status)
		}
		switch how {
		case "complete":
			m.Status, m.Done = "completed", s.now().UnixMilli()
		case "reject":
			m.Status, m.Done = "rejected", s.now().UnixMilli()
		case "abandon":
			m.Status, m.Locked = "queued", 0
		default:
			return invalid("complete, reject or abandon")
		}
		s.markDirty()
		return nil
	}
	return ErrNotFound
}

// Tick expires messages, returns timed-out locks to the queue (or the
// dead-letter state after MaxDeliveries) and delivers what is due.
func (s *Service) Tick() {
	now := s.now().UnixMilli()
	s.tickBatches(now)
	var redeliver []string
	s.mu.Lock()
	for id, r := range s.devs {
		changed, due := false, false
		keep := r.Messages[:0]
		for _, m := range r.Messages {
			switch {
			case m.active() && m.Expires <= now:
				m.Status, m.Done, changed = "expired", now, true
			case m.Status == "delivered" && m.Locked <= now:
				if m.Deliveries >= MaxDeliveries {
					m.Status, m.Done = "deadlettered", now
				} else {
					m.Status, due = "queued", true
				}
				changed = true
			case m.Status == "queued":
				due = true
			}
			if m.active() || now-m.Done < keepCompletedMs {
				keep = append(keep, m)
			}
		}
		if len(keep) > keepHistory {
			keep = keep[len(keep)-keepHistory:]
		}
		r.Messages = keep
		if changed {
			s.markDirty()
		}
		if due {
			redeliver = append(redeliver, id)
		}
	}
	s.mu.Unlock()
	sort.Strings(redeliver)
	for _, id := range redeliver {
		s.deliver(id)
	}
}

// ---- device side over MQTT --------------------------------------------------------

// HandleMQTT processes what a device publishes under devices/{id}/:
//
//	twin/get/{rid}                      → twin/res/200/{rid} {desired, reported}
//	twin/reported/{rid}  {patch}        → twin/res/204/{rid} {"version": n} (or 400)
//	methods/res/{status}/{rid}  {body}  answers a direct method
//	messages/complete|reject|abandon/{mid}
func (s *Service) HandleMQTT(id, sub string, payload []byte) {
	s.Touch(id)
	parts := strings.Split(sub, "/")
	tr := s.transport()
	respond := func(status int, rid string, body any) {
		b, _ := json.Marshal(body)
		tr.Send(id, fmt.Sprintf("twin/res/%d/%s", status, rid), b)
	}
	switch {
	case len(parts) >= 2 && parts[0] == "twin" && parts[1] == "get":
		rid := "0"
		if len(parts) == 3 {
			rid = parts[2]
		}
		respond(200, rid, s.DeviceView(id))
	case len(parts) >= 2 && parts[0] == "twin" && parts[1] == "reported":
		rid := "0"
		if len(parts) == 3 {
			rid = parts[2]
		}
		v, err := s.Report(id, payload)
		if err != nil {
			respond(400, rid, map[string]string{"error": err.Error()})
			return
		}
		respond(204, rid, map[string]int64{"version": v})
	case len(parts) == 4 && parts[0] == "methods" && parts[1] == "res":
		status, err := strconv.Atoi(parts[2])
		if err != nil {
			status = 500
		}
		s.answer(id, parts[3], status, payload)
	case len(parts) == 3 && parts[0] == "messages":
		_ = s.Settle(id, parts[2], parts[1])
	}
}

// Listening is called when a device subscribes or connects: deliver
// anything queued.
func (s *Service) Listening(id string) {
	s.Touch(id)
	s.deliver(id)
}

// ---- persistence ------------------------------------------------------------------------

func (s *Service) Load() error {
	if s.path == "" {
		return nil
	}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := json.Unmarshal(b, &s.devs); err != nil {
		return fmt.Errorf("parse %s: %w", s.path, err)
	}
	for _, r := range s.devs {
		for _, m := range r.Messages {
			if m.Status == "delivered" { // the lock didn't survive the restart
				m.Status = "queued"
			}
		}
	}
	return nil
}

// LoadAll reads the twins and the command catalog.
func (s *Service) LoadAll() error {
	if err := s.Load(); err != nil {
		return err
	}
	if err := s.loadBatches(); err != nil {
		return err
	}
	return s.loadCommands()
}

func (s *Service) save() error {
	if s.path == "" {
		return nil
	}
	// One snapshot of both, so a device's history and its batch result
	// never disagree after a crash.
	s.mu.Lock()
	b, err := json.Marshal(s.devs)
	var bb []byte
	if err == nil {
		bb, err = json.Marshal(s.batches)
	}
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	if err := writeFile(s.batchPath(), bb); err != nil {
		return err
	}
	return writeFile(s.path, b) // desired properties can hold device configuration: 0600
}

func writeFile(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Run saves changes (at most once a second) and ticks the message queue
// until ctx ends, then saves a last time.
func (s *Service) Run(ctx context.Context, logf func(string, ...any)) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			if err := s.save(); err != nil {
				logf("twins: %v", err)
			}
			return
		case <-t.C:
			s.Tick()
		case <-s.dirty:
			if err := s.save(); err != nil {
				logf("twins: %v", err)
			}
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}
	}
}
