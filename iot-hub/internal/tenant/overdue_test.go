package tenant

import (
	"strings"
	"testing"
	"time"

	"github.com/williamtatendajose/prediction/iot-hub/internal/anomaly"
	"github.com/williamtatendajose/prediction/iot-hub/internal/twin"
)

func TestOverdue(t *testing.T) {
	tw := twin.New("", func(string) bool { return true })
	det := anomaly.New(anomaly.Config{})
	var got []anomaly.Event
	emit := func(es []anomaly.Event) { got = append(got, es...) }
	disabled := map[string]bool{}
	tw.SetExpected("pump-1", time.Minute) // limit max(2 min, 1.5 min) = 2 min
	tw.SetExpected("never", time.Minute)  // never sent: nothing to wait for
	tw.SetExpected("off", time.Minute)
	tw.Data("pump-1")
	tw.Data("off")
	t0 := time.Now().UnixMilli()
	o := newOverdue(tw, det, emit, func() map[string]bool { return disabled }, t0-3600_000)
	tw.OnData = o.data
	disabled["off"] = true

	o.check(t0 + 119_000)
	if len(got) != 0 {
		t.Fatalf("raised before the limit: %+v", got)
	}
	o.check(t0 + 125_000)
	if len(got) != 1 || got[0].Kind != "overdue" || got[0].Sensor != "pump-1" || got[0].End != 0 ||
		!strings.Contains(got[0].Message, "expected every 1 min") {
		t.Fatalf("overdue alarm %+v", got)
	}
	o.check(t0 + 200_000)
	if len(got) != 1 {
		t.Fatal("raised twice")
	}
	if len(det.Active()) != 1 {
		t.Fatal("not active in the detector (escalation and the bell read it)")
	}
	// Data clears it at once, without waiting for the next check.
	tw.Data("pump-1")
	if len(got) != 2 || got[1].End == 0 || got[1].ID != got[0].ID {
		t.Fatalf("not cleared on data: %+v", got)
	}

	// After a restart, silence counts from the start: no alarm storm.
	got = nil
	late := newOverdue(tw, anomaly.New(anomaly.Config{}), emit, func() map[string]bool { return disabled }, t0+10*3600_000)
	late.check(t0 + 10*3600_000 + 60_000)
	if len(got) != 0 {
		t.Fatalf("overdue right after start: %+v", got)
	}
	late.check(t0 + 10*3600_000 + 121_000)
	if len(got) != 1 || got[0].Sensor != "pump-1" {
		t.Fatalf("not raised once the limit passed after start: %+v", got)
	}
	// Removing the interval clears it.
	tw.SetExpected("pump-1", 0)
	late.check(t0 + 10*3600_000 + 130_000)
	if len(got) != 2 || got[1].End == 0 {
		t.Fatalf("interval removed but alarm still open: %+v", got)
	}
	// "off" was overdue the whole time, but it is disabled.
	for _, e := range got {
		if e.Sensor == "off" || e.Sensor == "never" {
			t.Errorf("alarm for %s", e.Sensor)
		}
	}
	// Disabling an overdue device clears its alarm.
	got = nil
	tw.SetExpected("pump-1", time.Minute)
	dis := map[string]bool{"off": true}
	o2 := newOverdue(tw, anomaly.New(anomaly.Config{}), emit, func() map[string]bool { return dis }, t0)
	o2.check(t0 + 20*3600_000)
	dis["pump-1"] = true
	o2.check(t0 + 20*3600_000 + 10_000)
	if len(got) != 2 || got[1].End == 0 {
		t.Fatalf("disabled device's alarm not cleared: %+v", got)
	}
}
