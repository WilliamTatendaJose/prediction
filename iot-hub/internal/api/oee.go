package api

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/williamtatendajose/prediction/iot-hub/internal/notify"
	"github.com/williamtatendajose/prediction/iot-hub/internal/oee"
	"github.com/williamtatendajose/prediction/iot-hub/internal/store"
)

const maxOEEPoints = 2_000_000 // ~23 days of 1 Hz samples per tag

type oeeOut struct {
	Sensor  string       `json:"sensor"`
	Name    string       `json:"name"`
	Range   string       `json:"range"`
	Result  oee.Result   `json:"result"`
	Buckets []oee.Result `json:"buckets,omitempty"`
}

// window resolves from/to, adding "shift" (current shift start) and
// "today" (local midnight).
func (s *Server) window(r *http.Request) (int64, int64, string, error) {
	q := r.URL.Query()
	now := time.Now().In(s.loc())
	switch q.Get("from") {
	case "shift", "":
		if shifts := s.shifts(); len(shifts) > 0 {
			st, err := oee.ShiftStart(now, shifts, s.loc())
			return st.UnixMilli(), now.UnixMilli(), "current shift (since " + st.Format("15:04") + ")", err
		}
		if q.Get("from") == "shift" {
			return 0, 0, "", fmt.Errorf("%w: no shifts configured (-shifts)", store.ErrInvalid)
		}
		return now.Add(-8 * time.Hour).UnixMilli(), now.UnixMilli(), "last 8 h", nil
	case "today":
		mid := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, s.loc())
		return mid.UnixMilli(), now.UnixMilli(), "today", nil
	}
	from, to, err := rangeParams(r, 8*time.Hour, 0)
	return from, to, q.Get("from"), err
}

func (s *Server) loc() *time.Location {
	s.liveMu.RLock()
	defer s.liveMu.RUnlock()
	if s.Location != nil {
		return s.Location
	}
	return time.Local
}

func (s *Server) shifts() []string {
	s.liveMu.RLock()
	defer s.liveMu.RUnlock()
	return s.Shifts
}

func (s *Server) notifier() *notify.Notifier {
	s.liveMu.RLock()
	defer s.liveMu.RUnlock()
	return s.Notifier
}

func (s *Server) reporter() *notify.Notifier {
	s.liveMu.RLock()
	defer s.liveMu.RUnlock()
	return s.Reporter
}

// SetLive swaps notification and shift settings while serving.
func (s *Server) SetLive(n, rep *notify.Notifier, shifts []string, loc *time.Location) {
	s.liveMu.Lock()
	s.Notifier, s.Reporter, s.Shifts, s.Location = n, rep, shifts, loc
	s.liveMu.Unlock()
}

// oeeHandler: GET /api/sensors/{id}/oee?from=shift|today|-24h&to=&bucket=1h
func (s *Server) oeeHandler(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sv, err := s.Store.Get(id)
	if err != nil {
		storeErr(w, err)
		return
	}
	if sv.OEE == nil {
		writeErr(w, http.StatusBadRequest, errors.New("this sensor has no oee configuration"))
		return
	}
	from, to, label, err := s.window(r)
	if err != nil {
		storeErr(w, err)
		return
	}
	c := *sv.OEE
	get := func(field string) (oee.Series, error) {
		if field == "" {
			return oee.Series{}, nil
		}
		ts, vs, err := s.Analytics.Raw(r.Context(), id, field, from, to, maxOEEPoints)
		return oee.Series{TS: ts, V: vs}, err
	}
	var series [5]oee.Series
	for i, f := range []string{c.Running, c.Total, c.Good, c.Reject, c.PlannedStop} {
		if series[i], err = get(f); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	}
	compute := func(a, b int64) oee.Result {
		return oee.Compute(c, a, b, series[0], series[1], series[2], series[3], series[4])
	}
	out := oeeOut{Sensor: id, Name: sv.Name, Range: label, Result: compute(from, to)}
	if v := r.URL.Query().Get("bucket"); v != "" {
		d, err := parseDuration(v)
		if err != nil || d < 5*time.Minute || int64(to-from)/d.Milliseconds() > 500 {
			writeErr(w, http.StatusBadRequest, errors.New("bucket must be >= 5m and give at most 500 buckets"))
			return
		}
		b := d.Milliseconds()
		start := from - from%b
		if s.loc() != time.UTC { // align hour/day buckets to local time
			_, off := time.UnixMilli(from).In(s.loc()).Zone()
			start = from - (from+int64(off)*1000)%b
		}
		for a := start; a < to; a += b {
			out.Buckets = append(out.Buckets, compute(max(a, from), min(a+b, to)))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// oeeOverview: GET /api/oee?from=shift  every machine's OEE, worst first.
func (s *Server) oeeOverview(w http.ResponseWriter, r *http.Request) {
	from, to, label, err := s.window(r)
	if err != nil {
		storeErr(w, err)
		return
	}
	out := []oeeOut{}
	for _, sv := range s.Store.List() {
		if sv.OEE == nil {
			continue
		}
		c := *sv.OEE
		var series [5]oee.Series
		for i, f := range []string{c.Running, c.Total, c.Good, c.Reject, c.PlannedStop} {
			if f == "" {
				continue
			}
			ts, vs, err := s.Analytics.Raw(r.Context(), sv.ID, f, from, to, maxOEEPoints)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, err)
				return
			}
			series[i] = oee.Series{TS: ts, V: vs}
		}
		out = append(out, oeeOut{Sensor: sv.ID, Name: sv.Name, Range: label,
			Result: oee.Compute(c, from, to, series[0], series[1], series[2], series[3], series[4])})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].Result.OEE, out[j].Result.OEE
		if a == nil || b == nil {
			return b == nil && a != nil
		}
		return *a < *b
	})
	writeJSON(w, http.StatusOK, out)
}
