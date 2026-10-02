package alarm

import (
	"context"
	"testing"
	"time"

	"github.com/williamtatendajose/prediction/iot-hub/internal/anomaly"
)

func TestAckShelveAudit(t *testing.T) {
	ctx := context.Background()
	m := New(nil)
	clock := time.Unix(1000, 0)
	m.now = func() time.Time { return clock }
	e := anomaly.Event{ID: "tank.level.range.1", Sensor: "tank", Field: "level", Kind: "range"}

	if _, err := m.Acknowledge(ctx, e, "op1", "maybe", ""); err == nil {
		t.Fatal("unknown verdict accepted")
	}
	if _, err := m.Acknowledge(ctx, e, "op1", "false_alarm", "float stuck"); err != nil {
		t.Fatal(err)
	}
	m.AddNote(ctx, e.ID, "op2", "cleaned float switch")
	got := m.Decorate([]anomaly.Event{e})[0]
	if got.Ack == nil || got.Ack.By != "op1" || got.Ack.Verdict != "false_alarm" || got.Notes != 1 || got.Shelved {
		t.Fatalf("decorate: %+v", got)
	}

	for _, bad := range []struct {
		d      time.Duration
		reason string
	}{{0, "x"}, {8 * 24 * time.Hour, "x"}, {time.Hour, " "}} {
		if _, err := m.Shelve(ctx, "tank", "", "", bad.d, "op1", bad.reason); err == nil {
			t.Errorf("shelve %v %q should fail", bad.d, bad.reason)
		}
	}
	// Shelf on the whole sensor matches any field/kind of it, not other sensors.
	if _, err := m.Shelve(ctx, "tank", "", "", time.Hour, "op1", "level sensor being replaced"); err != nil {
		t.Fatal(err)
	}
	other := anomaly.Event{ID: "x", Sensor: "pump", Kind: "stale"}
	evs := m.Decorate([]anomaly.Event{e, {ID: "y", Sensor: "tank", Kind: "stale"}, other})
	if !evs[0].Shelved || !evs[1].Shelved || evs[2].Shelved {
		t.Fatalf("shelf matching: %v %v %v", evs[0].Shelved, evs[1].Shelved, evs[2].Shelved)
	}
	clock = clock.Add(61 * time.Minute) // expiry is automatic
	if m.Decorate([]anomaly.Event{e})[0].Shelved || len(m.Shelves()) != 0 {
		t.Fatal("shelf should have expired")
	}
	log, _ := m.AuditLog(ctx, 0, 10)
	var actions []string
	for _, a := range log {
		actions = append(actions, a.Action)
	}
	if len(log) != 3 || log[0].Action != "alarm.shelve" || log[2].Action != "alarm.ack" {
		t.Fatalf("audit newest first: %v", actions)
	}
}
