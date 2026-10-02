package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
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
		if strings.HasPrefix(h, auth.SASPrefix) { // Azure IoT Hub style
			id, ok = s.Auth.Authenticate(h)
			return id, false, ok
		}
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

// canIssue reports whether the caller may create or change credentials.
// In a SaaS deployment the platform operator may keep that to itself: a
// tenant admin then sees its devices but can't mint access.
func (s *Server) canIssue(w http.ResponseWriter, r *http.Request) bool {
	id, _ := r.Context().Value(identityKey{}).(*auth.Identity)
	if id != nil && id.Role != auth.Superadmin && s.SelfService != nil && !s.SelfService() {
		writeErr(w, http.StatusForbidden, errors.New("device credentials for this tenant are managed by the platform operator"))
		return false
	}
	return true
}

func (s *Server) tenantID() string {
	if t := s.Auth.TenantID(); t != "" {
		return t
	}
	return auth.DefaultTenant
}

// connectionString is the Azure IoT Hub-style string a device is
// configured with.
func (s *Server) connectionString(r *http.Request, id, key string) string {
	host := r.Host
	if u, err := url.Parse(s.PublicURL); err == nil && u.Host != "" {
		host = u.Host
	}
	return "HostName=" + host + ";TenantId=" + s.tenantID() + ";DeviceId=" + id + ";SharedAccessKey=" + key
}

func authErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, auth.ErrExists):
		writeErr(w, http.StatusConflict, err)
	case errors.Is(err, auth.ErrNotFound):
		writeErr(w, http.StatusNotFound, err)
	case errors.Is(err, auth.ErrInvalid):
		writeErr(w, http.StatusBadRequest, err)
	default:
		writeErr(w, http.StatusInternalServerError, err)
	}
}

func (s *Server) listDevices(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.Auth.List())
}

// addDevice: POST /api/devices {id, role, sensors, note, auth: token|keys|both, expires}
func (s *Server) addDevice(w http.ResponseWriter, r *http.Request) {
	if !s.canIssue(w, r) {
		return
	}
	if !s.Auth.Enabled() {
		writeErr(w, http.StatusConflict, errors.New("authentication is off: start the hub with -token (admin token) first"))
		return
	}
	var req auth.Spec
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	sec, err := s.Auth.Create(req)
	if err != nil {
		authErr(w, err)
		return
	}
	s.audit(r, "device.add", req.ID, fmt.Sprintf("role=%s sensors=%v auth=%s", req.Role, req.Sensors, req.Auth))
	// The only time secrets are shown unasked.
	out := map[string]any{"id": req.ID, "role": req.Role, "sensors": req.Sensors, "tenant": s.tenantID()}
	if sec.Token != "" {
		out["token"] = sec.Token
	}
	if sec.PrimaryKey != "" {
		out["primaryKey"], out["secondaryKey"] = sec.PrimaryKey, sec.SecondaryKey
		out["connectionString"] = s.connectionString(r, req.ID, sec.PrimaryKey)
	}
	writeJSON(w, http.StatusCreated, out)
}

// updateDevice: PATCH /api/devices/{id} {sensors, note, disabled, expires}
func (s *Server) updateDevice(w http.ResponseWriter, r *http.Request) {
	if !s.canIssue(w, r) {
		return
	}
	var u auth.Update
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&u); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	id := r.PathValue("id")
	if err := s.Auth.Update(id, u); err != nil {
		authErr(w, err)
		return
	}
	// Live sessions keep the permissions they connected with: drop them so
	// a disable or a narrower sensor list applies at once.
	if s.OnRevoke != nil && (u.Disabled != nil && *u.Disabled || u.Sensors != nil || u.Expires != nil) {
		s.OnRevoke(id)
	}
	b, _ := json.Marshal(u)
	s.audit(r, "device.update", id, string(b))
	w.WriteHeader(http.StatusNoContent)
}

// rotateDevice: POST /api/devices/{id}/rotate {"which": "token"|"primary"|"secondary"}
func (s *Server) rotateDevice(w http.ResponseWriter, r *http.Request) {
	if !s.canIssue(w, r) {
		return
	}
	var req struct {
		Which string `json:"which"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	id := r.PathValue("id")
	sec, err := s.Auth.Rotate(id, req.Which)
	if err != nil {
		authErr(w, err)
		return
	}
	if s.OnRevoke != nil {
		s.OnRevoke(id) // rotation is often a response to a leak: end sessions now
	}
	s.audit(r, "device.rotate", id, req.Which)
	out := map[string]any{"id": id}
	if sec.Token != "" {
		out["token"] = sec.Token
	}
	if sec.PrimaryKey != "" {
		out["primaryKey"] = sec.PrimaryKey
		out["connectionString"] = s.connectionString(r, id, sec.PrimaryKey)
	}
	if sec.SecondaryKey != "" {
		out["secondaryKey"] = sec.SecondaryKey
	}
	writeJSON(w, http.StatusOK, out)
}

// deviceKeys: GET /api/devices/{id}/keys (audited)
func (s *Server) deviceKeys(w http.ResponseWriter, r *http.Request) {
	if !s.canIssue(w, r) {
		return
	}
	id := r.PathValue("id")
	k, err := s.Auth.KeysOf(id)
	if err != nil {
		authErr(w, err)
		return
	}
	s.audit(r, "device.keys.view", id, "")
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "primaryKey": k.PrimaryKey, "secondaryKey": k.SecondaryKey,
		"connectionString":          s.connectionString(r, id, k.PrimaryKey),
		"secondaryConnectionString": s.connectionString(r, id, k.SecondaryKey)})
}

// deviceSAS: POST /api/devices/{id}/sas {"ttl": "24h"} issues a SAS token
// from the primary key, for devices that can't sign their own.
func (s *Server) deviceSAS(w http.ResponseWriter, r *http.Request) {
	if !s.canIssue(w, r) {
		return
	}
	var req struct {
		TTL string `json:"ttl"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ttl := 24 * time.Hour
	if req.TTL != "" {
		d, err := parseDuration(req.TTL)
		if err != nil || d < time.Minute || d > auth.MaxSASLifetime {
			writeErr(w, http.StatusBadRequest, errors.New("ttl must be between 1m and 366d"))
			return
		}
		ttl = d
	}
	id := r.PathValue("id")
	k, err := s.Auth.KeysOf(id)
	if err != nil {
		authErr(w, err)
		return
	}
	exp := time.Now().Add(ttl)
	tok, err := auth.SignSAS(auth.ResourceURI(s.tenantID(), id), k.PrimaryKey, exp)
	if err != nil {
		authErr(w, err)
		return
	}
	s.audit(r, "device.sas", id, "ttl="+ttl.String())
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "sas": tok, "expires": exp.UnixMilli()})
}

func (s *Server) deleteDevice(w http.ResponseWriter, r *http.Request) {
	if !s.canIssue(w, r) {
		return
	}
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
