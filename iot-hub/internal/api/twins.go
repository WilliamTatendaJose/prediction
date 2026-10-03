package api

import (
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/williamtatendajose/prediction/iot-hub/internal/auth"
	"github.com/williamtatendajose/prediction/iot-hub/internal/twin"
)

func twinErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, twin.ErrPrecondition):
		writeErr(w, http.StatusPreconditionFailed, err)
	case errors.Is(err, twin.ErrOffline):
		writeErr(w, http.StatusNotFound, err) // as Azure: the device isn't there to answer
	case errors.Is(err, twin.ErrTimeout):
		writeErr(w, http.StatusGatewayTimeout, err)
	case errors.Is(err, twin.ErrQueueFull):
		writeErr(w, http.StatusForbidden, err)
	default:
		storeErr(w, err)
	}
}

func (s *Server) twinsOn(w http.ResponseWriter) bool {
	if s.Twins == nil {
		writeErr(w, http.StatusNotFound, errors.New("device twins are not available"))
		return false
	}
	return true
}

func caller(r *http.Request) *auth.Identity {
	id, _ := r.Context().Value(identityKey{}).(*auth.Identity)
	return id
}

// twinIDs: the devices (and services) the caller may control.
func (s *Server) twinIDs(r *http.Request) []string {
	id := caller(r)
	var out []string
	for _, d := range s.Auth.List() {
		if (d.Role == auth.Device || d.Role == auth.Service) && id.Can(auth.Control, d.ID) {
			out = append(out, d.ID)
		}
	}
	return out
}

// listTwins: GET /api/twins?where=tags.site=plant1&where=properties.reported.firmware=1.4
func (s *Server) listTwins(w http.ResponseWriter, r *http.Request) {
	if !s.twinsOn(w) {
		return
	}
	fs, err := twin.ParseFilters(r.URL.Query()["where"])
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.Twins.Query(s.twinIDs(r), fs))
}

func (s *Server) getTwin(w http.ResponseWriter, r *http.Request) {
	if !s.twinsOn(w) {
		return
	}
	v, err := s.Twins.Get(r.PathValue("id"))
	if err != nil {
		twinErr(w, err)
		return
	}
	w.Header().Set("ETag", v.ETag)
	writeJSON(w, http.StatusOK, v)
}

// patchTwin: PATCH /api/twins/{id} {"tags":{…},"properties":{"desired":{…}}}, If-Match optional
func (s *Server) patchTwin(w http.ResponseWriter, r *http.Request) {
	if !s.twinsOn(w) {
		return
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	id := r.PathValue("id")
	v, err := s.Twins.Update(id, r.Header.Get("If-Match"), b)
	if err != nil {
		twinErr(w, err)
		return
	}
	s.audit(r, "twin.update", id, string(b))
	w.Header().Set("ETag", v.ETag)
	writeJSON(w, http.StatusOK, v)
}

// patchTwins: PATCH /api/twins?where=… applies one patch to every match.
func (s *Server) patchTwins(w http.ResponseWriter, r *http.Request) {
	if !s.twinsOn(w) {
		return
	}
	fs, err := twin.ParseFilters(r.URL.Query()["where"])
	if err != nil {
		storeErr(w, err)
		return
	}
	if len(fs) == 0 {
		writeErr(w, http.StatusBadRequest, errors.New("give at least one where= filter (use where=deviceId=… for one device)"))
		return
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	changed, err := s.Twins.UpdateMany(s.twinIDs(r), fs, b)
	if changed == nil {
		changed = []string{}
	}
	if len(changed) > 0 {
		s.audit(r, "twin.update-many", r.URL.RawQuery, string(b))
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "updated": changed})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"updated": changed})
}

// invokeMethod: POST /api/devices/{id}/methods/{method}?timeout=30s, body = JSON payload
func (s *Server) invokeMethod(w http.ResponseWriter, r *http.Request) {
	if !s.twinsOn(w) {
		return
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, twin.MaxPayload))
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, err)
		return
	}
	timeout := 30 * time.Second
	if t := r.URL.Query().Get("timeout"); t != "" {
		if timeout, err = time.ParseDuration(t); err != nil || timeout < time.Second {
			writeErr(w, http.StatusBadRequest, errors.New("timeout: a duration from 1s to 5m"))
			return
		}
	}
	id, name := r.PathValue("id"), r.PathValue("method")
	res, err := s.Twins.Invoke(r.Context(), id, name, b, timeout)
	s.audit(r, "method.invoke", id, name+" "+errText(err))
	if err != nil {
		twinErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func errText(err error) string {
	if err == nil {
		return "ok"
	}
	return err.Error()
}

// sendMessage: POST /api/devices/{id}/messages?ttl=1h, body = message
func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request) {
	if !s.twinsOn(w) {
		return
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, twin.MaxMessageBody))
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, err)
		return
	}
	var ttl time.Duration
	if t := r.URL.Query().Get("ttl"); t != "" {
		if ttl, err = time.ParseDuration(t); err != nil {
			writeErr(w, http.StatusBadRequest, errors.New("ttl: a duration up to 48h"))
			return
		}
	}
	id := r.PathValue("id")
	m, err := s.Twins.Send(id, b, ttl)
	if err != nil {
		twinErr(w, err)
		return
	}
	s.audit(r, "message.send", id, m.ID)
	writeJSON(w, http.StatusAccepted, m)
}

func (s *Server) listMessages(w http.ResponseWriter, r *http.Request) {
	if !s.twinsOn(w) {
		return
	}
	ms, err := s.Twins.Messages(r.PathValue("id"))
	if err != nil {
		twinErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ms)
}

// ---- the device's own side, over HTTP --------------------------------------------

// self returns the calling device's id, or answers 403.
func (s *Server) self(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := caller(r)
	if id == nil || id.Role != auth.Device && id.Role != auth.Service {
		writeErr(w, http.StatusForbidden, errors.New("only devices and services have a twin"))
		return "", false
	}
	if !s.twinsOn(w) {
		return "", false
	}
	s.Twins.Touch(id.ID)
	return id.ID, true
}

// deviceTwin: GET /api/device/twin (desired and reported)
func (s *Server) deviceTwin(w http.ResponseWriter, r *http.Request) {
	if id, ok := s.self(w, r); ok {
		writeJSON(w, http.StatusOK, s.Twins.DeviceView(id))
	}
}

// deviceReport: PATCH /api/device/twin/reported {patch}
func (s *Server) deviceReport(w http.ResponseWriter, r *http.Request) {
	id, ok := s.self(w, r)
	if !ok {
		return
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, twin.MaxReported))
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, err)
		return
	}
	v, err := s.Twins.Report(id, b)
	if err != nil {
		twinErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"version": v})
}

// deviceReceive: GET /api/device/messages → the next message (locked for
// a minute), or 204.
func (s *Server) deviceReceive(w http.ResponseWriter, r *http.Request) {
	id, ok := s.self(w, r)
	if !ok {
		return
	}
	m, err := s.Twins.Receive(id)
	if err != nil {
		twinErr(w, err)
		return
	}
	if m == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// deviceSettle: POST /api/device/messages/{mid}/{action} (complete|reject|abandon)
func (s *Server) deviceSettle(w http.ResponseWriter, r *http.Request) {
	id, ok := s.self(w, r)
	if !ok {
		return
	}
	if err := s.Twins.Settle(id, r.PathValue("mid"), r.PathValue("action")); err != nil {
		twinErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
