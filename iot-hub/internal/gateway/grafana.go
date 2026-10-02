package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/williamtatendajose/prediction/iot-hub/internal/auth"
	"github.com/williamtatendajose/prediction/iot-hub/internal/grafana"
	"github.com/williamtatendajose/prediction/iot-hub/internal/tenant"
)

func grafanaErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, tenant.ErrNoGrafana):
		writeErr(w, http.StatusNotFound, err)
	case errors.Is(err, tenant.ErrNotFound), grafana.NotFound(err):
		writeErr(w, http.StatusNotFound, err)
	case errors.Is(err, grafana.ErrInvalid):
		writeErr(w, http.StatusBadRequest, err)
	default:
		writeErr(w, http.StatusBadGateway, err)
	}
}

// provisionGrafana: POST /api/admin/tenants/{tenant}/grafana creates or
// repairs the tenant's Grafana (rotating its database password).
func (g *Gateway) provisionGrafana(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	id := r.PathValue("tenant")
	res, err := g.Platform.ProvisionGrafana(ctx, id)
	if err != nil {
		grafanaErr(w, err)
		return
	}
	g.Logger.Info("grafana provisioned", "tenant", id, "org", res.OrgID)
	writeJSON(w, http.StatusOK, res)
}

func (g *Gateway) deprovisionGrafana(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	id := r.PathValue("tenant")
	if err := g.Platform.DeprovisionGrafana(ctx, id); err != nil {
		grafanaErr(w, err)
		return
	}
	g.Logger.Warn("grafana removed", "tenant", id)
	w.WriteHeader(http.StatusNoContent)
}

type tenantHandler func(w http.ResponseWriter, r *http.Request, id *auth.Identity, t string)

// tenantAdmin: the tenant's admins (and the superadmin with X-Tenant).
func (g *Gateway) tenantAdmin(next tenantHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, _, rt, ok := g.resolve(w, r)
		if !ok {
			return
		}
		if !id.Can(auth.Manage, "") {
			writeErr(w, http.StatusForbidden, errors.New("Grafana access is managed by the tenant's admins"))
			return
		}
		if g.Platform.Grafana == nil {
			grafanaErr(w, tenant.ErrNoGrafana)
			return
		}
		next(w, r, id, rt.ID)
	}
}

// grafanaInfo: GET /api/grafana → link and users.
func (g *Gateway) grafanaInfo(w http.ResponseWriter, r *http.Request, _ *auth.Identity, t string) {
	in, _ := g.Platform.Info(t)
	out := map[string]any{"grafana": in.Grafana}
	if in.Grafana != nil && in.Grafana.OrgID > 0 {
		users, err := g.Platform.Grafana.Users(r.Context(), t)
		if err != nil {
			grafanaErr(w, err)
			return
		}
		out["users"] = users
	}
	writeJSON(w, http.StatusOK, out)
}

// grafanaSetUser: PUT /api/grafana/users/{login} {"role":"Viewer|Editor","email":""}
// creates the Grafana user {tenant}.{login} (the password is shown once)
// or changes its role.
func (g *Gateway) grafanaSetUser(w http.ResponseWriter, r *http.Request, id *auth.Identity, t string) {
	var req struct {
		Role  string `json:"role"`
		Email string `json:"email"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	login := r.PathValue("login")
	pw, err := g.Platform.Grafana.SetUser(r.Context(), t, login, req.Email, req.Role)
	if err != nil {
		grafanaErr(w, err)
		return
	}
	g.Logger.Info("grafana user set", "tenant", t, "login", login, "role", req.Role, "by", id.ID)
	in, _ := g.Platform.Info(t)
	out := map[string]any{"login": t + "." + login, "role": req.Role}
	if pw != "" {
		out["password"] = pw // the only time it is shown
	}
	if in.Grafana != nil {
		out["url"] = in.Grafana.URL
	}
	writeJSON(w, http.StatusOK, out)
}

func (g *Gateway) grafanaDeleteUser(w http.ResponseWriter, r *http.Request, id *auth.Identity, t string) {
	login := r.PathValue("login")
	if err := g.Platform.Grafana.DeleteUser(r.Context(), t, login); err != nil {
		grafanaErr(w, err)
		return
	}
	g.Logger.Info("grafana user removed", "tenant", t, "login", login, "by", id.ID)
	w.WriteHeader(http.StatusNoContent)
}
