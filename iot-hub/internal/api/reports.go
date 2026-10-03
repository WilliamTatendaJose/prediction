package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/williamtatendajose/prediction/iot-hub/internal/oee"
	"github.com/williamtatendajose/prediction/iot-hub/internal/report"
	"github.com/williamtatendajose/prediction/iot-hub/internal/store"
)

// reportWindow: ?shift=previous (default) | current, or explicit from/to.
func (s *Server) reportWindow(r *http.Request) (int64, int64, string, error) {
	q := r.URL.Query()
	if q.Get("from") != "" {
		from, to, err := rangeParams(r, 0, 0)
		return from, to, "", err
	}
	shift := q.Get("shift")
	if shift != "" && shift != "previous" && shift != "current" {
		return 0, 0, "", fmt.Errorf("%w: shift must be previous or current", store.ErrInvalid)
	}
	shifts := s.shifts()
	if len(shifts) == 0 {
		now := time.Now()
		return now.Add(-24 * time.Hour).UnixMilli(), now.UnixMilli(), "last 24 hours", nil
	}
	now := time.Now()
	if shift == "current" {
		start, err := oee.ShiftStart(now, shifts, s.loc())
		return start.UnixMilli(), now.UnixMilli(), "", err
	}
	a, b, err := report.PreviousShift(now, shifts, s.loc())
	return a.UnixMilli(), b.UnixMilli(), "", err
}

// BuildReport is shared by the API and the scheduler.
func (s *Server) BuildReport(r *http.Request, from, to int64, period string) (report.Report, error) {
	rep, err := report.Build(r.Context(), s.Store, s.Analytics, from, to, period, s.loc())
	if err == nil && s.PublicURL != "" {
		rep.Link = s.PublicURL
	}
	return rep, err
}

// reports: GET /api/reports?shift=previous|current&format=json|html|text
func (s *Server) reports(w http.ResponseWriter, r *http.Request) {
	from, to, period, err := s.reportWindow(r)
	if err != nil {
		storeErr(w, err)
		return
	}
	rep, err := s.BuildReport(r, from, to, period)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	switch r.URL.Query().Get("format") {
	case "html":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, rep.HTML(s.loc()))
	case "text":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, rep.Text(s.loc()))
	default:
		writeJSON(w, http.StatusOK, rep)
	}
}

// sendReport: POST /api/reports/send (admin) sends the previous shift's
// report to the report targets now; used to check setup.
func (s *Server) sendReport(w http.ResponseWriter, r *http.Request) {
	rp := s.reporter()
	if rp == nil {
		writeErr(w, http.StatusConflict, errors.New("no report targets configured (-report-to)"))
		return
	}
	from, to, period, err := s.reportWindow(r)
	if err != nil {
		storeErr(w, err)
		return
	}
	rep, err := s.BuildReport(r, from, to, period)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	res := rp.SendReport(r.Context(), rep.Title+" — "+rep.Period, rep.Text(s.loc()), rep.HTML(s.loc()), rep)
	s.audit(r, "report.send", rep.Period, "")
	writeJSON(w, http.StatusOK, res)
}
