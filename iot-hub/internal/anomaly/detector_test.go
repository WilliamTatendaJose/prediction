package anomaly

import (
	"math/rand/v2"
	"testing"
	"time"
)

func f(v float64) *float64 { return &v }

func TestSpikeOpensAndCloses(t *testing.T) {
	d := New(Config{Persist: 1})
	r := rand.New(rand.NewPCG(1, 2))
	var evs []Event
	for i := 0; i < 500; i++ {
		v := 20 + r.NormFloat64()*0.5
		if i == 300 {
			v = 40 // 40σ spike
		}
		evs = append(evs, d.Observe("s", "temp", int64(i), v, Rule{})...)
	}
	opens := 0
	for _, e := range evs {
		if e.Kind == "spike" && e.End == 0 {
			opens++
			if e.Start != 300 {
				t.Errorf("unexpected spike at %d (score %.1f)", e.Start, e.Score)
			}
		}
	}
	if opens != 1 {
		t.Fatalf("want 1 spike episode, got %d: %+v", opens, evs)
	}
	if len(d.Active()) != 0 {
		t.Fatal("spike should have closed")
	}
}

// Gaussian noise at z=4 should rarely alert: p(|z|>4) ~ 6e-5 per sample.
func TestFalsePositiveRate(t *testing.T) {
	d := New(Config{})
	r := rand.New(rand.NewPCG(3, 4))
	n := 0
	for i := 0; i < 20000; i++ {
		for _, e := range d.Observe("s", "x", int64(i), 100+r.NormFloat64()*3, Rule{}) {
			if e.End == 0 {
				n++
			}
		}
	}
	if n > 10 {
		t.Fatalf("%d false spike episodes in 20000 clean samples", n)
	}
}

func TestBaselineNotPoisoned(t *testing.T) {
	d := New(Config{Persist: 1})
	r := rand.New(rand.NewPCG(5, 6))
	for i := 0; i < 200; i++ {
		d.Observe("s", "x", int64(i), r.NormFloat64(), Rule{})
	}
	d.Observe("s", "x", 200, 1e6, Rule{}) // absurd glitch
	k := d.fields["s\x00x"]
	if k.mean > 1 || k.vr > 10 {
		t.Fatalf("glitch dragged baseline: mean=%g var=%g", k.mean, k.vr)
	}
}

func TestLevelShiftIsAbsorbed(t *testing.T) {
	d := New(Config{})
	r := rand.New(rand.NewPCG(7, 8))
	for i := 0; i < 200; i++ {
		d.Observe("s", "x", int64(i), 10+r.NormFloat64(), Rule{})
	}
	closedAt := -1
	for i := 200; i < 400; i++ {
		for _, e := range d.Observe("s", "x", int64(i), 30+r.NormFloat64(), Rule{}) {
			if e.End != 0 {
				closedAt = i
			}
		}
		if closedAt >= 0 {
			break
		}
	}
	if closedAt < 0 || closedAt > 260 {
		t.Fatalf("level shift not absorbed quickly: closed at %d", closedAt)
	}
}

func TestRangeAndOff(t *testing.T) {
	d := New(Config{})
	rule := Rule{Low: f(0), High: f(100), Z: -1}
	if evs := d.Observe("s", "lvl", 1, 50, rule); len(evs) != 0 {
		t.Fatal(evs)
	}
	evs := d.Observe("s", "lvl", 2, 120, rule)
	if len(evs) != 1 || evs[0].Kind != "range" || evs[0].End != 0 {
		t.Fatalf("open: %+v", evs)
	}
	if evs := d.Observe("s", "lvl", 3, 130, rule); len(evs) != 0 {
		t.Fatal("still open: no new event expected")
	}
	evs = d.Observe("s", "lvl", 4, 90, rule)
	if len(evs) != 1 || evs[0].End != 4 {
		t.Fatalf("close: %+v", evs)
	}
	d.Observe("s", "lvl", 5, -5, rule)
	if evs := d.Observe("s", "lvl", 6, -5, Rule{Off: true}); len(evs) != 1 || evs[0].End == 0 {
		t.Fatalf("off must close open episodes: %+v", evs)
	}
}

func TestStale(t *testing.T) {
	d := New(Config{StaleMin: time.Second})
	for i := int64(0); i < 5; i++ {
		d.Seen("s", i*1000) // every second
	}
	if evs := d.CheckStale(6000); len(evs) != 0 {
		t.Fatal("2s silence < 5x interval")
	}
	evs := d.CheckStale(10000)
	if len(evs) != 1 || evs[0].Kind != "stale" {
		t.Fatalf("want stale, got %+v", evs)
	}
	if evs := d.CheckStale(20000); len(evs) != 0 {
		t.Fatal("stale must not repeat")
	}
	evs = d.Seen("s", 21000)
	if len(evs) != 1 || evs[0].End != 21000 {
		t.Fatalf("data must close stale: %+v", evs)
	}
}

func TestPersistIgnoresSingleGlitch(t *testing.T) {
	d := New(Config{}) // default persist = 2
	r := rand.New(rand.NewPCG(11, 12))
	for i := 0; i < 100; i++ {
		d.Observe("s", "x", int64(i), r.NormFloat64(), Rule{})
	}
	if evs := d.Observe("s", "x", 100, 50, Rule{}); len(evs) != 0 {
		t.Fatal("single outlier must not open with persist=2")
	}
	d.Observe("s", "x", 101, 0, Rule{})
	d.Observe("s", "x", 102, 50, Rule{})
	if evs := d.Observe("s", "x", 103, 50, Rule{}); len(evs) != 1 {
		t.Fatalf("two consecutive outliers must open: %+v", evs)
	}
	if evs := d.Observe("s", "x", 104, 50, Rule{Persist: 1}); len(evs) != 0 {
		t.Fatal("already open")
	}
}

func TestWarmAndResume(t *testing.T) {
	d := New(Config{StaleMin: time.Second})
	r := rand.New(rand.NewPCG(21, 22))
	hist := make([]float64, 200)
	for i := range hist {
		hist[i] = 20 + r.NormFloat64()*0.5
	}
	hist[100] = 500 // a past glitch must not inflate the baseline
	d.Warm("s", "t", hist, Rule{})
	// Spike detection works on the first live samples after restart.
	if evs := d.Observe("s", "t", 1, 40, Rule{}); len(evs) != 0 {
		t.Fatal("persist=2: first outlier alone must not open")
	}
	if evs := d.Observe("s", "t", 2, 40, Rule{}); len(evs) != 1 || evs[0].Kind != "spike" {
		t.Fatalf("warm detector should catch a spike immediately: %+v", evs)
	}
	// Resume: the restart moment counts as seen, so no instant stale storm.
	d.Resume("s", 1000, 100_000)
	if evs := d.CheckStale(101_000); len(evs) != 0 {
		t.Fatal("stale right after restart")
	}
	if evs := d.CheckStale(110_000); len(evs) != 1 {
		t.Fatal("still detects a sensor that stays silent after restart")
	}
}
