package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/williamtatendajose/prediction/iot-hub/internal/auth"
)

const cookieName = "iothub_token"

type identityKey struct{}

// actor is who made the request, for the audit log.
func actor(r *http.Request) string {
	if id, ok := r.Context().Value(identityKey{}).(*auth.Identity); ok && id != nil {
		return id.ID
	}
	return "unknown"
}

// identity resolves the caller from "Authorization: Bearer <token>" or the
// login cookie. viaCookie reports which, for the CSRF check.
func (s *Server) identity(r *http.Request) (id *auth.Identity, viaCookie, ok bool) {
	if h := r.Header.Get("Authorization"); h != "" {
		tok, found := strings.CutPrefix(h, "Bearer ")
		if !found {
			return nil, false, false
		}
		id, ok = s.Auth.Authenticate(tok)
		return id, false, ok
	}
	if c, err := r.Cookie(cookieName); err == nil {
		id, ok = s.Auth.Authenticate(c.Value)
		return id, true, ok
	}
	id, ok = s.Auth.Authenticate("")
	return id, false, ok
}

// require wraps a handler with an authorisation check. For Ingest and
// Define the sensor is the {id} path value.
func (s *Server) require(a auth.Action, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a == auth.Read && s.PublicRead {
			id, _, ok := s.identity(r)
			if !ok {
				id = &auth.Identity{ID: "public", Role: auth.Viewer}
			}
			next(w, r.WithContext(context.WithValue(r.Context(), identityKey{}, id)))
			return
		}
		id, viaCookie, ok := s.identity(r)
		if !ok {
			writeErr(w, http.StatusUnauthorized, errors.New("missing or invalid token"))
			return
		}
		// A cookie is sent automatically by the browser, so a state change
		// must also carry a header that cross-site pages cannot set without
		// a CORS preflight (which this server never approves).
		if viaCookie && r.Method != http.MethodGet && r.Header.Get("X-Requested-With") != "iothub" {
			writeErr(w, http.StatusForbidden, errors.New("missing X-Requested-With header"))
			return
		}
		if !id.Can(a, r.PathValue("id")) {
			writeErr(w, http.StatusForbidden, errors.New(id.ID+" ("+string(id.Role)+") is not allowed to do this"))
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), identityKey{}, id)))
	}
}

// secureHeaders adds browser hardening to every response.
func secureHeaders(next http.Handler, tls bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		if tls {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

type loginReq struct {
	Token string `json:"token"`
}

// login validates a token and stores it in an HttpOnly cookie, so the
// dashboard (including EventSource, which cannot send headers) is
// authenticated without exposing the token to page scripts.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req loginReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	id, ok := s.Auth.Authenticate(req.Token)
	if !ok || !s.Auth.Enabled() {
		writeErr(w, http.StatusUnauthorized, errors.New("invalid token"))
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: req.Token, Path: "/", HttpOnly: true,
		Secure: s.SecureCookies, SameSite: http.SameSiteStrictMode,
		Expires: time.Now().Add(30 * 24 * time.Hour),
	})
	writeJSON(w, http.StatusOK, id)
}

func (s *Server) logout(w http.ResponseWriter, _ *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", HttpOnly: true,
		Secure: s.SecureCookies, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	w.WriteHeader(http.StatusNoContent)
}

// me reports who the caller is and whether auth is on (for the UI).
func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	id, _, ok := s.identity(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"authEnabled": true, "publicRead": s.PublicRead})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"authEnabled": s.Auth.Enabled(), "publicRead": s.PublicRead, "identity": id})
}

type deviceReq struct {
	ID      string    `json:"id"`
	Role    auth.Role `json:"role"`
	Sensors []string  `json:"sensors"`
	Note    string    `json:"note"`
}

func (s *Server) listDevices(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.Auth.List())
}

func (s *Server) addDevice(w http.ResponseWriter, r *http.Request) {
	if !s.Auth.Enabled() {
		writeErr(w, http.StatusConflict, errors.New("authentication is off: start the hub with -token (admin token) first"))
		return
	}
	var req deviceReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	token, err := s.Auth.Add(req.ID, req.Role, req.Sensors, req.Note)
	switch {
	case errors.Is(err, auth.ErrExists):
		writeErr(w, http.StatusConflict, err)
		return
	case errors.Is(err, auth.ErrInvalid):
		writeErr(w, http.StatusBadRequest, err)
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.audit(r, "device.add", req.ID, fmt.Sprintf("role=%s sensors=%v", req.Role, req.Sensors))
	// The only time the token is ever shown.
	writeJSON(w, http.StatusCreated, map[string]any{"id": req.ID, "role": req.Role, "sensors": req.Sensors, "token": token})
}

func (s *Server) deleteDevice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.Auth.Remove(id); err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if s.OnRevoke != nil {
		s.OnRevoke(id) // drop live MQTT sessions
	}
	s.audit(r, "device.revoke", id, "")
	w.WriteHeader(http.StatusNoContent)
}
