package oee

import (
	"math"
	"strings"
	"testing"
	"time"
)

const sec = int64(1000)

// shift builds an 8 h shift with known OEE:
//
//	planned break 4:00-4:30          -> planned time 7.5 h
//	unplanned stop 2:00-3:30          -> run time 6 h      -> A = 0.8
//	648 parts, ideal cycle 30 s       -> 6 h ideal = 720   -> P = 0.9
//	32 rejects                        -> 616 good          -> Q = 0.950617
//	OEE = 0.8 * 0.9 * 0.950617 = 0.684444
//
// The total counter is reset at 5:00 (PLC restart) and the window starts
// mid-count (baseline before the window).
func shift() (Config, int64, int64, Series, Series, Series, Series) {
	from, to := int64(1_000_000)*sec, int64(1_000_000+8*3600)*sec
	at := func(h float64) int64 { return from + int64(h*3600)*sec }
	run, planned, total, reject := Series{}, Series{}, Series{}, Series{}
	running := func(t int64) bool {
		h := float64(t-from) / 3600e3
		return !(h >= 2 && h < 3.5) && !(h >= 4 && h < 4.5)
	}
	for t := from - 30*sec; t < to; t += 10 * sec { // 10 s state samples, one before the window
		run.TS = append(run.TS, t)
		run.V = append(run.V, b(running(t)))
	}
	for t := from - 60*sec; t < to; t += 60 * sec {
		h := float64(t-from) / 3600e3
		planned.TS = append(planned.TS, t)
		planned.V = append(planned.V, b(h >= 4 && h < 4.5))
	}
	// 648 parts spread evenly over the 6 running hours: one every 33.33 s.
	runningSecs := []struct{ a, b float64 }{{0, 2}, {3.5, 4}, {4.5, 8}}
	count, base, made := 5000.0, 5000.0, 0
	total.TS, total.V = []int64{from - 5*sec}, []float64{base}
	for _, seg := range runningSecs {
		for t := at(seg.a); t < at(seg.b) && made < 648; t += 33_333 {
			made++
			if t >= at(5) && count > 1000 { // reset to 0 at the first count after 5:00
				count = 0
			}
			count++
			total.TS = append(total.TS, t)
			total.V = append(total.V, count)
		}
	}
	rj := 0.0
	reject.TS, reject.V = []int64{from - 5*sec}, []float64{77} // baseline from the previous shift
	for i := 1; i <= 32; i++ {
		rj++
		reject.TS = append(reject.TS, from+int64(i)*600*sec)
		reject.V = append(reject.V, 77+rj)
	}
	return Config{Running: "run", Total: "total", Reject: "rej", PlannedStop: "brk", IdealCycle: 30}, from, to, run, total, reject, planned
}

func b(x bool) float64 {
	if x {
		return 1
	}
	return 0
}

func near(a *float64, want float64) bool { return a != nil && math.Abs(*a-want) < 1e-3 }

func TestKnownShift(t *testing.T) {
	c, from, to, run, total, reject, planned := shift()
	r := Compute(c, from, to, run, total, Series{}, reject, planned)
	if !near(r.Availability, 0.8) || !near(r.Performance, 0.9) || !near(r.Quality, 616.0/648) || !near(r.OEE, 0.8*0.9*616/648) {
		t.Fatalf("A %v P %v Q %v OEE %v (total %v reject %v run %v planned %v)",
			deref(r.Availability), deref(r.Performance), deref(r.Quality), deref(r.OEE), r.Total, r.Reject, r.RunSec, r.PlannedSec)
	}
	if r.Total != 648 || r.Reject != 32 || r.PlannedSec != 7.5*3600 || r.RunSec != 6*3600 || r.Coverage != 1 || len(r.Warnings) != 0 {
		t.Fatalf("details %+v", r)
	}
}

func TestGapsAndWarnings(t *testing.T) {
	c, from, to, run, total, _, _ := shift()
	// Remove 20 minutes of running samples: 5 min held, 15 min no data.
	var g Series
	for i, ts := range run.TS {
		if ts >= from+3600*sec && ts < from+3600*sec+20*60*sec {
			continue
		}
		g.TS, g.V = append(g.TS, ts), append(g.V, run.V[i])
	}
	c.PlannedStop, c.Reject = "", ""
	r := Compute(c, from, to, g, total, Series{}, Series{}, Series{})
	// Samples at -30 s + 10 s*k: the last before the gap is at 3590 s, the next
	// at 4800 s. The gap is 1210 s, of which 300 s are held: 910 s no data.
	wantNoData := 910.0
	if r.NoDataSec != wantNoData {
		t.Fatalf("no data %v s, want ~%v", r.NoDataSec, wantNoData)
	}
	if r.Quality != nil || !strings.Contains(strings.Join(r.Warnings, ";"), "quality is not measured") {
		t.Fatalf("missing quality must be said: %+v", r)
	}
	c.IdealCycle = 60 // twice the real cycle: performance > 100 %
	r = Compute(c, from, to, run, total, Series{}, Series{}, Series{})
	if !strings.Contains(strings.Join(r.Warnings, ";"), "ideal cycle time") {
		t.Fatalf("performance >100%% must warn: %v", r.Warnings)
	}
	r = Compute(c, from, to, Series{}, Series{}, Series{}, Series{}, Series{})
	if r.OEE != nil || r.Coverage != 0 {
		t.Fatalf("no data: %+v", r)
	}
}

func TestShiftStart(t *testing.T) {
	loc, _ := time.LoadLocation("Africa/Harare")
	starts := []string{"06:00", "14:00", "22:00"}
	for in, want := range map[string]string{
		"2026-10-02T07:30": "2026-10-02T06:00",
		"2026-10-02T14:00": "2026-10-02T14:00",
		"2026-10-02T03:10": "2026-10-01T22:00", // night shift started yesterday
		"2026-10-02T23:59": "2026-10-02T22:00",
	} {
		ti, _ := time.ParseInLocation("2006-01-02T15:04", in, loc)
		got, err := ShiftStart(ti, starts, loc)
		if err != nil || got.Format("2006-01-02T15:04") != want {
			t.Errorf("%s -> %s (%v), want %s", in, got.Format("2006-01-02T15:04"), err, want)
		}
	}
	if _, err := ShiftStart(time.Now(), []string{"6am"}, loc); err == nil {
		t.Fatal("bad shift format accepted")
	}
}

func deref(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}
