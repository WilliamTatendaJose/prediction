package jobs

import (
	"context"
	"fmt"
	"math"
	"path"
	"sort"
	"sync"
	"time"

	"github.com/williamtatendajose/prediction/iot-hub/internal/store"
)

// Row is one window result.
type Row struct {
	Job   string  `json:"job"`
	Group string  `json:"group,omitempty"`
	Start int64   `json:"windowStart"`
	End   int64   `json:"windowEnd"`
	Field string  `json:"field"`
	Value float64 `json:"value"`
	Count int64   `json:"count"`
	Pass  bool    `json:"-"` // HAVING held
}

// Outputs are where results go (implemented by the tenant runtime).
type Outputs struct {
	Sensor  func(sensor string, ts int64, values map[string]any) error
	Raise   func(key, sensor, field string, ts int64, v float64, msg string)
	Clear   func(key string, ts int64)
	Webhook func(target string, r Row)
}

// Limits bound memory per job.
const (
	MaxOpenWindows = 20000 // open (group, window) aggregates
	MaxSliding     = 10000 // readings kept per group in a sliding window
)

type agg struct {
	n                    int64
	sum, sumsq, min, max float64
	firstTS, lastTS      int64
	first, last, inc     float64
}

func (a *agg) add(ts int64, v, rise float64) {
	if a.n == 0 {
		a.min, a.max, a.firstTS, a.first, a.lastTS, a.last = v, v, ts, v, ts, v
	}
	a.n++
	a.sum += v
	a.sumsq += v * v
	a.min, a.max = math.Min(a.min, v), math.Max(a.max, v)
	if ts < a.firstTS {
		a.firstTS, a.first = ts, v
	}
	if ts >= a.lastTS {
		a.lastTS, a.last = ts, v
	}
	a.inc += rise
}

func (a *agg) value(kind string) float64 {
	switch kind {
	case "avg":
		return a.sum / float64(a.n)
	case "min":
		return a.min
	case "max":
		return a.max
	case "sum":
		return a.sum
	case "count":
		return float64(a.n)
	case "stddev":
		if a.n < 2 {
			return 0
		}
		return math.Sqrt(math.Max(0, (a.sumsq-a.sum*a.sum/float64(a.n))/float64(a.n-1)))
	case "first":
		return a.first
	case "last":
		return a.last
	case "delta":
		return a.last - a.first
	case "increase":
		return a.inc
	}
	return math.NaN()
}

// slider keeps one group's readings inside a sliding window, with running
// sums and monotonic queues so each reading costs O(1) amortised.
type slider struct {
	ts         []int64
	v          []float64
	n          int64
	sum, sumsq float64
	minq, maxq []int // indexes into ts/v (absolute, minus off)
	off        int   // readings removed from the front so far
	evicted    int
}

func (s *slider) push(ts int64, v float64) {
	s.ts, s.v = append(s.ts, ts), append(s.v, v)
	s.n++
	s.sum += v
	s.sumsq += v * v
	i := s.off + len(s.v) - 1
	for len(s.minq) > 0 && s.at(s.minq[len(s.minq)-1]) >= v {
		s.minq = s.minq[:len(s.minq)-1]
	}
	s.minq = append(s.minq, i)
	for len(s.maxq) > 0 && s.at(s.maxq[len(s.maxq)-1]) <= v {
		s.maxq = s.maxq[:len(s.maxq)-1]
	}
	s.maxq = append(s.maxq, i)
}

func (s *slider) at(i int) float64 { return s.v[i-s.off] }

func (s *slider) popFront() {
	v := s.v[0]
	s.ts, s.v = s.ts[1:], s.v[1:]
	s.n--
	s.sum -= v
	s.sumsq -= v * v
	if len(s.minq) > 0 && s.minq[0] == s.off {
		s.minq = s.minq[1:]
	}
	if len(s.maxq) > 0 && s.maxq[0] == s.off {
		s.maxq = s.maxq[1:]
	}
	s.off++
	if s.evicted++; s.evicted%4096 == 0 { // bound floating-point drift of the running sums
		s.sum, s.sumsq = 0, 0
		for _, x := range s.v {
			s.sum += x
			s.sumsq += x * x
		}
	}
}

func (s *slider) evict(before int64) {
	for len(s.ts) > 0 && s.ts[0] <= before {
		s.popFront()
	}
}

func (s *slider) agg() *agg {
	a := &agg{n: s.n, sum: s.sum, sumsq: s.sumsq}
	if s.n > 0 {
		a.min, a.max = s.at(s.minq[0]), s.at(s.maxq[0])
	}
	return a
}

// Status is a job's health, for the API.
type Status struct {
	Spec
	Error       string `json:"error,omitempty"`
	In          uint64 `json:"in"`
	Filtered    uint64 `json:"filtered"`
	Out         uint64 `json:"out"`
	Late        uint64 `json:"late"`
	Dropped     uint64 `json:"dropped"`
	Errors      uint64 `json:"errors"`
	LastError   string `json:"lastError,omitempty"`
	LastOutput  *Row   `json:"lastOutput,omitempty"`
	OpenWindows int    `json:"openWindows"`
	OpenAlerts  int    `json:"openAlerts"`
}

type job struct {
	spec Spec
	plan *Plan
	err  error

	mu          sync.Mutex
	wins        map[string]map[int64]*agg // group -> window start -> aggregate
	open        int
	nextEnd     int64 // earliest end among open windows (MaxInt64: none)
	closed      int64 // every window ending at or before this is closed
	watermark   int64
	lastArrival time.Time
	prev        map[string]float64 // last value per group (for increase)
	slide       map[string]*slider
	alerts      map[string]bool
	st          Status
}

func newJob(sp Spec) *job {
	j := &job{spec: sp, wins: map[string]map[int64]*agg{}, prev: map[string]float64{},
		slide: map[string]*slider{}, alerts: map[string]bool{}, closed: math.MinInt64, nextEnd: math.MaxInt64}
	j.plan, j.err = Compile(sp)
	return j
}

// Engine runs a tenant's jobs.
type Engine struct {
	out Outputs
	now func() time.Time

	mu     sync.RWMutex
	jobs   []*job
	inputs []string // FROM globs of enabled jobs (loop guard)
}

func New(out Outputs) *Engine { return &Engine{out: out, now: time.Now} }

// Set replaces the job list. A job whose spec is unchanged keeps its open
// windows; a removed or changed job's open alerts are cleared.
func (e *Engine) Set(specs []Spec) {
	e.mu.Lock()
	old := map[string]*job{}
	for _, j := range e.jobs {
		old[j.spec.ID] = j
	}
	var next []*job
	var inputs []string
	for _, sp := range specs {
		j := old[sp.ID]
		if j == nil || j.spec != sp {
			j = newJob(sp)
		} else {
			delete(old, sp.ID)
		}
		next = append(next, j)
		if j.plan != nil && sp.Enabled {
			inputs = append(inputs, j.plan.Sensors)
		}
	}
	e.jobs, e.inputs = next, inputs
	e.mu.Unlock()
	now := e.now().UnixMilli()
	for _, j := range old {
		j.mu.Lock()
		keys := make([]string, 0, len(j.alerts))
		for g := range j.alerts {
			keys = append(keys, alertKey(j.spec.ID, g))
		}
		j.mu.Unlock()
		for _, k := range keys {
			if e.out.Clear != nil {
				e.out.Clear(k, now)
			}
		}
	}
}

func alertKey(job, group string) string { return "rule." + job + "." + group }

// feeds reports whether writing to sensor would feed any enabled job.
func (e *Engine) feeds(sensor string) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	for _, g := range e.inputs {
		if ok, _ := path.Match(g, sensor); ok {
			return true
		}
	}
	return false
}

type emit struct {
	j   *job
	row Row
}

// Observe feeds one stored reading to every job.
func (e *Engine) Observe(r store.Reading) {
	e.mu.RLock()
	jobs := e.jobs
	e.mu.RUnlock()
	var out []emit
	for _, j := range jobs {
		if j.plan == nil || !j.spec.Enabled {
			continue
		}
		if ok, _ := path.Match(j.plan.Sensors, r.Sensor); !ok {
			continue
		}
		var v float64
		switch x := r.Values[j.plan.Field].(type) {
		case float64:
			v = x
		case bool:
			v = map[bool]float64{true: 1}[x]
		default:
			continue
		}
		out = append(out, j.observe(r.Sensor, r.TS, v, e.now())...)
	}
	e.emit(out)
}

func (j *job) observe(sensor string, ts int64, v float64, now time.Time) []emit {
	p := j.plan
	j.mu.Lock()
	defer j.mu.Unlock()
	j.st.In++
	j.lastArrival = now
	if p.Where != nil {
		x, err := p.Where.Eval(func(n string) (float64, bool) { return v, true })
		if err != nil || x == 0 {
			j.st.Filtered++
			return nil
		}
	}
	group := "*"
	if p.BySensor {
		group = sensor
	}
	if p.Window.Kind == "sliding" {
		s := j.slide[group]
		if s == nil {
			s = &slider{}
			j.slide[group] = s
		}
		s.push(ts, v)
		if len(s.ts) > MaxSliding {
			s.popFront()
			j.st.Dropped++
		}
		s.evict(ts - p.Window.Size.Milliseconds())
		a := s.agg()
		return []emit{{j, j.row(group, ts-p.Window.Size.Milliseconds(), ts, a)}}
	}
	rise := 0.0
	if prev, ok := j.prev[group]; ok {
		if v >= prev {
			rise = v - prev
		} else {
			rise = v // counter reset: it counted up from zero
		}
	}
	j.prev[group] = v
	size, hop := p.Window.Size.Milliseconds(), p.Window.Hop.Milliseconds()
	late := false
	for k := floorDiv(ts-size, hop) + 1; k <= floorDiv(ts, hop); k++ {
		start := k * hop
		if start+size <= j.closed {
			late = true
			continue
		}
		g := j.wins[group]
		if g == nil {
			g = map[int64]*agg{}
			j.wins[group] = g
		}
		a := g[start]
		if a == nil {
			if j.open >= MaxOpenWindows {
				j.st.Dropped++
				continue
			}
			a = &agg{}
			g[start] = a
			j.open++
			if end := start + size; end < j.nextEnd {
				j.nextEnd = end
			}
		}
		a.add(ts, v, rise)
	}
	if late {
		j.st.Late++
	}
	if wm := ts - p.Lateness.Milliseconds(); wm > j.watermark {
		j.watermark = wm
	}
	if j.watermark >= j.nextEnd { // only scan when a window is actually due
		return j.closeUpTo(j.watermark)
	}
	if j.watermark > j.closed {
		j.closed = j.watermark
	}
	return nil
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

// closeUpTo emits every window ending at or before limit. Caller holds j.mu.
func (j *job) closeUpTo(limit int64) []emit {
	size := j.plan.Window.Size.Milliseconds()
	var out []emit
	j.nextEnd = math.MaxInt64
	for group, g := range j.wins {
		for start, a := range g {
			if start+size <= limit {
				out = append(out, emit{j, j.row(group, start, start+size, a)})
				delete(g, start)
				j.open--
			} else if start+size < j.nextEnd {
				j.nextEnd = start + size
			}
		}
		if len(g) == 0 {
			delete(j.wins, group)
		}
	}
	if limit > j.closed {
		j.closed = limit
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].row.End != out[b].row.End {
			return out[a].row.End < out[b].row.End
		}
		return out[a].row.Group < out[b].row.Group
	})
	return out
}

func (j *job) row(group string, start, end int64, a *agg) Row {
	p := j.plan
	r := Row{Job: j.spec.ID, Group: group, Start: start, End: end, Field: p.Out.Field, Value: a.value(p.Agg), Count: a.n, Pass: true}
	if r.Field == "" {
		r.Field = p.Alias
	}
	if a.n == 0 {
		r.Value = 0
		if p.Agg != "count" && p.Agg != "sum" && p.Agg != "increase" {
			r.Pass = false // nothing to report for an empty sliding window
			return r
		}
	}
	if p.Having != nil {
		x, err := p.Having.Eval(func(n string) (float64, bool) {
			if n == "count" {
				return float64(a.n), true
			}
			return r.Value, true // value or the alias
		})
		r.Pass = err == nil && x != 0
	}
	return r
}

// emit performs outputs, outside every lock.
func (e *Engine) emit(out []emit) {
	for _, o := range out {
		j, r, p := o.j, o.row, o.j.plan
		var err error
		switch p.Out.Kind {
		case "sensor":
			if !r.Pass || math.IsNaN(r.Value) || math.IsInf(r.Value, 0) {
				continue
			}
			sensor := p.OutputSensor(r.Group)
			switch {
			case e.feeds(sensor):
				err = fmt.Errorf("output %s is an input of a job: not written (it would loop)", sensor)
			case e.out.Sensor != nil:
				err = e.out.Sensor(sensor, r.End, map[string]any{r.Field: r.Value})
			}
		case "alert":
			key := alertKey(j.spec.ID, r.Group)
			j.mu.Lock()
			was := j.alerts[r.Group]
			if r.Pass {
				j.alerts[r.Group] = true
			} else {
				delete(j.alerts, r.Group)
			}
			j.mu.Unlock()
			switch {
			case r.Pass && !was && e.out.Raise != nil:
				sensor := r.Group
				if sensor == "*" {
					sensor = p.Sensors
				}
				e.out.Raise(key, sensor, r.Field, r.End, r.Value,
					fmt.Sprintf("%s: %s(%s) = %.4g over %s (%d readings)", j.spec.ID, p.Agg, p.Field, r.Value, p.Window.Size, r.Count))
			case !r.Pass && was && e.out.Clear != nil:
				e.out.Clear(key, r.End)
			}
			if !r.Pass && !was {
				continue
			}
		case "webhook":
			if !r.Pass {
				continue
			}
			if e.out.Webhook != nil {
				e.out.Webhook(p.Out.Target, r)
			}
		}
		j.mu.Lock()
		if err != nil {
			j.st.Errors++
			j.st.LastError = err.Error()
		} else {
			j.st.Out++
			rc := r
			j.st.LastOutput = &rc
		}
		j.mu.Unlock()
	}
}

// Tick closes windows whose readings have stopped coming, by wall clock,
// and re-evaluates sliding alerts (so "more than 5 in 10 min" clears when
// readings stop).
func (e *Engine) Tick() {
	now := e.now()
	e.mu.RLock()
	jobs := e.jobs
	e.mu.RUnlock()
	var out []emit
	for _, j := range jobs {
		if j.plan == nil || !j.spec.Enabled {
			continue
		}
		j.mu.Lock()
		p := j.plan
		if p.Window.Kind == "sliding" {
			if p.Out.Kind == "alert" {
				ts := now.UnixMilli() - p.Lateness.Milliseconds()
				for g, s := range j.slide {
					s.evict(ts - p.Window.Size.Milliseconds())
					if j.alerts[g] {
						out = append(out, emit{j, j.row(g, ts-p.Window.Size.Milliseconds(), ts, s.agg())})
					}
				}
			}
		} else if now.Sub(j.lastArrival) >= 2*time.Second {
			if limit := now.UnixMilli() - p.Lateness.Milliseconds(); limit > j.closed {
				out = append(out, j.closeUpTo(limit)...)
			}
		}
		j.mu.Unlock()
	}
	e.emit(out)
}

// Flush closes every open window now (end of a test replay).
func (e *Engine) Flush() {
	e.mu.RLock()
	jobs := e.jobs
	e.mu.RUnlock()
	var out []emit
	for _, j := range jobs {
		if j.plan == nil {
			continue
		}
		j.mu.Lock()
		out = append(out, j.closeUpTo(math.MaxInt64)...)
		j.mu.Unlock()
	}
	e.emit(out)
}

// Run ticks every second until ctx ends.
func (e *Engine) Run(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.Tick()
		}
	}
}

// Status lists the jobs with their counters.
func (e *Engine) Status() []Status {
	e.mu.RLock()
	jobs := e.jobs
	e.mu.RUnlock()
	out := make([]Status, 0, len(jobs))
	for _, j := range jobs {
		j.mu.Lock()
		st := j.st
		st.Spec = j.spec
		if j.err != nil {
			st.Error = j.err.Error()
		}
		st.OpenWindows, st.OpenAlerts = j.open, len(j.alerts)
		j.mu.Unlock()
		out = append(out, st)
	}
	return out
}
