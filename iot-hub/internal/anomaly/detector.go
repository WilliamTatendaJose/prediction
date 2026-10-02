// Package anomaly detects unusual sensor behaviour in a streaming fashion:
// constant memory and O(1) work per reading, no history scans.
//
// Three detectors:
//
//	range  value outside the field's configured low/high limits
//	spike  value more than Z standard deviations from an exponentially
//	       weighted moving mean (EWMA z-score)
//	stale  sensor silent for longer than max(StaleMin, StaleFactor x its
//	       usual reporting interval)
//
// Each anomaly is an episode: one event when it opens, the same event (same
// ID, End set) when it closes. Episodes, not samples, keep alert volume sane.
package anomaly

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"time"
)

type Event struct {
	ID      string  `json:"id"`
	Sensor  string  `json:"sensor"`
	Field   string  `json:"field,omitempty"`
	Kind    string  `json:"kind"`          // range | spike | stale
	Start   int64   `json:"start"`         // ms
	End     int64   `json:"end,omitempty"` // ms; 0 while open
	Value   float64 `json:"value"`         // value that opened the episode
	Score   float64 `json:"score,omitempty"`
	Message string  `json:"message"`

	// Alarm handling, filled in by package alarm (never by the detector).
	Ack     *Ack `json:"ack,omitempty"`
	Shelved bool `json:"shelved,omitempty"`
	Notes   int  `json:"notes,omitempty"`
}

// Verdicts recorded on acknowledgement. They double as training labels.
const (
	VerdictConfirmed  = "confirmed"   // a real problem
	VerdictFalseAlarm = "false_alarm" // nothing was wrong
	VerdictExpected   = "expected"    // known cause: maintenance, changeover, test
)

// Ack is an operator's acknowledgement of an episode.
type Ack struct {
	By      string `json:"by"`
	At      int64  `json:"at"`
	Verdict string `json:"verdict,omitempty"`
	Note    string `json:"note,omitempty"`
}

// Rule is the per-field configuration (store.Field.Detect).
type Rule struct {
	Low  *float64 `json:"low,omitempty"`
	High *float64 `json:"high,omitempty"`
	Z    float64  `json:"z,omitempty"` // 0 = default; <0 disables spike detection
	// Persist: consecutive outlying samples needed to open a spike (0 =
	// default). 2-3 suppresses heavy-tailed noise on fast sensors at the
	// cost of missing one-sample glitches.
	Persist int  `json:"persist,omitempty"`
	Off     bool `json:"off,omitempty"`
}

type Config struct {
	Z           float64       // spike threshold in standard deviations (default 5)
	Window      int           // EWMA span in samples (default 300)
	Persist     int           // consecutive outliers to open a spike (default 2)
	Warmup      int           // samples before spike detection starts (default 30)
	StaleMin    time.Duration // minimum silence before "stale"; 0 disables
	StaleFactor float64       // x typical interval (default 5)
	Recent      int           // closed episodes kept in memory (default 200)
}

type fieldState struct {
	mean, vr float64
	n        int
	over     int // consecutive samples beyond the threshold
	rng      *Event
	spike    *Event
}

type sensorState struct {
	last     int64   // arrival time, ms
	interval float64 // EWMA of arrival gaps, ms
	n        int
	stale    *Event
}

type Detector struct {
	cfg     Config
	alpha   float64
	mu      sync.Mutex
	fields  map[string]*fieldState // sensor + "\x00" + field
	sensors map[string]*sensorState
	recent  []Event // ring of closed episodes
	rhead   int
}

func New(cfg Config) *Detector {
	if cfg.Z <= 0 {
		cfg.Z = 5
	}
	if cfg.Window <= 0 {
		cfg.Window = 300
	}
	if cfg.Persist <= 0 {
		cfg.Persist = 2
	}
	if cfg.Warmup <= 0 {
		cfg.Warmup = 30
	}
	if cfg.StaleFactor <= 0 {
		cfg.StaleFactor = 5
	}
	if cfg.Recent <= 0 {
		cfg.Recent = 200
	}
	return &Detector{
		cfg:     cfg,
		alpha:   2 / float64(cfg.Window+1),
		fields:  map[string]*fieldState{},
		sensors: map[string]*sensorState{},
		recent:  make([]Event, 0, cfg.Recent),
	}
}

func open(sensor, field, kind string, ts int64, v, score float64, msg string) *Event {
	return &Event{
		ID:     fmt.Sprintf("%s.%s.%s.%d", sensor, field, kind, ts),
		Sensor: sensor, Field: field, Kind: kind, Start: ts, Value: v, Score: score, Message: msg,
	}
}

func (d *Detector) close(e *Event, ts int64) Event {
	e.End = ts
	if e.End < e.Start {
		e.End = e.Start
	}
	if len(d.recent) < cap(d.recent) {
		d.recent = append(d.recent, *e)
	} else {
		d.recent[d.rhead] = *e
		d.rhead = (d.rhead + 1) % cap(d.recent)
	}
	return *e
}

// Observe feeds one numeric value and returns opened/closed episodes.
func (d *Detector) Observe(sensor, field string, ts int64, v float64, r Rule) []Event {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	k := sensor + "\x00" + field
	s := d.fields[k]
	if s == nil {
		s = &fieldState{}
		d.fields[k] = s
	}
	var out []Event
	if r.Off {
		if s.rng != nil {
			out = append(out, d.close(s.rng, ts))
			s.rng = nil
		}
		if s.spike != nil {
			out = append(out, d.close(s.spike, ts))
			s.spike = nil
		}
		return out
	}

	// range
	outside := (r.Low != nil && v < *r.Low) || (r.High != nil && v > *r.High)
	switch {
	case outside && s.rng == nil:
		lim, side := r.Low, "below"
		if r.High != nil && v > *r.High {
			lim, side = r.High, "above"
		}
		s.rng = open(sensor, field, "range", ts, v, 0, fmt.Sprintf("%s %g is %s limit %g", field, v, side, *lim))
		out = append(out, *s.rng)
	case !outside && s.rng != nil:
		out = append(out, d.close(s.rng, ts))
		s.rng = nil
	}

	// spike (EWMA z-score)
	z := d.cfg.Z
	if r.Z > 0 {
		z = r.Z
	}
	x := v
	if s.n >= d.cfg.Warmup && r.Z >= 0 {
		// Floor the deviation so a perfectly flat signal does not turn
		// rounding noise into infinite z-scores.
		sd := math.Max(math.Sqrt(s.vr), 1e-6+1e-4*math.Abs(s.mean))
		score := (v - s.mean) / sd
		persist := d.cfg.Persist
		if r.Persist > 0 {
			persist = r.Persist
		}
		if math.Abs(score) > z {
			s.over++
		} else {
			s.over = 0
		}
		switch {
		case s.over >= persist && s.spike == nil:
			s.spike = open(sensor, field, "spike", ts, v, round2(score),
				fmt.Sprintf("%s %g is %.1fσ from recent mean %.4g", field, v, score, s.mean))
			out = append(out, *s.spike)
		case math.Abs(score) <= 0.8*z && s.spike != nil: // hysteresis avoids flapping
			out = append(out, d.close(s.spike, ts))
			s.spike = nil
		}
		// Winsorize the update so one wild value cannot drag the baseline,
		// while a genuine level shift is still absorbed within ~20 samples.
		if lim := z * sd; math.Abs(v-s.mean) > lim {
			x = s.mean + math.Copysign(lim, v-s.mean)
		}
	}
	a := math.Max(d.alpha, 1/float64(s.n+1)) // plain running mean during warm-up
	diff := x - s.mean
	inc := a * diff
	s.mean += inc
	s.vr = (1 - a) * (s.vr + diff*inc)
	s.n++
	return out
}

// Warm feeds restored history (oldest first) into a field's baseline
// without opening episodes, so spike detection works right after a restart
// instead of re-learning for Warmup samples. Outliers are clipped as in
// Observe, so a past spike does not inflate the baseline.
func (d *Detector) Warm(sensor, field string, vals []float64, r Rule) {
	d.mu.Lock()
	defer d.mu.Unlock()
	k := sensor + "\x00" + field
	s := d.fields[k]
	if s == nil {
		s = &fieldState{}
		d.fields[k] = s
	}
	z := d.cfg.Z
	if r.Z > 0 {
		z = r.Z
	}
	for _, v := range vals {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		x := v
		if s.n >= d.cfg.Warmup {
			sd := math.Max(math.Sqrt(s.vr), 1e-6+1e-4*math.Abs(s.mean))
			if lim := z * sd; math.Abs(v-s.mean) > lim {
				x = s.mean + math.Copysign(lim, v-s.mean)
			}
		}
		a := math.Max(d.alpha, 1/float64(s.n+1))
		diff := x - s.mean
		inc := a * diff
		s.mean += inc
		s.vr = (1 - a) * (s.vr + diff*inc)
		s.n++
	}
}

// Resume restores a sensor's reporting rhythm after a restart. It counts as
// seen at now (not at its last stored reading), so the time the hub itself
// was down never makes every sensor look stale at once.
func (d *Detector) Resume(sensor string, intervalMs float64, now int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := &sensorState{last: now}
	if intervalMs > 0 {
		st.interval, st.n = intervalMs, 3
	}
	d.sensors[sensor] = st
}

// Seen records that a sensor reported at arrival time now (ms). It closes a
// stale episode and learns the sensor's usual interval.
func (d *Detector) Seen(sensor string, now int64) []Event {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.sensors[sensor]
	if s == nil {
		d.sensors[sensor] = &sensorState{last: now}
		return nil
	}
	var out []Event
	if s.stale != nil {
		out = append(out, d.close(s.stale, now))
		s.stale = nil
	}
	gap := float64(now - s.last)
	if gap > 0 {
		if s.n == 0 {
			s.interval = gap
		} else {
			s.interval = 0.8*s.interval + 0.2*gap
		}
		s.n++
	}
	s.last = now
	return out
}

// CheckStale opens stale episodes for silent sensors; call periodically.
func (d *Detector) CheckStale(now int64) []Event {
	if d.cfg.StaleMin <= 0 {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []Event
	for id, s := range d.sensors {
		if s.stale != nil || s.n < 3 {
			continue
		}
		limit := math.Max(float64(d.cfg.StaleMin.Milliseconds()), d.cfg.StaleFactor*s.interval)
		if silent := float64(now - s.last); silent > limit {
			s.stale = open(id, "", "stale", now, 0, round2(silent/1000),
				fmt.Sprintf("no data for %s (usually every %s)", dur(silent), dur(s.interval)))
			out = append(out, *s.stale)
		}
	}
	return out
}

// Forget drops all state for a deleted sensor.
func (d *Detector) Forget(sensor string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.sensors, sensor)
	for k := range d.fields {
		if len(k) > len(sensor) && k[:len(sensor)] == sensor && k[len(sensor)] == 0 {
			delete(d.fields, k)
		}
	}
}

// Active returns open episodes, newest first.
func (d *Detector) Active() []Event {
	d.mu.Lock()
	var out []Event
	for _, s := range d.fields {
		if s.rng != nil {
			out = append(out, *s.rng)
		}
		if s.spike != nil {
			out = append(out, *s.spike)
		}
	}
	for _, s := range d.sensors {
		if s.stale != nil {
			out = append(out, *s.stale)
		}
	}
	d.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Start > out[j].Start })
	return out
}

// Recent returns open plus recently closed episodes, newest first.
func (d *Detector) Recent() []Event {
	out := d.Active()
	d.mu.Lock()
	out = append(out, d.recent...)
	d.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Start > out[j].Start })
	return out
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }

func dur(ms float64) string {
	d := time.Duration(ms) * time.Millisecond
	if d < 10*time.Second {
		return d.Round(100 * time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}

// Note is a journal entry on an episode ("replaced probe", "ack").
type Note struct {
	EventID string `json:"eventId"`
	TS      int64  `json:"ts"`
	By      string `json:"by"`
	Text    string `json:"text"`
}

// Shelf suppresses notifications and active status for matching episodes
// until Until. Empty Field/Kind match all.
type Shelf struct {
	Key     string `json:"key"`
	Sensor  string `json:"sensor"`
	Field   string `json:"field,omitempty"`
	Kind    string `json:"kind,omitempty"`
	Until   int64  `json:"until"`
	By      string `json:"by"`
	Reason  string `json:"reason"`
	Created int64  `json:"created"`
}

// AuditEntry records who changed what.
type AuditEntry struct {
	TS     int64  `json:"ts"`
	Actor  string `json:"actor"`
	Action string `json:"action"`
	Target string `json:"target"`
	Detail string `json:"detail,omitempty"`
}
