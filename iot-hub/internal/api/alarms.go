package api

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/williamtatendajose/prediction/iot-hub/internal/alarm"
	"github.com/williamtatendajose/prediction/iot-hub/internal/tsdb"
)

func alarmErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, alarm.ErrInvalid):
		writeErr(w, http.StatusBadRequest, err)
	case errors.Is(err, alarm.ErrNotFound):
		writeErr(w, http.StatusNotFound, err)
	default:
		writeErr(w, http.StatusInternalServerError, err)
	}
}

func (s *Server) audit(r *http.Request, action, target, detail string) {
	if s.Alarms != nil {
		s.Alarms.Audit(r.Context(), actor(r), action, target, detail)
	}
}

type ackReq struct {
	Verdict string `json:"verdict"`
	Note    string `json:"note"`
}

// ackEvent: POST /api/anomalies/{eid}/ack  {"verdict":"false_alarm","note":"sensor knocked"}
func (s *Server) ackEvent(w http.ResponseWriter, r *http.Request) {
	var req ackReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	e, ok, err := s.Analytics.Event(r.Context(), r.PathValue("eid"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, errors.New("no such episode"))
		return
	}
	a, err := s.Alarms.Acknowledge(r.Context(), e, actor(r), req.Verdict, req.Note)
	if err != nil {
		alarmErr(w, err)
		return
	}
	e.Ack = &a
	writeJSON(w, http.StatusOK, e)
}

func (s *Server) addNote(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	id := r.PathValue("eid")
	if _, ok, err := s.Analytics.Event(r.Context(), id); err != nil || !ok {
		writeErr(w, http.StatusNotFound, errors.New("no such episode"))
		return
	}
	n, err := s.Alarms.AddNote(r.Context(), id, actor(r), req.Text)
	if err != nil {
		alarmErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, n)
}

func (s *Server) listNotes(w http.ResponseWriter, r *http.Request) {
	ns, err := s.Alarms.Notes(r.Context(), r.PathValue("eid"))
	if err != nil {
		alarmErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ns)
}

func (s *Server) listShelves(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.Alarms.Shelves())
}

type shelveReq struct {
	Sensor   string `json:"sensor"`
	Field    string `json:"field"`
	Kind     string `json:"kind"`
	Duration string `json:"duration"` // e.g. "8h"
	Reason   string `json:"reason"`
}

func (s *Server) shelve(w http.ResponseWriter, r *http.Request) {
	var req shelveReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	d, err := parseDuration(req.Duration)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	sh, err := s.Alarms.Shelve(r.Context(), req.Sensor, req.Field, req.Kind, d, actor(r), req.Reason)
	if err != nil {
		alarmErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sh)
}

func (s *Server) unshelve(w http.ResponseWriter, r *http.Request) {
	if err := s.Alarms.Unshelve(r.Context(), r.PathValue("key"), actor(r)); err != nil {
		alarmErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) auditLog(w http.ResponseWriter, r *http.Request) {
	from, _, err := rangeParams(r, 30*24*time.Hour, time.Second)
	if err != nil {
		storeErr(w, err)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	es, err := s.Alarms.AuditLog(r.Context(), from, limit)
	if err != nil {
		alarmErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, es)
}

// labels: GET /api/labels.csv?from=-90d  Episodes with operator verdicts,
// ready to join with readings as training data for a model.
func (s *Server) labels(w http.ResponseWriter, r *http.Request) {
	from, to, err := rangeParams(r, 90*24*time.Hour, 24*time.Hour)
	if err != nil {
		storeErr(w, err)
		return
	}
	evs, err := s.Analytics.Events(r.Context(), tsdb.EventQuery{From: from, To: to, Limit: 1000})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="iothub-labels.csv"`)
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"episode_id", "sensor", "field", "kind", "start", "end", "value", "score", "verdict", "acked_by", "acked_at", "note"})
	iso := func(ms int64) string {
		if ms == 0 {
			return ""
		}
		return time.UnixMilli(ms).UTC().Format(time.RFC3339)
	}
	for _, e := range evs {
		if e.Ack == nil || e.Ack.Verdict == "" {
			continue // unlabelled
		}
		_ = cw.Write([]string{e.ID, e.Sensor, e.Field, e.Kind, iso(e.Start), iso(e.End),
			fmt.Sprint(e.Value), fmt.Sprint(e.Score), e.Ack.Verdict, e.Ack.By, iso(e.Ack.At), csvSafe(e.Ack.Note)})
	}
	cw.Flush()
}

// csvSafe defuses spreadsheet formula injection in free text.
func csvSafe(s string) string {
	if s != "" && (s[0] == '=' || s[0] == '+' || s[0] == '-' || s[0] == '@') {
		return "'" + s
	}
	return s
}
