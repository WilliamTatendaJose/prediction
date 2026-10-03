package tenant

import (
	"fmt"
	"sync"
	"time"

	"github.com/williamtatendajose/prediction/iot-hub/internal/anomaly"
	"github.com/williamtatendajose/prediction/iot-hub/internal/twin"
)

// overdue raises an alarm (kind "overdue") for a device that has missed its
// expected interval, and clears it when data arrives. It is an ordinary
// episode, so acknowledging, shelving, notes, notifications and escalation
// all apply.
//
// A device is overdue once max(2 x interval, interval + 30 s) has passed
// since its last data, counting silence only from when this hub started:
// a hub that was down must not find every device overdue at once. A
// device that never sent data, or is disabled, raises nothing.
type overdue struct {
	twins    *twin.Service
	det      *anomaly.Detector
	emit     func([]anomaly.Event)
	disabled func() map[string]bool // ids that are disabled; nil: none
	started  int64

	mu   sync.Mutex
	open map[string]bool
}

func newOverdue(t *twin.Service, det *anomaly.Detector, emit func([]anomaly.Event), disabled func() map[string]bool, started int64) *overdue {
	return &overdue{twins: t, det: det, emit: emit, disabled: disabled, started: started, open: map[string]bool{}}
}

const overdueKey = "overdue:"

// Limit is how long a device with this expected interval may be silent.
func overdueLimit(expectedSec int64) int64 {
	return max(2*expectedSec*1000, expectedSec*1000+30_000)
}

// check runs periodically.
func (o *overdue) check(now int64) {
	seen := map[string]bool{}
	var dis map[string]bool
	if o.disabled != nil {
		dis = o.disabled()
	}
	for _, w := range o.twins.Watched() {
		seen[w.ID] = true
		if w.LastData == 0 || dis[w.ID] {
			o.clear(w.ID, now)
			continue
		}
		silent := now - max(w.LastData, o.started)
		if silent <= overdueLimit(w.ExpectedSec) {
			o.clear(w.ID, now)
			continue
		}
		msg := fmt.Sprintf("%s has sent no data for %s (expected every %s)", w.ID, human(now-w.LastData), human(w.ExpectedSec*1000))
		if e, ok := o.det.Raise(overdueKey+w.ID, w.ID, "", "overdue", now, float64(now-w.LastData)/1000, msg); ok {
			o.mu.Lock()
			o.open[w.ID] = true
			o.mu.Unlock()
			o.emit([]anomaly.Event{e})
		}
	}
	// Interval removed, or device deleted: nothing to wait for any more.
	o.mu.Lock()
	var gone []string
	for id := range o.open {
		if !seen[id] {
			gone = append(gone, id)
		}
	}
	o.mu.Unlock()
	for _, id := range gone {
		o.clear(id, now)
	}
}

// data: a reading from id arrived.
func (o *overdue) data(id string) {
	o.mu.Lock()
	open := o.open[id]
	o.mu.Unlock()
	if open {
		o.clear(id, time.Now().UnixMilli())
	}
}

func (o *overdue) clear(id string, now int64) {
	o.mu.Lock()
	open := o.open[id]
	delete(o.open, id)
	o.mu.Unlock()
	if !open {
		return
	}
	if e, ok := o.det.Clear(overdueKey+id, now); ok {
		o.emit([]anomaly.Event{e})
	}
}

func human(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%.1f h", d.Hours())
	default:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
}
