// Package oee computes Overall Equipment Effectiveness from machine tags:
//
//	OEE = Availability x Performance x Quality
//	Availability = run time / planned production time
//	Performance  = ideal cycle time x parts made / run time
//	Quality      = good parts / parts made
//
// Inputs are raw tag samples. Rules (chosen to be explainable on the shop
// floor rather than clever):
//   - a state tag (running, planned stop) holds its value until the next
//     sample, for at most MaxHold; longer gaps are "no data" and are left out
//     of planned time, and reported as coverage
//   - counters are summed as positive steps from the last value before the
//     window; a drop means the counter was reset, so the new value counts
//     from zero
package oee

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"time"
)

// Config is set on a sensor definition ("oee": {...}).
type Config struct {
	Running     string  `json:"running"`               // bool: machine producing
	Total       string  `json:"total"`                 // counter: parts made
	Good        string  `json:"good,omitempty"`        // counter: good parts, or
	Reject      string  `json:"reject,omitempty"`      // counter: rejected parts
	PlannedStop string  `json:"plannedStop,omitempty"` // bool: planned downtime (breaks, changeovers)
	IdealCycle  float64 `json:"idealCycleSec"`         // ideal seconds per part
	MaxHold     string  `json:"maxHold,omitempty"`     // default 5m
}

func (c *Config) Validate() error {
	switch {
	case c.Running == "" || c.Total == "":
		return errors.New("oee needs running and total fields")
	case c.IdealCycle <= 0:
		return errors.New("oee idealCycleSec must be > 0")
	case c.Good != "" && c.Reject != "":
		return errors.New("oee: use good or reject, not both")
	}
	if c.MaxHold != "" {
		if d, err := time.ParseDuration(c.MaxHold); err != nil || d <= 0 {
			return fmt.Errorf("oee: bad maxHold %q", c.MaxHold)
		}
	}
	return nil
}

func (c *Config) Fields() []string {
	out := []string{c.Running, c.Total}
	for _, f := range []string{c.Good, c.Reject, c.PlannedStop} {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

func (c *Config) maxHold() int64 {
	if d, err := time.ParseDuration(c.MaxHold); err == nil && d > 0 {
		return d.Milliseconds()
	}
	return (5 * time.Minute).Milliseconds()
}

// Series is a tag's samples, oldest first. It should include the last sample
// before the window (state and counter baseline).
type Series struct {
	TS []int64
	V  []float64
}

type Result struct {
	From         int64    `json:"from"`
	To           int64    `json:"to"`
	OEE          *float64 `json:"oee"`
	Availability *float64 `json:"availability"`
	Performance  *float64 `json:"performance"`
	Quality      *float64 `json:"quality"`
	PlannedSec   float64  `json:"plannedSec"`
	RunSec       float64  `json:"runSec"`
	NoDataSec    float64  `json:"noDataSec"`
	Coverage     float64  `json:"coverage"` // share of the window with running data
	Total        float64  `json:"total"`
	Good         float64  `json:"good"`
	Reject       float64  `json:"reject"`
	IdealCycle   float64  `json:"idealCycleSec"`
	Warnings     []string `json:"warnings"`
}

// stateAt returns the held value of a state series at t: the last sample at
// or before t, held for [sample, sample+hold). Integration segments start
// exactly at expiry instants, so the end must be exclusive.
func stateAt(s Series, t, hold int64) (float64, bool) {
	i := sort.Search(len(s.TS), func(i int) bool { return s.TS[i] > t }) - 1
	if i < 0 || t-s.TS[i] >= hold {
		return 0, false
	}
	return s.V[i], true
}

// counted sums positive steps of a counter over [from, to), the same
// half-open window as the state integration: a count stamped exactly at a
// shift change belongs to the shift that starts then.
func counted(s Series, from, to int64) float64 {
	var sum float64
	prev, have := 0.0, false
	for i, t := range s.TS {
		if t >= to {
			break
		}
		v := s.V[i]
		if t < from {
			prev, have = v, true // baseline: last value before the window
			continue
		}
		if have {
			if v >= prev {
				sum += v - prev
			} else {
				sum += v // counter reset (PLC restart, shift reset)
			}
		}
		prev, have = v, true
	}
	return sum
}

func ptr(f float64) *float64 { return &f }

// Compute evaluates one window [from, to).
func Compute(c Config, from, to int64, running, total, good, reject, planned Series) Result {
	r := Result{From: from, To: to, IdealCycle: c.IdealCycle, Warnings: []string{}}
	hold := c.maxHold()
	// Integrate states over every point where any state can change: sample
	// times and the instants where a held value expires.
	cuts := []int64{from, to}
	for _, s := range []Series{running, planned} {
		for _, t := range s.TS {
			for _, x := range []int64{t, t + hold} {
				if x > from && x < to {
					cuts = append(cuts, x)
				}
			}
		}
	}
	sort.Slice(cuts, func(i, j int) bool { return cuts[i] < cuts[j] })
	var plannedMs, runMs, noDataMs float64
	for i := 0; i+1 < len(cuts); i++ {
		a, b := cuts[i], cuts[i+1]
		if b <= a {
			continue
		}
		d := float64(b - a)
		run, ok := stateAt(running, a, hold)
		if !ok {
			noDataMs += d
			continue
		}
		if stop, ok := stateAt(planned, a, hold); ok && stop != 0 {
			continue // planned downtime is not lost time
		}
		plannedMs += d
		if run != 0 {
			runMs += d
		}
	}
	r.PlannedSec, r.RunSec, r.NoDataSec = plannedMs/1000, runMs/1000, noDataMs/1000
	if to > from {
		r.Coverage = 1 - noDataMs/float64(to-from)
	}
	r.Total = counted(total, from, to)
	switch {
	case len(good.TS) > 0:
		r.Good = counted(good, from, to)
		r.Reject = math.Max(0, r.Total-r.Good)
	case len(reject.TS) > 0:
		r.Reject = counted(reject, from, to)
		r.Good = math.Max(0, r.Total-r.Reject)
	default:
		r.Good = r.Total
	}

	if r.PlannedSec > 0 {
		r.Availability = ptr(r.RunSec / r.PlannedSec)
	}
	if r.RunSec > 0 {
		r.Performance = ptr(c.IdealCycle * r.Total / r.RunSec)
	}
	q := 1.0
	if r.Total > 0 && (len(good.TS) > 0 || len(reject.TS) > 0) {
		q = r.Good / r.Total
		r.Quality = ptr(q)
	} else if c.Good == "" && c.Reject == "" {
		r.Warnings = append(r.Warnings, "quality is not measured (no good/reject counter): OEE assumes 100 % quality")
	}
	if r.Availability != nil && r.Performance != nil {
		r.OEE = ptr(*r.Availability * *r.Performance * q)
	}
	if r.Performance != nil && *r.Performance > 1.05 {
		r.Warnings = append(r.Warnings, fmt.Sprintf("performance is %.0f %%: the ideal cycle time (%gs) is probably too long", *r.Performance*100, c.IdealCycle))
	}
	if r.Coverage < 0.9 && to > from {
		r.Warnings = append(r.Warnings, fmt.Sprintf("only %.0f %% of the window has running data", r.Coverage*100))
	}
	if r.RunSec > 0 && r.Total == 0 {
		r.Warnings = append(r.Warnings, "the machine ran but no parts were counted: check the total counter")
	}
	return r
}

// ShiftStart returns the start of the shift containing t, given shift start
// times of day (e.g. "06:00","14:00","22:00") in loc.
func ShiftStart(t time.Time, starts []string, loc *time.Location) (time.Time, error) {
	if len(starts) == 0 {
		return time.Time{}, errors.New("no shifts configured (-shifts)")
	}
	t = t.In(loc)
	best := time.Time{}
	for dayOff := -1; dayOff <= 0; dayOff++ {
		d := t.AddDate(0, 0, dayOff)
		for _, s := range starts {
			hm, err := time.Parse("15:04", s)
			if err != nil {
				return time.Time{}, fmt.Errorf("bad shift start %q (want HH:MM)", s)
			}
			c := time.Date(d.Year(), d.Month(), d.Day(), hm.Hour(), hm.Minute(), 0, 0, loc)
			if !c.After(t) && c.After(best) {
				best = c
			}
		}
	}
	return best, nil
}
