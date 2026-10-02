package report

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/williamtatendajose/prediction/iot-hub/internal/oee"
)

// NextShiftStart returns the first shift start strictly after t.
func NextShiftStart(t time.Time, starts []string, loc *time.Location) (time.Time, error) {
	if len(starts) == 0 {
		return time.Time{}, errors.New("no shifts configured")
	}
	t = t.In(loc)
	var best time.Time
	for day := 0; day <= 1; day++ {
		d := t.AddDate(0, 0, day)
		for _, s := range starts {
			hm, err := time.Parse("15:04", s)
			if err != nil {
				return time.Time{}, fmt.Errorf("bad shift start %q", s)
			}
			c := time.Date(d.Year(), d.Month(), d.Day(), hm.Hour(), hm.Minute(), 0, 0, loc)
			if c.After(t) && (best.IsZero() || c.Before(best)) {
				best = c
			}
		}
	}
	return best, nil
}

// PreviousShift returns the last completed shift before t: [start, end).
func PreviousShift(t time.Time, starts []string, loc *time.Location) (time.Time, time.Time, error) {
	end, err := oee.ShiftStart(t, starts, loc) // the current shift began when the previous ended
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	start, err := oee.ShiftStart(end.Add(-time.Millisecond), starts, loc)
	return start, end, err
}

// Schedule calls run at every shift change for the shift that just ended.
// A report missed while the hub was down is not sent afterwards (it can be
// produced on demand from the API).
func Schedule(ctx context.Context, starts []string, loc *time.Location, run func(from, to time.Time), logf func(string, ...any)) {
	for {
		next, err := NextShiftStart(time.Now(), starts, loc)
		if err != nil {
			logf("report schedule: %v", err)
			return
		}
		t := time.NewTimer(time.Until(next) + 5*time.Second) // let the last readings of the shift land
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		from, to, err := PreviousShift(next.Add(time.Second), starts, loc)
		if err != nil {
			logf("report schedule: %v", err)
			continue
		}
		run(from, to)
	}
}
