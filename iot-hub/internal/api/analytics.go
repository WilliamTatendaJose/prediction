package api

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/williamtatendajose/prediction/iot-hub/internal/analytics"
	"github.com/williamtatendajose/prediction/iot-hub/internal/forecast"
	"github.com/williamtatendajose/prediction/iot-hub/internal/store"
	"github.com/williamtatendajose/prediction/iot-hub/internal/tsdb"
)

// parseTime accepts unix ms, RFC 3339, "now", or -<duration> (d = days).
func parseTime(v string, def time.Time) (int64, error) {
	switch {
	case v == "":
		return def.UnixMilli(), nil
	case v == "now":
		return time.Now().UnixMilli(), nil
	case strings.HasPrefix(v, "-"):
		d, err := parseDuration(v[1:])
		if err != nil {
			return 0, err
		}
		return time.Now().Add(-d).UnixMilli(), nil
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return n, nil
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return 0, fmt.Errorf("%w: bad time %q", store.ErrInvalid, v)
	}
	return t.UnixMilli(), nil
}

func parseDuration(v string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(v, "d"); ok {
		f, err := strconv.ParseFloat(n, 64)
		if err != nil || f < 0 {
			return 0, fmt.Errorf("%w: bad duration %q", store.ErrInvalid, v)
		}
		return time.Duration(f * float64(24*time.Hour)), nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("%w: bad duration %q", store.ErrInvalid, v)
	}
	return d, nil
}

// rangeParams reads from/to; from defaults to defFrom before now. Ranges
// exclude "to", so its default is defTo after now: a reading stamped this
// very millisecond is still included.
func rangeParams(r *http.Request, defFrom, defTo time.Duration) (from, to int64, err error) {
	q := r.URL.Query()
	now := time.Now()
	if to, err = parseTime(q.Get("to"), now.Add(defTo)); err != nil {
		return
	}
	if from, err = parseTime(q.Get("from"), now.Add(-defFrom)); err != nil {
		return
	}
	if from >= to {
		err = fmt.Errorf("%w: from must be before to", store.ErrInvalid)
	}
	return
}

func fieldParam(r *http.Request) string {
	if f := r.URL.Query().Get("field"); f != "" {
		return f
	}
	return "value"
}

func (s *Server) series(w http.ResponseWriter, r *http.Request) {
	from, to, err := rangeParams(r, time.Hour, time.Second)
	if err != nil {
		storeErr(w, err)
		return
	}
	var bucket int64
	if b := r.URL.Query().Get("bucket"); b != "" && b != "auto" {
		d, err := parseDuration(b)
		if err != nil || d < time.Second {
			storeErr(w, fmt.Errorf("%w: bucket must be a duration >= 1s", store.ErrInvalid))
			return
		}
		bucket = d.Milliseconds()
		if (to-from)/bucket > 10000 {
			storeErr(w, fmt.Errorf("%w: more than 10000 buckets; use a larger bucket", store.ErrInvalid))
			return
		}
	}
	res, err := s.Analytics.Series(r.Context(), r.PathValue("id"), fieldParam(r), from, to, bucket)
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	from, to, err := rangeParams(r, time.Hour, time.Second)
	if err != nil {
		storeErr(w, err)
		return
	}
	res, err := s.Analytics.Stats(r.Context(), r.PathValue("id"), fieldParam(r), from, to)
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) anomalies(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, to, err := rangeParams(r, 7*24*time.Hour, 24*time.Hour) // devices may run ahead by up to 24 h
	if err != nil {
		storeErr(w, err)
		return
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	evs, err := s.Analytics.Events(r.Context(), tsdb.EventQuery{
		Sensor: q.Get("sensor"), From: from, To: to, Limit: limit,
		ActiveOnly: q.Get("active") == "1" || q.Get("active") == "true",
	})
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, evs)
}

type crossingOut struct {
	Threshold float64 `json:"threshold"`
	Side      string  `json:"side"`
	ETA       int64   `json:"eta,omitempty"`      // ms; absent = not within horizon
	ETAEarly  int64   `json:"etaEarly,omitempty"` // band edges
	ETALate   int64   `json:"etaLate,omitempty"`
}

type forecastOut struct {
	Field     string          `json:"field"`
	Bucket    int64           `json:"bucket"`
	Source    string          `json:"source"`
	History   int             `json:"historyPoints"`
	Filled    int             `json:"filledGaps"`
	Method    string          `json:"method"`
	Params    forecast.Params `json:"params"`
	Skill     float64         `json:"skill"`
	MAE       float64         `json:"mae"`
	NaiveMAE  float64         `json:"naiveMae"`
	T         []int64         `json:"t"`
	Yhat      []float64       `json:"yhat"`
	Lo        []float64       `json:"lo"`
	Hi        []float64       `json:"hi"`
	Crossings []crossingOut   `json:"crossings"`
}

// forecastSeries: GET /api/sensors/{id}/forecast?field=&horizon=6h&history=&threshold=&side=
// Thresholds default to the field's detect.low (below) and detect.high (above).
func (s *Server) forecastSeries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	horizon := 6 * time.Hour
	if v := q.Get("horizon"); v != "" {
		d, err := parseDuration(v)
		if err != nil || d < time.Minute || d > 30*24*time.Hour {
			storeErr(w, fmt.Errorf("%w: horizon must be between 1m and 30d", store.ErrInvalid))
			return
		}
		horizon = d
	}
	history := max(24*time.Hour, 4*horizon)
	if v := q.Get("history"); v != "" {
		d, err := parseDuration(strings.TrimPrefix(v, "-"))
		if err != nil || d < time.Minute {
			storeErr(w, fmt.Errorf("%w: bad history", store.ErrInvalid))
			return
		}
		history = d
	}
	history = min(history, 90*24*time.Hour)
	now := time.Now()
	from, to := now.Add(-history).UnixMilli(), now.Add(time.Second).UnixMilli()
	id, field := r.PathValue("id"), fieldParam(r)
	// Exponential smoothing needs evenly spaced points. Try progressively
	// coarser buckets until at least 80 % of them hold real data (a sensor
	// reporting every minute cannot fill 10 s buckets); short gaps are then
	// interpolated. Mostly-empty history is refused rather than invented.
	var (
		ser    analytics.Series
		y      []float64
		n      int
		filled int
		err    error
	)
	for _, target := range []int{600, 300, 150, 75, 40} {
		ser, err = s.Analytics.Series(r.Context(), id, field, from, to, tsdb.AutoBucket(from, to, target))
		if err != nil {
			storeErr(w, err)
			return
		}
		if len(ser.T) < 2 {
			break
		}
		y, filled = gridFill(ser)
		n = len(y)
		if filled*5 <= n {
			break
		}
	}
	if len(ser.T) < 2 || filled*5 > n {
		storeErr(w, fmt.Errorf("%w: not enough evenly spaced history to forecast", store.ErrInvalid))
		return
	}
	b := ser.Bucket
	steps := int(horizon.Milliseconds() / b)
	steps = max(1, min(steps, 5000))
	season := 0
	if day := int64(24 * time.Hour / time.Millisecond); day%b == 0 && int64(n)*b >= 3*day {
		season = int(day / b)
	}
	res, err := forecast.Forecast(y, steps, season)
	if err != nil {
		storeErr(w, fmt.Errorf("%w: %v", store.ErrInvalid, err))
		return
	}
	// A forecast cannot leave the field's physical range (a tank is never
	// below 0 %), so clamp to the field's min/max when they are defined.
	if sv, err := s.Store.Get(id); err == nil {
		if f, ok := sv.Fields[field]; ok && (f.Min != nil || f.Max != nil) {
			for _, xs := range [][]float64{res.Yhat, res.Lo, res.Hi} {
				for i := range xs {
					if f.Min != nil && xs[i] < *f.Min {
						xs[i] = *f.Min
					}
					if f.Max != nil && xs[i] > *f.Max {
						xs[i] = *f.Max
					}
				}
			}
		}
	}
	last := ser.T[0] + int64(n-1)*b
	out := forecastOut{Field: field, Bucket: b, Source: ser.Source, History: n, Filled: filled,
		Method: res.Method, Params: res.Params, Skill: round3(res.Skill), MAE: res.MAE, NaiveMAE: res.NaiveMAE,
		Yhat: res.Yhat, Lo: res.Lo, Hi: res.Hi, Crossings: []crossingOut{}}
	for k := 1; k <= steps; k++ {
		out.T = append(out.T, last+int64(k)*b)
	}
	at := func(step int) int64 {
		if step == 0 {
			return 0
		}
		return last + int64(step)*b
	}
	type thr struct {
		v    float64
		side string
	}
	var ths []thr
	if v := q.Get("threshold"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		side := q.Get("side")
		if err != nil || (side != "below" && side != "above") {
			storeErr(w, fmt.Errorf("%w: threshold needs a number and side=below|above", store.ErrInvalid))
			return
		}
		ths = append(ths, thr{f, side})
	} else {
		rule := s.Store.Rule(id, field)
		if rule.Low != nil {
			ths = append(ths, thr{*rule.Low, "below"})
		}
		if rule.High != nil {
			ths = append(ths, thr{*rule.High, "above"})
		}
	}
	for _, th := range ths {
		c := forecast.Cross(res, th.v, th.side)
		out.Crossings = append(out.Crossings, crossingOut{Threshold: th.v, Side: th.side, ETA: at(c.Step), ETAEarly: at(c.Early), ETALate: at(c.Late)})
	}
	writeJSON(w, http.StatusOK, out)
}

func round3(f float64) float64 { return math.Round(f*1000) / 1000 }

// gridFill places bucket averages on an even grid and linearly interpolates
// missing buckets. Returns the series and how many points were filled.
func gridFill(ser analytics.Series) ([]float64, int) {
	b := ser.Bucket
	n := int((ser.T[len(ser.T)-1]-ser.T[0])/b) + 1
	y := make([]float64, n)
	have := make([]bool, n)
	for i, t := range ser.T {
		k := int((t - ser.T[0]) / b)
		y[k], have[k] = ser.Avg[i], true
	}
	filled := 0
	for k := 0; k < n; k++ {
		if have[k] {
			continue
		}
		j := k
		for j < n && !have[j] { // the last bucket always has data
			j++
		}
		for m := k; m < j; m++ {
			y[m] = y[k-1] + (y[j]-y[k-1])*float64(m-k+1)/float64(j-k+1)
		}
		filled += j - k
		k = j
	}
	return y, filled
}
