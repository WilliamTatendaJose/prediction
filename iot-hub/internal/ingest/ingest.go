// Package ingest is the single entry point for sensor data, shared by the
// REST API and the MQTT broker so both transports behave identically.
package ingest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/williamtatendajose/prediction/iot-hub/internal/alarm"
	"github.com/williamtatendajose/prediction/iot-hub/internal/anomaly"
	"github.com/williamtatendajose/prediction/iot-hub/internal/store"
	"github.com/williamtatendajose/prediction/iot-hub/internal/stream"
	"github.com/williamtatendajose/prediction/iot-hub/internal/tsdb"
)

type Pipeline struct {
	Store    *store.Store
	Hub      *stream.Hub
	Detector *anomaly.Detector // optional
	Writer   *tsdb.Writer      // optional: persistence
	// OnEvent receives every anomaly open/close (e.g. MQTT publish). Set
	// before ingestion starts.
	OnEvent func(anomaly.Event)
	// Alarms marks episodes as shelved/acknowledged before they go out.
	Alarms *alarm.Manager
}

// Handle accepts either a JSON object of fields ({"temp":21.5,"door":"open"})
// or, when field is set, a bare value (21.5, true, "open"). A bare value with
// no field is stored as "value". "ts" or "timestamp" in an object sets the
// sample time (unix seconds, unix ms, or RFC 3339).
func (p *Pipeline) Handle(sensor, field string, payload []byte) (store.Reading, error) {
	payload = bytes.TrimSpace(payload)
	values, ts, err := Parse(field, payload)
	if err != nil {
		return store.Reading{}, err
	}
	return p.HandleValues(sensor, ts, values)
}

// HandleValues ingests already-decoded values (used by PLC connectors).
// ts <= 0 means now.
func (p *Pipeline) HandleValues(sensor string, ts int64, values map[string]any) (store.Reading, error) {
	r, err := p.Store.Ingest(sensor, ts, values)
	if err != nil {
		return r, err
	}
	if msg, err := json.Marshal(r); err == nil {
		p.Hub.Publish(&stream.Msg{Sensor: sensor, Data: msg})
	}
	if p.Writer != nil {
		for k, v := range r.Values {
			switch x := v.(type) {
			case float64:
				p.Writer.Add(tsdb.Point{Sensor: sensor, Field: k, TS: r.TS, Value: x})
			case bool:
				f := 0.0
				if x {
					f = 1
				}
				p.Writer.Add(tsdb.Point{Sensor: sensor, Field: k, TS: r.TS, Value: f})
			}
		}
		for _, k := range r.Changed {
			p.Writer.Add(tsdb.Point{Sensor: sensor, Field: k, TS: r.TS, Text: r.Values[k].(string), IsText: true})
		}
	}
	if p.Detector != nil {
		p.Emit(p.Detector.Seen(sensor, time.Now().UnixMilli()))
		for k, v := range r.Values {
			if x, ok := v.(float64); ok {
				p.Emit(p.Detector.Observe(sensor, k, r.TS, x, p.Store.Rule(sensor, k)))
			}
		}
	}
	return r, nil
}

// Emit distributes anomaly episodes to the live stream, the database and
// OnEvent.
func (p *Pipeline) Emit(evs []anomaly.Event) {
	if p.Alarms != nil {
		evs = p.Alarms.Decorate(evs)
	}
	for _, e := range evs {
		if msg, err := json.Marshal(e); err == nil {
			p.Hub.Publish(&stream.Msg{Event: "anomaly", Sensor: e.Sensor, Data: msg})
		}
		if p.Writer != nil {
			p.Writer.AddEvent(e)
		}
		if p.OnEvent != nil {
			p.OnEvent(e)
		}
	}
}

func Parse(field string, payload []byte) (map[string]any, int64, error) {
	if len(payload) == 0 {
		return nil, 0, fmt.Errorf("%w: empty payload", store.ErrInvalid)
	}
	if payload[0] == '{' {
		var m map[string]any
		if err := json.Unmarshal(payload, &m); err != nil {
			return nil, 0, fmt.Errorf("%w: %v", store.ErrInvalid, err)
		}
		var ts int64
		for _, k := range []string{"ts", "timestamp", "Timestamp"} {
			if v, ok := m[k]; ok {
				ts = parseTS(v)
				delete(m, k)
			}
		}
		if field != "" { // topic named a field: {"value":..} or first value
			if v, ok := m["value"]; ok {
				return map[string]any{field: v}, ts, nil
			}
		}
		return m, ts, nil
	}
	if field == "" {
		field = "value"
	}
	var v any
	if err := json.Unmarshal(payload, &v); err != nil {
		v = string(payload) // unquoted text such as: open
	}
	return map[string]any{field: v}, 0, nil
}

func parseTS(v any) int64 {
	var ms int64
	switch x := v.(type) {
	case float64:
		ms = int64(x)
		if x < 1e11 { // seconds
			ms = int64(x * 1000)
		}
	case string:
		if t, err := time.Parse(time.RFC3339Nano, x); err == nil {
			ms = t.UnixMilli()
		} else if n, err := strconv.ParseFloat(x, 64); err == nil {
			return parseTS(n)
		}
	}
	// Reject clocks that are badly off; the store substitutes "now".
	if ms > time.Now().Add(24*time.Hour).UnixMilli() {
		return 0
	}
	return ms
}
