// Package gateway is the multi-tenant front door: it authenticates every
// request once, resolves which tenant it is for, and hands it to that
// tenant's API server. Tenant users can only ever reach their own tenant;
// the superadmin picks one with the X-Tenant header (or ?tenant= for
// EventSource, which can't send headers) and manages tenants under
// /api/admin.
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/williamtatendajose/prediction/iot-hub/internal/api"
	"github.com/williamtatendajose/prediction/iot-hub/internal/auth"
	"github.com/williamtatendajose/prediction/iot-hub/internal/tenant"
)

type Gateway struct {
	Platform      *tenant.Platform
	Creds         *auth.Store // platform view
	Static        http.Handler
	SecureCookies bool
	TLS           bool
	Logger        *slog.Logger
	started       time.Time
}

func (g *Gateway) Handler() http.Handler {
	g.started = time.Now()
	if g.Logger == nil {
		g.Logger = slog.Default()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", g.health)
	mux.HandleFunc("POST /api/login", g.login)
	mux.HandleFunc("POST /api/logout", g.logout)
	mux.HandleFunc("GET /api/me", g.me)
	mux.HandleFunc("GET /api/admin/tenants", g.super(g.listTenants))
	mux.HandleFunc("POST /api/admin/tenants", g.super(g.createTenant))
	mux.HandleFunc("GET /api/admin/tenants/{tenant}", g.super(g.getTenant))
	mux.HandleFunc("PATCH /api/admin/tenants/{tenant}", g.super(g.patchTenant))
	mux.HandleFunc("DELETE /api/admin/tenants/{tenant}", g.super(g.deleteTenant))
	mux.Handle("/api/admin/", http.NotFoundHandler())
	mux.HandleFunc("/api/", g.dispatch)
	if g.Static != nil {
		// "/" without a method: "GET /" would conflict with "/api/admin/".
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				w.Header().Set("Allow", "GET, HEAD")
				writeErr(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
				return
			}
			g.Static.ServeHTTP(w, r)
		})
	}
	return api.SecureHeaders(mux, g.TLS)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// authenticate reads a bearer token, an Azure-style SAS header or the
// login cookie.
func (g *Gateway) authenticate(r *http.Request) (*auth.Identity, bool, bool) {
	if h := r.Header.Get("Authorization"); h != "" {
		if strings.HasPrefix(h, auth.SASPrefix) {
			id, ok := g.Creds.Authenticate(h)
			return id, false, ok
		}
		tok, found := strings.CutPrefix(h, "Bearer ")
		if !found {
			return nil, false, false
		}
		id, ok := g.Creds.Authenticate(tok)
		return id, false, ok
	}
	if c, err := r.Cookie(api.CookieName); err == nil {
		id, ok := g.Creds.Authenticate(c.Value)
		return id, true, ok
	}
	return nil, false, false
}

// csrf: a cookie is sent by the browser automatically, so a state change
// must also carry a header cross-site pages can't set.
func csrf(w http.ResponseWriter, r *http.Request, viaCookie bool) bool {
	if viaCookie && r.Method != http.MethodGet && r.Header.Get("X-Requested-With") != "iothub" {
		writeErr(w, http.StatusForbidden, errors.New("missing X-Requested-With header"))
		return false
	}
	return true
}

func (g *Gateway) dispatch(w http.ResponseWriter, r *http.Request) {
	id, viaCookie, ok := g.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, errors.New("missing or invalid token"))
		return
	}
	if !csrf(w, r, viaCookie) {
		return
	}
	t := r.Header.Get("X-Tenant")
	if t == "" {
		t = r.URL.Query().Get("tenant")
	}
	switch {
	case id.Role == auth.Superadmin:
		if t == "" {
			writeErr(w, http.StatusBadRequest, errors.New("superadmin: choose a tenant with the X-Tenant header or ?tenant="))
			return
		}
	case t != "" && t != id.Tenant:
		writeErr(w, http.StatusForbidden, errors.New("this credential belongs to another tenant"))
		return
	default:
		t = id.Tenant
	}
	rt, err := g.Platform.Runtime(t)
	switch {
	case errors.Is(err, tenant.ErrNotFound):
		writeErr(w, http.StatusNotFound, err)
		return
	case errors.Is(err, tenant.ErrSuspended):
		writeErr(w, http.StatusForbidden, err)
		return
	case err != nil || rt == nil:
		writeErr(w, http.StatusServiceUnavailable, errors.New("tenant is not running"))
		return
	}
	rt.Handler.ServeHTTP(w, api.WithIdentity(r, id, viaCookie))
}

func (g *Gateway) health(w http.ResponseWriter, _ *http.Request) {
	// Unauthenticated: nothing about tenants.
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "mode": "multi-tenant",
		"uptimeSec": int(time.Since(g.started).Seconds())})
}

func (g *Gateway) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	id, ok := g.Creds.Authenticate(req.Token)
	if !ok {
		writeErr(w, http.StatusUnauthorized, errors.New("invalid token"))
		return
	}
	if id.Role != auth.Superadmin {
		if _, err := g.Platform.Runtime(id.Tenant); err != nil {
			writeErr(w, http.StatusForbidden, err)
			return
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name: api.CookieName, Value: req.Token, Path: "/", HttpOnly: true,
		Secure: g.SecureCookies, SameSite: http.SameSiteStrictMode,
		Expires: time.Now().Add(30 * 24 * time.Hour),
	})
	writeJSON(w, http.StatusOK, id)
}

func (g *Gateway) logout(w http.ResponseWriter, _ *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: api.CookieName, Value: "", Path: "/", HttpOnly: true,
		Secure: g.SecureCookies, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	w.WriteHeader(http.StatusNoContent)
}

type tenantRef struct {
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	Status string `json:"status"`
}

func (g *Gateway) me(w http.ResponseWriter, r *http.Request) {
	id, _, ok := g.authenticate(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"authEnabled": true, "multiTenant": true})
		return
	}
	out := map[string]any{"authEnabled": true, "multiTenant": true, "identity": id}
	if id.Role == auth.Superadmin {
		var ts []tenantRef
		for _, u := range g.Platform.List() {
			ts = append(ts, tenantRef{u.ID, u.Name, u.Status})
		}
		out["tenants"] = ts
	} else if in, ok := g.Platform.Info(id.Tenant); ok {
		out["tenant"] = tenantRef{in.ID, in.Name, in.Status}
		out["deviceSelfService"] = in.DeviceSelfService
	}
	writeJSON(w, http.StatusOK, out)
}

// super allows only the superadmin.
func (g *Gateway) super(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, viaCookie, ok := g.authenticate(r)
		if !ok {
			writeErr(w, http.StatusUnauthorized, errors.New("missing or invalid token"))
			return
		}
		if !id.Can(auth.Platform, "") {
			writeErr(w, http.StatusForbidden, errors.New("platform administration is for the superadmin"))
			return
		}
		if !csrf(w, r, viaCookie) {
			return
		}
		next(w, r)
	}
}

func tenantErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, tenant.ErrNotFound):
		writeErr(w, http.StatusNotFound, err)
	case errors.Is(err, tenant.ErrExists):
		writeErr(w, http.StatusConflict, err)
	default:
		writeErr(w, http.StatusBadRequest, err)
	}
}

func (g *Gateway) listTenants(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, g.Platform.List())
}

func (g *Gateway) getTenant(w http.ResponseWriter, r *http.Request) {
	u, err := g.Platform.Get(r.PathValue("tenant"))
	if err != nil {
		tenantErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (g *Gateway) createTenant(w http.ResponseWriter, r *http.Request) {
	var in tenant.Info
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	in, err := g.Platform.Create(in)
	if err != nil {
		tenantErr(w, err)
		return
	}
	g.Logger.Info("tenant created", "tenant", in.ID)
	u, _ := g.Platform.Get(in.ID)
	writeJSON(w, http.StatusCreated, u)
}

func (g *Gateway) patchTenant(w http.ResponseWriter, r *http.Request) {
	var p tenant.Patch
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	id := r.PathValue("tenant")
	if _, err := g.Platform.Update(id, p); err != nil {
		tenantErr(w, err)
		return
	}
	b, _ := json.Marshal(p)
	g.Logger.Info("tenant updated", "tenant", id, "patch", string(b))
	u, _ := g.Platform.Get(id)
	writeJSON(w, http.StatusOK, u)
}

// deleteTenant: DELETE /api/admin/tenants/{tenant}?confirm={tenant}
// removes the tenant with all its data, credentials and backups.
func (g *Gateway) deleteTenant(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("tenant")
	if r.URL.Query().Get("confirm") != id {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("deleting erases all of %s's data: repeat its id as ?confirm=%s", id, id))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := g.Platform.Delete(ctx, id); err != nil {
		tenantErr(w, err)
		return
	}
	g.Logger.Warn("tenant deleted", "tenant", id)
	w.WriteHeader(http.StatusNoContent)
}
