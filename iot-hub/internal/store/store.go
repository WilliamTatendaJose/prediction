// Package store holds sensor definitions, recent readings and the dashboard
// layout. Readings live only in fixed-size in-memory rings, so memory use is
// bounded by MaxSensors * MaxFields * Capacity * 12 bytes. Definitions and the
// dashboard are persisted to a JSON file.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/williamtatendajose/prediction/iot-hub/internal/anomaly"
	"github.com/williamtatendajose/prediction/iot-hub/internal/calc"
	"github.com/williamtatendajose/prediction/iot-hub/internal/oee"
)

var (
	ErrNotFound = errors.New("not found")
	ErrLimit    = errors.New("limit reached")
	ErrInvalid  = errors.New("invalid")
)

var idRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

func ValidID(s string) bool { return idRe.MatchString(s) }

type Field struct {
	Label string   `json:"label,omitempty"`
	Unit  string   `json:"unit,omitempty"`
	Min   *float64 `json:"min,omitempty"` // display range
	Max   *float64 `json:"max,omitempty"`
	// Detect configures anomaly detection; nil uses the server defaults.
	Detect *anomaly.Rule `json:"detect,omitempty"`
	// Type is learnt from the first value: number | bool | text. The
	// database stores booleans as 0/1, so restore needs it to give them back.
	Type string `json:"type,omitempty"`
	// Calc makes this a calculated field (formula or integral).
	Calc *calc.Spec `json:"calc,omitempty"`
}

func kindOf(v any) string {
	switch v.(type) {
	case bool:
		return "bool"
	case string:
		return "text"
	}
	return "number"
}

type Sensor struct {
	ID       string           `json:"id"`
	Name     string           `json:"name,omitempty"`
	Kind     string           `json:"kind,omitempty"`
	Location string           `json:"location,omitempty"`
	Fields   map[string]Field `json:"fields"`
	// OEE makes this sensor a machine with efficiency tracking.
	OEE *oee.Config `json:"oee,omitempty"`
}

// SensorView is a definition plus its latest state.
type SensorView struct {
	Sensor
	Last     map[string]any `json:"last"`
	LastSeen int64          `json:"lastSeen"`
}

// Reading is one ingested sample; the JSON shape is what the live stream sends.
type Reading struct {
	Sensor string         `json:"s"`
	TS     int64          `json:"t"`
	Values map[string]any `json:"v"`
	// Changed lists string fields whose value differs from the previous one;
	// only these are worth persisting.
	Changed []string `json:"-"`
}

type Options struct {
	Path         string // persistence file; empty disables persistence
	Capacity     int    // points kept per field
	MaxSensors   int
	MaxFields    int // per sensor
	AutoRegister bool
}

type entry struct {
	def    Sensor
	series map[string]*Ring
	last   map[string]any
	seen   int64
}

type Store struct {
	opts      Options
	mu        sync.RWMutex
	sensors   map[string]*entry
	dashboard json.RawMessage
	dirty     chan struct{}
}

func New(opts Options) *Store {
	if opts.Capacity <= 0 {
		opts.Capacity = 1024
	}
	if opts.MaxSensors <= 0 {
		opts.MaxSensors = 500
	}
	if opts.MaxFields <= 0 {
		opts.MaxFields = 16
	}
	return &Store{
		opts:      opts,
		sensors:   map[string]*entry{},
		dashboard: json.RawMessage(`{"tiles":[]}`),
		dirty:     make(chan struct{}, 1),
	}
}

func (s *Store) markDirty() {
	select {
	case s.dirty <- struct{}{}:
	default:
	}
}

func (s *Store) newEntry(def Sensor) *entry {
	if def.Fields == nil {
		def.Fields = map[string]Field{}
	}
	return &entry{def: def, series: map[string]*Ring{}, last: map[string]any{}}
}

func (e *entry) view() SensorView {
	last := make(map[string]any, len(e.last))
	for k, v := range e.last {
		last[k] = v
	}
	def := e.def
	def.Fields = make(map[string]Field, len(e.def.Fields))
	for k, v := range e.def.Fields {
		def.Fields[k] = v
	}
	return SensorView{Sensor: def, Last: last, LastSeen: e.seen}
}

func (s *Store) List() []SensorView {
	s.mu.RLock()
	out := make([]SensorView, 0, len(s.sensors))
	for _, e := range s.sensors {
		out = append(out, e.view())
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.sensors)
}

func (s *Store) Get(id string) (SensorView, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.sensors[id]
	if !ok {
		return SensorView{}, ErrNotFound
	}
	return e.view(), nil
}

// Upsert creates or replaces a sensor definition, keeping its data.
func (s *Store) Upsert(def Sensor) error {
	if !ValidID(def.ID) {
		return fmt.Errorf("%w: id must match %s", ErrInvalid, idRe)
	}
	if len(def.Fields) > s.opts.MaxFields {
		return fmt.Errorf("%w: max %d fields", ErrLimit, s.opts.MaxFields)
	}
	specs := map[string]*calc.Spec{}
	for k, f := range def.Fields {
		if !ValidID(k) {
			return fmt.Errorf("%w: field %q", ErrInvalid, k)
		}
		if f.Calc != nil {
			specs[k] = f.Calc
		}
	}
	if err := calc.Validate(specs); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if def.OEE != nil {
		if err := def.OEE.Validate(); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		for _, f := range def.OEE.Fields() {
			if !ValidID(f) {
				return fmt.Errorf("%w: oee field %q", ErrInvalid, f)
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.sensors[def.ID]
	if !ok {
		if len(s.sensors) >= s.opts.MaxSensors {
			return fmt.Errorf("%w: max %d sensors", ErrLimit, s.opts.MaxSensors)
		}
		s.sensors[def.ID] = s.newEntry(def)
	} else {
		if def.Fields == nil {
			def.Fields = map[string]Field{}
		}
		// Fields already carrying data stay registered so they remain visible.
		for k := range e.last {
			if _, ok := def.Fields[k]; !ok {
				def.Fields[k] = e.def.Fields[k]
			}
		}
		e.def = def
	}
	s.markDirty()
	return nil
}

func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sensors[id]; !ok {
		return ErrNotFound
	}
	delete(s.sensors, id)
	s.markDirty()
	return nil
}

// Ingest records values for a sensor. Numbers and booleans are stored as time
// series; strings are kept as latest state only. ts <= 0 means now.
func (s *Store) Ingest(id string, ts int64, values map[string]any) (Reading, error) {
	if !ValidID(id) {
		return Reading{}, fmt.Errorf("%w: sensor id", ErrInvalid)
	}
	if ts <= 0 {
		ts = time.Now().UnixMilli()
	}
	clean := make(map[string]any, len(values))
	for k, v := range values {
		if !ValidID(k) {
			continue
		}
		switch x := v.(type) {
		case float64:
			clean[k] = x
		case bool:
			clean[k] = x
		case string:
			if len(x) > 256 {
				x = x[:256]
			}
			clean[k] = x
		} // nested objects, arrays and nulls are ignored
	}
	if len(clean) == 0 {
		return Reading{}, fmt.Errorf("%w: no usable values", ErrInvalid)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.sensors[id]
	if !ok {
		if !s.opts.AutoRegister {
			return Reading{}, ErrNotFound
		}
		if len(s.sensors) >= s.opts.MaxSensors {
			return Reading{}, fmt.Errorf("%w: max %d sensors", ErrLimit, s.opts.MaxSensors)
		}
		e = s.newEntry(Sensor{ID: id, Name: id})
		s.sensors[id] = e
		s.markDirty()
	}
	var changed []string
	for k, v := range clean {
		if _, known := e.def.Fields[k]; !known {
			if !s.opts.AutoRegister || len(e.def.Fields) >= s.opts.MaxFields {
				delete(clean, k)
				continue
			}
			e.def.Fields[k] = Field{}
			s.markDirty()
		}
		if f := e.def.Fields[k]; f.Type == "" {
			f.Type = kindOf(v)
			e.def.Fields[k] = f
			s.markDirty()
		}
		if str, ok := v.(string); ok && e.last[k] != str {
			changed = append(changed, k)
		}
		e.last[k] = v
		var f float32
		switch x := v.(type) {
		case float64:
			f = float32(x)
		case bool:
			if x {
				f = 1
			}
		default:
			continue
		}
		r := e.series[k]
		if r == nil {
			r = NewRing(s.opts.Capacity)
			e.series[k] = r
		}
		r.Push(ts, f)
	}
	if len(clean) == 0 {
		return Reading{}, fmt.Errorf("%w: no registered fields in payload", ErrInvalid)
	}
	if ts > e.seen {
		e.seen = ts
	}
	return Reading{Sensor: id, TS: ts, Values: clean, Changed: changed}, nil
}

// Restore loads persisted history into a sensor's live state after a
// restart: numeric points into the ring buffer (oldest first), and the
// newest value as "last". Unknown sensors are recreated if auto-register
// is on. It does not publish, persist or run detection.
func (s *Store) Restore(sensor, field string, ts []int64, vals []float64, text string, textTS int64) bool {
	if !ValidID(sensor) || !ValidID(field) {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.sensors[sensor]
	if !ok {
		if !s.opts.AutoRegister || len(s.sensors) >= s.opts.MaxSensors {
			return false
		}
		e = s.newEntry(Sensor{ID: sensor, Name: sensor})
		s.sensors[sensor] = e
		s.markDirty()
	}
	f, known := e.def.Fields[field]
	if !known {
		if !s.opts.AutoRegister || len(e.def.Fields) >= s.opts.MaxFields {
			return false
		}
		e.def.Fields[field] = Field{}
		s.markDirty()
	}
	var lastTS int64
	var last any
	if len(ts) > 0 {
		r := e.series[field]
		if r == nil {
			r = NewRing(s.opts.Capacity)
			e.series[field] = r
		}
		start := max(0, len(ts)-s.opts.Capacity)
		for i := start; i < len(ts); i++ {
			r.Push(ts[i], float32(vals[i]))
		}
		lastTS, last = ts[len(ts)-1], vals[len(vals)-1]
		if f.Type == "bool" {
			last = vals[len(vals)-1] != 0
		}
	}
	if textTS > lastTS {
		lastTS, last = textTS, text
	}
	if last == nil {
		return false
	}
	if _, live := e.last[field]; !live { // never overwrite data that arrived meanwhile
		e.last[field] = last
	}
	if lastTS > e.seen {
		e.seen = lastTS
	}
	return true
}

// CalcState returns a sensor's calculated-field specs and a copy of its
// latest values; nil specs when it has none (the common, cheap case).
func (s *Store) CalcState(sensor string) (map[string]*calc.Spec, map[string]any) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.sensors[sensor]
	if !ok {
		return nil, nil
	}
	var specs map[string]*calc.Spec
	for k, f := range e.def.Fields {
		if f.Calc != nil {
			if specs == nil {
				specs = map[string]*calc.Spec{}
			}
			specs[k] = f.Calc
		}
	}
	if specs == nil {
		return nil, nil
	}
	last := make(map[string]any, len(e.last))
	for k, v := range e.last {
		last[k] = v
	}
	return specs, last
}

// Rule returns the anomaly rule for a field (zero value if none).
func (s *Store) Rule(sensor, field string) anomaly.Rule {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if e, ok := s.sensors[sensor]; ok {
		if f, ok := e.def.Fields[field]; ok && f.Detect != nil {
			return *f.Detect
		}
	}
	return anomaly.Rule{}
}

func (s *Store) History(id, field string, limit int, since int64) ([]int64, []float32, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.sensors[id]
	if !ok {
		return nil, nil, ErrNotFound
	}
	r, ok := e.series[field]
	if !ok {
		return []int64{}, []float32{}, nil
	}
	ts, v := r.Last(limit, since)
	return ts, v, nil
}

func (s *Store) Dashboard() json.RawMessage {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dashboard
}

// SetDashboard stores the layout as opaque JSON: the server does not need to
// know tile types, so new tiles are a frontend-only change.
func (s *Store) SetDashboard(raw json.RawMessage) error {
	if !json.Valid(raw) {
		return fmt.Errorf("%w: dashboard is not valid JSON", ErrInvalid)
	}
	cp := append(json.RawMessage(nil), raw...)
	s.mu.Lock()
	s.dashboard = cp
	s.mu.Unlock()
	s.markDirty()
	return nil
}

type persisted struct {
	Sensors   []Sensor        `json:"sensors"`
	Dashboard json.RawMessage `json:"dashboard"`
}

func (s *Store) Load() error {
	if s.opts.Path == "" {
		return nil
	}
	b, err := os.ReadFile(s.opts.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var p persisted
	if err := json.Unmarshal(b, &p); err != nil {
		return fmt.Errorf("parse %s: %w", s.opts.Path, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range p.Sensors {
		if ValidID(d.ID) {
			s.sensors[d.ID] = s.newEntry(d)
		}
	}
	if len(p.Dashboard) > 0 {
		s.dashboard = p.Dashboard
	}
	return nil
}

// Save writes atomically (temp file + rename) so a crash never leaves a
// truncated config.
func (s *Store) Save() error {
	if s.opts.Path == "" {
		return nil
	}
	s.mu.RLock()
	p := persisted{Dashboard: s.dashboard, Sensors: make([]Sensor, 0, len(s.sensors))}
	for _, e := range s.sensors {
		p.Sensors = append(p.Sensors, e.view().Sensor)
	}
	s.mu.RUnlock()
	sort.Slice(p.Sensors, func(i, j int) bool { return p.Sensors[i].ID < p.Sensors[j].ID })
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.opts.Path), 0o755); err != nil {
		return err
	}
	tmp := s.opts.Path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.opts.Path)
}

// RunPersist saves at most once per interval after changes, and once more on
// shutdown.
func (s *Store) RunPersist(ctx context.Context, interval time.Duration, logf func(string, ...any)) {
	for {
		select {
		case <-ctx.Done():
			if err := s.Save(); err != nil {
				logf("persist: %v", err)
			}
			return
		case <-s.dirty:
			if err := s.Save(); err != nil {
				logf("persist: %v", err)
			}
			select {
			case <-ctx.Done():
			case <-time.After(interval):
			}
		}
	}
}
