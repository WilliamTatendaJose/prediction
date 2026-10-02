package report

import (
	"strings"
	"testing"
	"time"

	"github.com/williamtatendajose/prediction/iot-hub/internal/oee"
)

func TestShiftTimes(t *testing.T) {
	loc := time.FixedZone("CAT", 2*3600)
	starts := []string{"06:00", "14:00", "22:00"}
	at := func(d, h, m int) time.Time { return time.Date(2026, 10, d, h, m, 0, 0, loc) }
	for _, c := range []struct {
		now, next, from, to time.Time
	}{
		{at(2, 10, 0), at(2, 14, 0), at(1, 22, 0), at(2, 6, 0)},  // day shift: previous is last night
		{at(2, 23, 30), at(3, 6, 0), at(2, 14, 0), at(2, 22, 0)}, // night shift, before midnight
		{at(3, 2, 0), at(3, 6, 0), at(2, 14, 0), at(2, 22, 0)},   // night shift, after midnight
		{at(2, 14, 0), at(2, 22, 0), at(2, 6, 0), at(2, 14, 0)},  // exactly at a change
	} {
		next, err := NextShiftStart(c.now, starts, loc)
		if err != nil || !next.Equal(c.next) {
			t.Errorf("%v: next %v, want %v (%v)", c.now, next, c.next, err)
		}
		from, to, err := PreviousShift(c.now, starts, loc)
		if err != nil || !from.Equal(c.from) || !to.Equal(c.to) {
			t.Errorf("%v: previous %v–%v, want %v–%v (%v)", c.now, from, to, c.from, c.to, err)
		}
	}
	// What the scheduler reports when it wakes just after 06:00: the night.
	from, to, _ := PreviousShift(at(3, 6, 0).Add(5*time.Second), starts, loc)
	if !from.Equal(at(2, 22, 0)) || !to.Equal(at(3, 6, 0)) {
		t.Errorf("scheduled window %v–%v", from, to)
	}
	if _, err := NextShiftStart(at(2, 0, 0), nil, loc); err == nil {
		t.Error("no shifts should be an error")
	}
}

func TestRenderEscapesDeviceText(t *testing.T) {
	p := func(f float64) *float64 { return &f }
	loc := time.UTC
	from := time.Date(2026, 10, 2, 6, 0, 0, 0, loc).UnixMilli()
	r := Report{Title: "Shift report", Period: Label(from, from+8*3600_000, loc), From: from, To: from + 8*3600_000,
		Machines: []Machine{{Sensor: "m1", Name: `<script>alert(1)</script>`, Result: oee.Result{OEE: p(0.9), Availability: p(1), Performance: p(0.9), Quality: p(1), Total: 120}}},
		Alarms: Alarms{Total: 1, Unacked: 1, ByKind: map[string]int{"range": 1}, Longest: []Alarm{{Sensor: "tank", Field: "t", Kind: "range",
			Start: from + 3600_000, DurationSec: 750, Message: `<img src=x onerror=alert(1)>`}}},
		Silent: []string{"pump-2"}, Link: "javascript:alert(1)"}
	h := r.HTML(loc)
	for _, bad := range []string{"<script>alert", "<img src", `href="javascript:`} {
		if strings.Contains(h, bad) {
			t.Errorf("HTML contains %q", bad)
		}
	}
	for _, want := range []string{"&lt;script&gt;", "90.0%", "world class", "13 min", "not acknowledged", "pump-2", "Fri 2 Oct 2026, 06:00–14:00"} {
		if !strings.Contains(h, want) {
			t.Errorf("HTML missing %q", want)
		}
	}
	txt := r.Text(loc)
	for _, want := range []string{"OEE 90.0% (world class)", "120 parts", "07:00 tank.t, 13 min, not acknowledged", "Alarms: 1 (1 unacknowledged, 0 shelved) — 1 range", "went silent: pump-2"} {
		if !strings.Contains(txt, want) {
			t.Errorf("text missing %q in\n%s", want, txt)
		}
	}
}
