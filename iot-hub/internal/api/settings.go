package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/williamtatendajose/prediction/iot-hub/internal/calc"
	"github.com/williamtatendajose/prediction/iot-hub/internal/store"
)

// SettingsStore is a tenant's settings (implemented by the tenant runtime).
type SettingsStore interface {
	View() any
	Update(raw []byte) error
	AddTarget(id, spec string) error
	RemoveTarget(id string) error
	TestTarget(ctx context.Context, id string) error
}

func (s *Server) settingsOr404(w http.ResponseWriter) bool {
	if s.Settings == nil {
		writeErr(w, http.StatusNotFound, errors.New("settings are not available"))
		return false
	}
	return true
}

// getSettings: GET /api/settings (target URLs are never shown)
func (s *Server) getSettings(w http.ResponseWriter, _ *http.Request) {
	if !s.settingsOr404(w) {
		return
	}
	out := map[string]any{"settings": s.Settings.View()}
	if s.Escalations != nil {
		out["recentEscalations"] = s.Escalations()
	}
	writeJSON(w, http.StatusOK, out)
}

// putSettings: PUT /api/settings {notify, escalation, reports, shifts, timeZone, webhookSecret}
func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	if !s.settingsOr404(w) {
		return
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxConfigBody))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.Settings.Update(b); err != nil {
		storeErr(w, err)
		return
	}
	var redacted map[string]any
	if json.Unmarshal(b, &redacted) == nil {
		if _, ok := redacted["webhookSecret"]; ok {
			redacted["webhookSecret"] = "(changed)"
		}
		b, _ = json.Marshal(redacted)
	}
	s.audit(r, "settings.update", "settings", string(b))
	writeJSON(w, http.StatusOK, s.Settings.View())
}

// putTarget: PUT /api/settings/targets/{target} {"spec":"slack=https://..."}
func (s *Server) putTarget(w http.ResponseWriter, r *http.Request) {
	if !s.settingsOr404(w) {
		return
	}
	var req struct {
		Spec string `json:"spec"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	id := r.PathValue("target")
	if err := s.Settings.AddTarget(id, req.Spec); err != nil {
		storeErr(w, err)
		return
	}
	s.audit(r, "settings.target", id, "set")
	writeJSON(w, http.StatusOK, s.Settings.View())
}

func (s *Server) deleteTarget(w http.ResponseWriter, r *http.Request) {
	if !s.settingsOr404(w) {
		return
	}
	id := r.PathValue("target")
	if err := s.Settings.RemoveTarget(id); err != nil {
		storeErr(w, err)
		return
	}
	s.audit(r, "settings.target", id, "removed")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) testTarget(w http.ResponseWriter, r *http.Request) {
	if !s.settingsOr404(w) {
		return
	}
	if err := s.Settings.TestTarget(r.Context(), r.PathValue("target")); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			storeErr(w, err)
			return
		}
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"result": "sent"})
}

// JobsStore is a tenant's stream jobs (implemented by the tenant runtime).
type JobsStore interface {
	List() any
	Put(id string, raw []byte) error
	Delete(id string) error
	Test(ctx context.Context, raw []byte, from, to int64) (any, error)
}

func (s *Server) listJobs(w http.ResponseWriter, _ *http.Request) {
	if s.Jobs == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	writeJSON(w, http.StatusOK, s.Jobs.List())
}

// putJob: PUT /api/jobs/{job} {"query": "...", "enabled": true, "lateness": "5s"}
func (s *Server) putJob(w http.ResponseWriter, r *http.Request) {
	if s.Jobs == nil {
		writeErr(w, http.StatusNotFound, errors.New("stream jobs are not available"))
		return
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<10))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	id := r.PathValue("job")
	if err := s.Jobs.Put(id, b); err != nil {
		storeErr(w, err)
		return
	}
	s.audit(r, "job.put", id, string(b))
	writeJSON(w, http.StatusOK, s.Jobs.List())
}

func (s *Server) deleteJob(w http.ResponseWriter, r *http.Request) {
	if s.Jobs == nil {
		writeErr(w, http.StatusNotFound, errors.New("stream jobs are not available"))
		return
	}
	id := r.PathValue("job")
	if err := s.Jobs.Delete(id); err != nil {
		storeErr(w, err)
		return
	}
	s.audit(r, "job.delete", id, "")
	w.WriteHeader(http.StatusNoContent)
}

// testJob: POST /api/jobs/test?from=-6h&to=now {"query": "..."} replays
// stored history through the query and returns what it would output.
func (s *Server) testJob(w http.ResponseWriter, r *http.Request) {
	if s.Jobs == nil {
		writeErr(w, http.StatusNotFound, errors.New("stream jobs are not available"))
		return
	}
	from, to, err := rangeParams(r, 6*time.Hour, time.Second)
	if err != nil {
		storeErr(w, err)
		return
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<10))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	res, err := s.Jobs.Test(r.Context(), b, from, to)
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// calcTest: POST /api/calc/test {"sensor": "meter", "formula": "voltage * current / 1000"}
// evaluates a formula against the sensor's latest values, so the editor
// can show the result before saving.
func (s *Server) calcTest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Sensor  string `json:"sensor"`
		Formula string `json:"formula"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	e, err := calc.Parse(req.Formula)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"error": err.Error()})
		return
	}
	sv, err := s.Store.Get(req.Sensor)
	if err != nil {
		storeErr(w, err)
		return
	}
	v, err := e.Eval(func(f string) (float64, bool) {
		switch x := sv.Last[f].(type) {
		case float64:
			return x, true
		case bool:
			return map[bool]float64{true: 1}[x], true
		}
		return 0, false
	})
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"error": err.Error(), "uses": e.Fields()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"value": v, "uses": e.Fields()})
}
