// Package connect pulls data from industrial equipment into the hub:
// Modbus TCP/RTU (polled, block reads) and OPC UA (subscriptions). Each
// connector turns tags into fields of a hub sensor and hands them to the
// same ingest pipeline as MQTT and HTTP, so PLC data gets charts, storage,
// analytics and anomaly detection with no further setup.
package connect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sync"
	"time"
)

// Duration accepts "500ms", "2s" in JSON.
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"1s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

func (d Duration) or(def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return time.Duration(d)
}

// Config is the connectors file (-connectors).
type Config struct {
	Modbus []ModbusConfig `json:"modbus"`
	OPCUA  []OPCUAConfig  `json:"opcua"`
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields() // a typo in a tag name should fail loudly, not read nothing
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for i := range c.Modbus {
		if err := c.Modbus[i].validate(); err != nil {
			return nil, fmt.Errorf("modbus %q: %w", c.Modbus[i].Name, err)
		}
	}
	for i := range c.OPCUA {
		if err := c.OPCUA[i].validate(); err != nil {
			return nil, fmt.Errorf("opcua %q: %w", c.OPCUA[i].Name, err)
		}
	}
	return &c, nil
}

// Field metadata a connector declares for its sensor (units, labels).
type FieldInfo struct {
	Unit  string
	Label string
}

// Target is where connectors deliver data.
type Target struct {
	// Ingest receives one sample set for a sensor.
	Ingest func(sensor string, ts int64, values map[string]any)
	// Define declares the fields a sensor will carry (optional).
	Define func(sensor string, fields map[string]FieldInfo)
	Logf   func(format string, args ...any)
}

type Status struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Endpoint  string `json:"endpoint"`
	Connected bool   `json:"connected"`
	LastOK    int64  `json:"lastOk,omitempty"`
	LastError string `json:"lastError,omitempty"`
	Reads     uint64 `json:"reads"`
	Errors    uint64 `json:"errors"`
	Published uint64 `json:"published"` // field values sent to the hub
	Skipped   uint64 `json:"skipped"`   // unchanged values suppressed by deadband
}

type Connector interface {
	Run(ctx context.Context)
	Status() Status
}

// Start launches every configured connector.
func Start(ctx context.Context, c *Config, t Target) []Connector {
	var out []Connector
	for i := range c.Modbus {
		out = append(out, newModbus(c.Modbus[i], t))
	}
	for i := range c.OPCUA {
		out = append(out, newOPCUA(c.OPCUA[i], t))
	}
	for _, conn := range out {
		go conn.Run(ctx)
	}
	return out
}

// status is the shared, lock-protected Status implementation.
type status struct {
	mu sync.Mutex
	s  Status
}

func (st *status) get() Status {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.s
}

func (st *status) ok(n int) {
	st.mu.Lock()
	st.s.Connected, st.s.LastOK, st.s.LastError = true, time.Now().UnixMilli(), ""
	st.s.Reads += uint64(n)
	st.mu.Unlock()
}

func (st *status) fail(err error) {
	st.mu.Lock()
	st.s.Connected, st.s.LastError = false, err.Error()
	st.s.Errors++
	st.mu.Unlock()
}

func (st *status) counted(published, skipped int) {
	st.mu.Lock()
	st.s.Published += uint64(published)
	st.s.Skipped += uint64(skipped)
	st.mu.Unlock()
}

// filter implements report-by-exception per field. By default every sample
// is sent, which keeps averages unbiased. A field with onChange (or a
// deadband) is sent only when it changes (numbers: by more than the
// deadband), and at least once per heartbeat so it never looks stale.
// Exception reporting cuts stored rows sharply for slow tags, at the cost
// of sample-weighted averages over-weighting busy periods.
type filter struct {
	heartbeat time.Duration
	last      map[string]any
	sentAt    map[string]time.Time
}

func newFilter(heartbeat time.Duration) *filter {
	return &filter{heartbeat: heartbeat, last: map[string]any{}, sentAt: map[string]time.Time{}}
}

// exception describes a field's report-by-exception setting.
type exception struct {
	on       bool
	deadband float64
}

// apply returns the subset of values to publish.
func (f *filter) apply(now time.Time, values map[string]any, rule func(field string) exception) (map[string]any, int) {
	out := make(map[string]any, len(values))
	skipped := 0
	for k, v := range values {
		prev, seen := f.last[k]
		r := rule(k)
		send := !r.on || !seen || now.Sub(f.sentAt[k]) >= f.heartbeat
		if !send {
			x, xn := v.(float64)
			p, pn := prev.(float64)
			if xn && pn {
				send = math.Abs(x-p) > r.deadband || (r.deadband == 0 && x != p)
			} else {
				send = prev != v
			}
		}
		if send {
			out[k] = v
			f.last[k] = v
			f.sentAt[k] = now
		} else {
			skipped++
		}
	}
	return out, skipped
}

// backoff grows retry delays to 30 s so an offline PLC costs almost nothing.
type backoff struct{ d time.Duration }

func (b *backoff) next() time.Duration {
	if b.d == 0 {
		b.d = time.Second
	} else if b.d < 30*time.Second {
		b.d *= 2
	}
	return min(b.d, 30*time.Second)
}

func (b *backoff) reset() { b.d = 0 }

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

var errConfig = errors.New("invalid configuration")
