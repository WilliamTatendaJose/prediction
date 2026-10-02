package api

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

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

// rangeParams reads from/to; from defaults to defFrom before now.
func rangeParams(r *http.Request, defFrom time.Duration) (from, to int64, err error) {
	q := r.URL.Query()
	now := time.Now()
	if to, err = parseTime(q.Get("to"), now); err != nil {
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
	from, to, err := rangeParams(r, time.Hour)
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
	from, to, err := rangeParams(r, time.Hour)
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
	from, to, err := rangeParams(r, 7*24*time.Hour)
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
