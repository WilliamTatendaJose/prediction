package jobs

import (
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/williamtatendajose/prediction/iot-hub/internal/store"
)

func TestCompile(t *testing.T) {
	p, err := Compile(Spec{ID: "tank-avg", Query: `SELECT avg(level) AS level_avg
		INTO [avg5-{sensor}] FROM [tank-*] WHERE value >= 0
		GROUP  BY sensor, TumblingWindow(minute, 5) HAVING AVG(level) > 80 and count >= 3`})
	if err != nil {
		t.Fatal(err)
	}
	if p.Agg != "avg" || p.Field != "level" || p.Alias != "level_avg" || p.Sensors != "tank-*" || !p.BySensor ||
		p.Window != (Window{"tumbling", 5 * time.Minute, 5 * time.Minute}) || p.Out != (Output{Kind: "sensor", Sensor: "avg5-{sensor}", Field: "level_avg"}) {
		t.Fatalf("%+v", p)
	}
	if p.OutputSensor("tank-1") != "avg5-tank-1" {
		t.Fatal(p.OutputSensor("tank-1"))
	}
	ok := []string{
		"SELECT increase(kwh) INTO [plant.energy_hourly] FROM [meter-*] GROUP BY HoppingWindow(hour, 2, 1)",
		"select count(door) into alert from [door-1] where value = 1 group by SlidingWindow(minute, 10) having count > 5",
		"SELECT max(temp) INTO webhook:ops-hook FROM oven-1 GROUP BY TumblingWindow(second, 30) HAVING value > 250",
	}
	for _, q := range ok {
		if _, err := Compile(Spec{ID: "ok", Query: q}); err != nil {
			t.Errorf("%s: %v", q, err)
		}
	}
	bad := map[string]string{
		"no select":         "INTO [x] FROM [y] GROUP BY TumblingWindow(minute, 1)",
		"two aggregates":    "SELECT avg(a), max(a) INTO [x] FROM [y] GROUP BY TumblingWindow(minute, 1)",
		"unknown aggregate": "SELECT median(a) INTO [x] FROM [y] GROUP BY TumblingWindow(minute, 1)",
		"no window":         "SELECT avg(a) INTO [x] FROM [y] GROUP BY sensor",
		"hop not divisor":   "SELECT avg(a) INTO [x] FROM [y] GROUP BY HoppingWindow(minute, 10, 3)",
		"too many hops":     "SELECT avg(a) INTO [x] FROM [y] GROUP BY HoppingWindow(second, 3600, 1)",
		"window too small":  "SELECT avg(a) INTO [x] FROM [y] GROUP BY TumblingWindow(second, 0)",
		"feeds itself":      "SELECT avg(a) INTO [tank-avg] FROM [tank-*] GROUP BY TumblingWindow(minute, 1)",
		"template feeds":    "SELECT avg(a) INTO [{sensor}-1m] FROM [tank-*] GROUP BY sensor, TumblingWindow(minute, 1)",
		"alert no having":   "SELECT avg(a) INTO alert FROM [y] GROUP BY TumblingWindow(minute, 1)",
		"template ungroup":  "SELECT avg(a) INTO [{sensor}-x] FROM [y-*] GROUP BY TumblingWindow(minute, 1)",
		"grouped one out":   "SELECT avg(a) INTO [x] FROM [y-*] GROUP BY sensor, TumblingWindow(minute, 1)",
		"bad placeholder":   "SELECT avg(a) INTO [{device}-x] FROM [y-*] GROUP BY sensor, TumblingWindow(minute, 1)",
		"where other field": "SELECT avg(a) INTO [x] FROM [y] WHERE b > 1 GROUP BY TumblingWindow(minute, 1)",
		"having unknown":    "SELECT avg(a) INTO [x] FROM [y] GROUP BY TumblingWindow(minute, 1) HAVING zz > 1",
		"sliding first":     "SELECT first(a) INTO [x] FROM [y] GROUP BY SlidingWindow(minute, 1)",
		"group by field":    "SELECT avg(a) INTO [x] FROM [y] GROUP BY line, TumblingWindow(minute, 1)",
		"unbalanced":        "SELECT avg(a INTO [x] FROM [y] GROUP BY TumblingWindow(minute, 1)",
	}
	for name, q := range bad {
		if _, err := Compile(Spec{ID: "bad", Query: q}); err == nil {
			t.Errorf("%s accepted: %s", name, q)
		}
	}
	if _, err := Compile(Spec{ID: "Bad_ID", Query: ok[0]}); err == nil {
		t.Error("bad id accepted")
	}
}

type capture struct {
	mu      sync.Mutex
	writes  []string
	vals    []float64
	raised  []string
	cleared []string
	hooks   []Row
}

func (c *capture) outputs() Outputs {
	return Outputs{
		Sensor: func(s string, ts int64, v map[string]any) error {
			c.mu.Lock()
			defer c.mu.Unlock()
			for k, x := range v {
				c.writes = append(c.writes, s+"."+k+"@"+time.UnixMilli(ts).UTC().Format("15:04:05"))
				c.vals = append(c.vals, x.(float64))
			}
			return nil
		},
		Raise: func(key, s, f string, ts int64, v float64, msg string) {
			c.mu.Lock()
			c.raised = append(c.raised, key+"@"+time.UnixMilli(ts).UTC().Format("15:04:05")+" "+msg)
			c.mu.Unlock()
		},
		Clear: func(key string, ts int64) {
			c.mu.Lock()
			c.cleared = append(c.cleared, key+"@"+time.UnixMilli(ts).UTC().Format("15:04:05"))
			c.mu.Unlock()
		},
		Webhook: func(target string, r Row) { c.mu.Lock(); c.hooks = append(c.hooks, r); c.mu.Unlock() },
	}
}

var t0 = time.Date(2026, 10, 2, 6, 0, 0, 0, time.UTC)

func rd(sensor string, sec int, field string, v float64) store.Reading {
	return store.Reading{Sensor: sensor, TS: t0.Add(time.Duration(sec) * time.Second).UnixMilli(), Values: map[string]any{field: v}}
}

func TestTumblingPerSensorWithLateness(t *testing.T) {
	c := &capture{}
	e := New(c.outputs())
	e.now = func() time.Time { return t0.Add(time.Hour) } // wall clock irrelevant: readings keep arriving
	e.Set([]Spec{{ID: "avg", Enabled: true, Lateness: "10s", Query: "SELECT avg(level) INTO [avg-{sensor}] FROM [tank-*] GROUP BY sensor, TumblingWindow(minute, 1)"}})
	// tank-1: 10, 20, 30 in minute 0; tank-2: 100 in minute 0.
	for _, r := range []store.Reading{rd("tank-1", 5, "level", 10), rd("tank-1", 25, "level", 20), rd("tank-2", 30, "level", 100),
		rd("tank-1", 55, "level", 30), rd("pump-1", 40, "level", 999), rd("tank-1", 65, "level", 50)} {
		e.Observe(r)
	}
	if len(c.writes) != 0 {
		t.Fatalf("closed before lateness passed: %v", c.writes)
	}
	e.Observe(rd("tank-1", 58, "level", 40)) // out of order but within lateness: counts
	e.Observe(rd("tank-2", 71, "level", 1))  // 71 - 10 s lateness passes minute 1's start: minute 0 closes
	want := map[string]float64{"avg-tank-1.avg_level@06:01:00": 25, "avg-tank-2.avg_level@06:01:00": 100}
	if len(c.writes) != 2 {
		t.Fatalf("writes %v", c.writes)
	}
	for i, w := range c.writes {
		if want[w] != c.vals[i] {
			t.Errorf("%s = %v, want %v", w, c.vals[i], want[w])
		}
	}
	e.Observe(rd("tank-1", 30, "level", 1000)) // minute 0 already closed: late, dropped
	st := e.Status()[0]
	if st.Late != 1 || st.In != 8 || st.Out != 2 || st.OpenWindows != 2 { // pump-1 is not an input
		t.Fatalf("status %+v", st)
	}
}

func TestHoppingIncreaseAndIdleClose(t *testing.T) {
	c := &capture{}
	e := New(c.outputs())
	now := t0
	e.now = func() time.Time { return now }
	e.Set([]Spec{{ID: "energy", Enabled: true, Query: "SELECT increase(kwh) INTO [plant.kwh_10m] FROM [meter] GROUP BY HoppingWindow(minute, 10, 5)"}})
	// A meter total: 100 at 0:00, rising 1/min, reset to 0 at 7:30 (power cut), then rising again.
	vals := map[int]float64{0: 100, 120: 102, 240: 104, 360: 106, 450: 0.5, 540: 2, 660: 4}
	for _, sec := range []int{0, 120, 240, 360, 450, 540, 660} {
		now = t0.Add(time.Duration(sec) * time.Second)
		e.Observe(rd("meter", sec, "kwh", vals[sec]))
	}
	// Readings stop. Two seconds later and past lateness, the wall clock
	// closes what is due: windows ending 06:05 and 06:10.
	now = t0.Add(10*time.Minute + 6*time.Second)
	e.Tick()
	// [05:55,06:05): 100 baseline, +2 +2 → 4. [06:00,06:10): 4 + 2 (106) + 0.5 (reset) + 1.5 → 8.
	if len(c.writes) != 2 || c.vals[0] != 4 || c.vals[1] != 8 {
		t.Fatalf("writes %v %v", c.writes, c.vals)
	}
	now = t0.Add(20 * time.Minute)
	e.Tick() // [06:05,06:15): +2 (104→106 at 06:06), +0.5 (reset), +1.5, +2 → 6
	if len(c.vals) != 3 || c.vals[2] != 6 {
		t.Fatalf("third window %v", c.vals)
	}
}

func TestAggregates(t *testing.T) {
	a := &agg{}
	for i, v := range []float64{4, 8, 6, 2} {
		a.add(int64(i), v, 0)
	}
	for kind, want := range map[string]float64{"avg": 5, "min": 2, "max": 8, "sum": 20, "count": 4, "first": 4, "last": 2, "delta": -2,
		"stddev": math.Sqrt(20.0 / 3)} {
		if got := a.value(kind); math.Abs(got-want) > 1e-12 {
			t.Errorf("%s = %v, want %v", kind, got, want)
		}
	}
}

func TestSlidingAlertRaisesAndClears(t *testing.T) {
	c := &capture{}
	e := New(c.outputs())
	now := t0
	e.now = func() time.Time { return now }
	e.Set([]Spec{{ID: "door", Enabled: true, Lateness: "0s",
		Query: "SELECT count(open) INTO alert FROM [door-1] WHERE value = 1 GROUP BY SlidingWindow(minute, 10) HAVING count > 3"}})
	for i, sec := range []int{0, 60, 120, 130, 180, 200} {
		now = t0.Add(time.Duration(sec) * time.Second)
		v := 1.0
		if i == 3 {
			v = 0 // a close: filtered out by WHERE
		}
		e.Observe(rd("door-1", sec, "open", v))
	}
	// Opens: 0, 60, 120, 180 → the fourth makes count 4 > 3 at 06:03:00.
	if len(c.raised) != 1 || !strings.HasPrefix(c.raised[0], "rule.door.*@06:03:00 door: count(open) = 4 over 10m0s") {
		t.Fatalf("raised %v", c.raised)
	}
	// No more openings: when the first ones slide out, it clears by itself.
	now = t0.Add(11 * time.Minute)
	e.Tick()
	if len(c.cleared) != 1 {
		t.Fatalf("cleared %v", c.cleared)
	}
	if st := e.Status()[0]; st.Filtered != 1 || st.OpenAlerts != 0 {
		t.Fatalf("status %+v", st)
	}
	// Removing a job clears its open alerts.
	now = t0.Add(12 * time.Minute)
	for _, s := range []int{720, 721, 722, 723} {
		e.Observe(rd("door-1", s, "open", 1))
	}
	if len(c.raised) != 2 {
		t.Fatalf("raised again %v", c.raised)
	}
	e.Set(nil)
	if len(c.cleared) != 2 {
		t.Fatalf("job removed: %v", c.cleared)
	}
}

func TestLoopGuardAndWebhook(t *testing.T) {
	c := &capture{}
	e := New(c.outputs())
	e.now = func() time.Time { return t0.Add(time.Hour) }
	// Job a writes plant-a; job b reads plant-*: a's output would feed b.
	e.Set([]Spec{
		{ID: "a", Enabled: true, Lateness: "0s", Query: "SELECT max(t) INTO [plant-a] FROM [oven-1] GROUP BY TumblingWindow(second, 10)"},
		{ID: "b", Enabled: true, Lateness: "0s", Query: "SELECT max(t) INTO webhook:ops FROM [plant-*] GROUP BY TumblingWindow(second, 10) HAVING value > 100"},
		{ID: "c", Enabled: true, Lateness: "0s", Query: "SELECT max(t) INTO webhook:ops FROM [oven-1] GROUP BY TumblingWindow(second, 10) HAVING value > 100"},
	})
	e.Observe(rd("oven-1", 1, "t", 250))
	e.Observe(rd("oven-1", 11, "t", 90))
	st := e.Status()
	if len(c.writes) != 0 || st[0].Errors != 1 || !strings.Contains(st[0].LastError, "would loop") {
		t.Fatalf("loop guard: writes %v status %+v", c.writes, st[0])
	}
	if len(c.hooks) != 1 || c.hooks[0].Value != 250 || c.hooks[0].Job != "c" {
		t.Fatalf("webhook rows %v", c.hooks)
	}
	// A job with a bad query is listed with its error and does nothing.
	e.Set([]Spec{{ID: "x", Enabled: true, Query: "SELECT nonsense"}})
	if st := e.Status(); st[0].Error == "" {
		t.Fatal("compile error not reported")
	}
}

func TestSliderMatchesNaive(t *testing.T) {
	rnd := uint64(42)
	next := func() uint64 { rnd ^= rnd << 13; rnd ^= rnd >> 7; rnd ^= rnd << 17; return rnd }
	s := &slider{}
	var ts []int64
	var vs []float64
	now := int64(0)
	for step := 0; step < 20000; step++ {
		now += int64(next()%5) + 1
		v := float64(int64(next()%2000) - 1000)
		s.push(now, v)
		ts, vs = append(ts, now), append(vs, v)
		before := now - int64(next()%300)
		s.evict(before)
		for len(ts) > 0 && ts[0] <= before {
			ts, vs = ts[1:], vs[1:]
		}
		a := s.agg()
		n := &agg{}
		for i := range ts {
			n.add(ts[i], vs[i], 0)
		}
		if a.n != n.n || a.n > 0 && (a.min != n.min || a.max != n.max || math.Abs(a.sum-n.sum) > 1e-6 || math.Abs(a.value("stddev")-n.value("stddev")) > 1e-6) {
			t.Fatalf("step %d: incremental %+v naive %+v", step, a, n)
		}
	}
}
