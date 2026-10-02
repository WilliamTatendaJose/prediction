package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

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
