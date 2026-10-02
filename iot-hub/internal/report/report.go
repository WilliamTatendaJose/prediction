// Package report builds shift and period reports: machine OEE, energy
// used, alarms and silent sensors, from the same data and code paths as the
// dashboard (so a report never disagrees with what operators saw).
package report

import (
	"context"
	"sort"
	"time"

	"github.com/williamtatendajose/prediction/iot-hub/internal/analytics"
	"github.com/williamtatendajose/prediction/iot-hub/internal/oee"
	"github.com/williamtatendajose/prediction/iot-hub/internal/store"
	"github.com/williamtatendajose/prediction/iot-hub/internal/tsdb"
)

const maxPoints = 2_000_000

// MachineOEE computes OEE for one machine sensor over [from, to).
func MachineOEE(ctx context.Context, an *analytics.Service, sv store.SensorView, from, to int64) (oee.Result, error) {
	c := *sv.OEE
	var s [5]oee.Series
	for i, f := range []string{c.Running, c.Total, c.Good, c.Reject, c.PlannedStop} {
		if f == "" {
			continue
		}
		ts, vs, err := an.Raw(ctx, sv.ID, f, from, to, maxPoints)
		if err != nil {
			return oee.Result{}, err
		}
		s[i] = oee.Series{TS: ts, V: vs}
	}
	return oee.Compute(c, from, to, s[0], s[1], s[2], s[3], s[4]), nil
}

type Machine struct {
	Sensor string     `json:"sensor"`
	Name   string     `json:"name"`
	Result oee.Result `json:"result"`
}

type Energy struct {
	Sensor   string  `json:"sensor"`
	Name     string  `json:"name"`
	Field    string  `json:"field"`
	Unit     string  `json:"unit,omitempty"`
	Consumed float64 `json:"consumed"`
}

type Alarm struct {
	Sensor      string  `json:"sensor"`
	Field       string  `json:"field,omitempty"`
	Kind        string  `json:"kind"`
	Start       int64   `json:"start"`
	DurationSec float64 `json:"durationSec"`
	Open        bool    `json:"open"`
	Verdict     string  `json:"verdict,omitempty"`
	AckedBy     string  `json:"ackedBy,omitempty"`
	Message     string  `json:"message"`
}

type Alarms struct {
	Total     int            `json:"total"`
	Unacked   int            `json:"unacknowledged"`
	Shelved   int            `json:"shelved"`
	ByKind    map[string]int `json:"byKind"`
	ByVerdict map[string]int `json:"byVerdict"`
	Longest   []Alarm        `json:"longest"`
}

type Report struct {
	Title     string    `json:"title"`
	Period    string    `json:"period"`
	From      int64     `json:"from"`
	To        int64     `json:"to"`
	Generated int64     `json:"generated"`
	Machines  []Machine `json:"machines"`
	Energy    []Energy  `json:"energy"`
	Alarms    Alarms    `json:"alarms"`
	Silent    []string  `json:"silentSensors"`
	Link      string    `json:"link,omitempty"`
}

// Build assembles a report for [from, to).
func Build(ctx context.Context, st *store.Store, an *analytics.Service, from, to int64, period string, loc *time.Location) (Report, error) {
	r := Report{Title: "Shift report", Period: period, From: from, To: to, Generated: time.Now().UnixMilli(),
		Machines: []Machine{}, Energy: []Energy{}, Silent: []string{},
		Alarms: Alarms{ByKind: map[string]int{}, ByVerdict: map[string]int{}, Longest: []Alarm{}}}
	if period == "" {
		r.Period = Label(from, to, loc)
	}
	for _, sv := range st.List() {
		name := sv.Name
		if name == "" {
			name = sv.ID
		}
		if sv.OEE != nil {
			res, err := MachineOEE(ctx, an, sv, from, to)
			if err != nil {
				return r, err
			}
			r.Machines = append(r.Machines, Machine{Sensor: sv.ID, Name: name, Result: res})
		}
		// Energy: every integrated field; consumption is the rise of its
		// running total over the window (resets handled like counters).
		for field, f := range sv.Fields {
			if f.Calc == nil || f.Calc.Integrate == "" {
				continue
			}
			ts, vs, err := an.Raw(ctx, sv.ID, field, from, to, maxPoints)
			if err != nil {
				return r, err
			}
			r.Energy = append(r.Energy, Energy{Sensor: sv.ID, Name: name, Field: field, Unit: f.Unit,
				Consumed: oee.Counted(oee.Series{TS: ts, V: vs}, from, to)})
		}
	}
	sort.Slice(r.Machines, func(i, j int) bool { return r.Machines[i].Name < r.Machines[j].Name })
	sort.Slice(r.Energy, func(i, j int) bool {
		return r.Energy[i].Name+r.Energy[i].Field < r.Energy[j].Name+r.Energy[j].Field
	})

	evs, err := an.Events(ctx, tsdb.EventQuery{From: from, To: to, Limit: 1000})
	if err != nil {
		return r, err
	}
	silent := map[string]bool{}
	var all []Alarm
	for _, e := range evs {
		r.Alarms.Total++
		r.Alarms.ByKind[e.Kind]++
		if e.Shelved {
			r.Alarms.Shelved++
		}
		v := ""
		by := ""
		if e.Ack != nil {
			v, by = e.Ack.Verdict, e.Ack.By
			if v == "" {
				v = "acknowledged"
			}
		} else if !e.Shelved {
			r.Alarms.Unacked++
		}
		if v != "" {
			r.Alarms.ByVerdict[v]++
		}
		if e.Kind == "stale" {
			silent[e.Sensor] = true
		}
		end := e.End
		if end == 0 || end > to {
			end = to
		}
		all = append(all, Alarm{Sensor: e.Sensor, Field: e.Field, Kind: e.Kind, Start: e.Start,
			DurationSec: float64(max(0, end-e.Start)) / 1000, Open: e.End == 0, Verdict: v, AckedBy: by, Message: e.Message})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].DurationSec > all[j].DurationSec })
	r.Alarms.Longest = append(r.Alarms.Longest, all[:min(len(all), 10)]...)
	for s := range silent {
		r.Silent = append(r.Silent, s)
	}
	sort.Strings(r.Silent)
	return r, nil
}

// Label renders a window like "Fri 2 Oct 2026, 06:00–14:00".
func Label(from, to int64, loc *time.Location) string {
	a, b := time.UnixMilli(from).In(loc), time.UnixMilli(to).In(loc)
	if a.YearDay() == b.YearDay() || b.Sub(a) <= 24*time.Hour && b.Hour() == 0 && b.Minute() == 0 {
		return a.Format("Mon 2 Jan 2006, 15:04") + "–" + b.Format("15:04")
	}
	return a.Format("Mon 2 Jan 15:04") + " – " + b.Format("Mon 2 Jan 2006 15:04")
}
