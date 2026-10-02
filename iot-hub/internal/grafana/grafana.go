// Package grafana gives each tenant its own Grafana: an organization with
// a PostgreSQL data source, the IoT Hub dashboards and the tenant's users.
//
// Isolation is enforced by the database, not by Grafana: every tenant's
// data source logs in as its own PostgreSQL role, which may only read that
// tenant's schema (search_path set, read-only transactions, a statement
// timeout and a connection limit). Grafana editors can write any SQL; the
// role makes that harmless to other tenants.
//
// Grafana users are global, so tenant users are namespaced
// "{tenant}.{login}" and are members of their tenant's organization only
// (never the main organization, which belongs to the platform). Tenants
// may grant Viewer or Editor; organization Admin stays with the platform,
// because an org admin can edit data sources and aim them anywhere.
package grafana

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers "pgx"
)

//go:embed dashboards/iothub-overview.json
var overview []byte

// Overview is the embedded overview dashboard (also in deploy/).
func Overview() []byte { return overview }

type Config struct {
	URL      string // Grafana as the hub reaches it, e.g. http://grafana:3000
	User     string // Grafana server admin (basic auth)
	Password string
	// PublicURL is how people reach Grafana (links); default URL.
	PublicURL string
	// DBAdminURL connects with a role that may create roles and grant on
	// the tenant schemas (the hub's own role with CREATEROLE).
	DBAdminURL string
	// DBHost is host:port of PostgreSQL as Grafana reaches it.
	DBHost  string
	SSLMode string // default disable
	Client  *http.Client
}

type Provisioner struct{ cfg Config }

func New(cfg Config) (*Provisioner, error) {
	if cfg.URL == "" || cfg.User == "" || cfg.DBAdminURL == "" || cfg.DBHost == "" {
		return nil, errors.New("grafana: URL, admin user, database admin URL and database host are required")
	}
	cfg.URL = strings.TrimRight(cfg.URL, "/")
	if cfg.PublicURL == "" {
		cfg.PublicURL = cfg.URL
	}
	cfg.PublicURL = strings.TrimRight(cfg.PublicURL, "/")
	if cfg.SSLMode == "" {
		cfg.SSLMode = "disable"
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 20 * time.Second}
	}
	return &Provisioner{cfg: cfg}, nil
}

// Result describes a tenant's Grafana.
type Result struct {
	OrgID       int64  `json:"orgId"`
	Org         string `json:"org"`
	URL         string `json:"url"`
	DBRole      string `json:"dbRole"`
	Provisioned int64  `json:"provisioned"`
	Error       string `json:"error,omitempty"`
}

var identRe = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

func orgName(tenant string) string { return "iothub-" + tenant }

// Role is the PostgreSQL role a tenant's data source uses.
func Role(schema string) string { return "grafana_" + schema }

func secret() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ---- Grafana HTTP API -----------------------------------------------------------

type apiError struct {
	Status int
	Msg    string
}

func (e *apiError) Error() string { return fmt.Sprintf("grafana: HTTP %d: %s", e.Status, e.Msg) }

func (p *Provisioner) call(ctx context.Context, method, path string, org int64, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.cfg.URL+path, body)
	if err != nil {
		return err
	}
	req.SetBasicAuth(p.cfg.User, p.cfg.Password)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if org > 0 {
		req.Header.Set("X-Grafana-Org-Id", strconv.FormatInt(org, 10))
	}
	res, err := p.cfg.Client.Do(req)
	if err != nil {
		return fmt.Errorf("grafana: %w", err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if res.StatusCode >= 300 {
		var m struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(b, &m)
		if m.Message == "" {
			m.Message = strings.TrimSpace(string(b))
		}
		return &apiError{res.StatusCode, m.Message}
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

func notFound(err error) bool {
	var e *apiError
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

// org finds (and optionally creates) a tenant's organization.
func (p *Provisioner) org(ctx context.Context, tenant string, create bool) (int64, error) {
	var o struct {
		ID int64 `json:"id"`
	}
	err := p.call(ctx, "GET", "/api/orgs/name/"+url.PathEscape(orgName(tenant)), 0, nil, &o)
	if err == nil {
		return o.ID, nil
	}
	if !notFound(err) || !create {
		return 0, err
	}
	var c struct {
		OrgID int64 `json:"orgId"`
	}
	if err := p.call(ctx, "POST", "/api/orgs", 0, map[string]string{"name": orgName(tenant)}, &c); err != nil {
		return 0, err
	}
	return c.OrgID, nil
}

// ---- PostgreSQL role ------------------------------------------------------------

func (p *Provisioner) db(ctx context.Context) (*sql.DB, error) {
	db, err := sql.Open("pgx", p.cfg.DBAdminURL)
	if err != nil {
		return nil, err
	}
	return db, db.PingContext(ctx)
}

// role creates or updates a tenant's read-only role and returns its new
// password (rotated on every provisioning) and the database name.
func (p *Provisioner) role(ctx context.Context, schema string) (string, string, error) {
	if !identRe.MatchString(schema) {
		return "", "", fmt.Errorf("bad schema %q", schema)
	}
	role := Role(schema)
	pw := secret() // hex: safe to inline in DDL, which takes no parameters
	db, err := p.db(ctx)
	if err != nil {
		return "", "", err
	}
	defer db.Close()
	var dbname, owner string
	if err := db.QueryRowContext(ctx, `SELECT current_database(), (SELECT nspowner::regrole::text FROM pg_namespace WHERE nspname = $1)`, schema).Scan(&dbname, &owner); err != nil {
		return "", "", fmt.Errorf("schema %s not found (does the tenant use PostgreSQL?): %w", schema, err)
	}
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, role).Scan(&exists); err != nil {
		return "", "", err
	}
	q := func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
	// CREATE sets the safe attributes; a CREATEROLE (non-superuser) admin
	// may not touch SUPERUSER at all on ALTER, so ALTER changes only what
	// provisioning manages.
	create := `CREATE ROLE ` + q(role) + ` WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS`
	if exists {
		create = `ALTER ROLE ` + q(role) + ` WITH LOGIN`
	}
	stmts := []string{
		create + ` CONNECTION LIMIT 5 PASSWORD '` + pw + `'`,
		`GRANT CONNECT ON DATABASE ` + q(dbname) + ` TO ` + q(role),
		`GRANT USAGE ON SCHEMA ` + q(schema) + ` TO ` + q(role),
		`GRANT SELECT ON ALL TABLES IN SCHEMA ` + q(schema) + ` TO ` + q(role),
		`ALTER DEFAULT PRIVILEGES FOR ROLE ` + owner + ` IN SCHEMA ` + q(schema) + ` GRANT SELECT ON TABLES TO ` + q(role),
		`ALTER ROLE ` + q(role) + ` SET search_path = ` + q(schema),
		`ALTER ROLE ` + q(role) + ` SET default_transaction_read_only = on`,
		`ALTER ROLE ` + q(role) + ` SET statement_timeout = '30s'`,
		`ALTER ROLE ` + q(role) + ` SET work_mem = '16MB'`,
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback()
	for _, s := range stmts {
		if _, err := tx.ExecContext(ctx, s); err != nil {
			return "", "", fmt.Errorf("%s: %w", strings.SplitN(s, " PASSWORD", 2)[0], err)
		}
	}
	return pw, dbname, tx.Commit()
}

// ---- provisioning ---------------------------------------------------------------

// Provision creates or repairs a tenant's Grafana: its read-only database
// role (password rotated), organization, data source and dashboards. It is
// idempotent: run it again to fix drift.
func (p *Provisioner) Provision(ctx context.Context, tenant, schema string) (Result, error) {
	r := Result{Org: orgName(tenant), DBRole: Role(schema)}
	pw, dbname, err := p.role(ctx, schema)
	if err != nil {
		return r, err
	}
	if r.OrgID, err = p.org(ctx, tenant, true); err != nil {
		return r, err
	}
	ds := map[string]any{
		"name": "IoT Hub", "uid": "iothub-pg", "type": "grafana-postgresql-datasource", "access": "proxy",
		"url": p.cfg.DBHost, "user": Role(schema), "isDefault": true, "editable": false,
		"jsonData":       map[string]any{"database": dbname, "sslmode": p.cfg.SSLMode, "postgresVersion": 1600, "maxOpenConns": 4, "maxIdleConns": 2},
		"secureJsonData": map[string]any{"password": pw},
	}
	var existing struct {
		ID int64 `json:"id"`
	}
	switch err := p.call(ctx, "GET", "/api/datasources/uid/iothub-pg", r.OrgID, nil, &existing); {
	case err == nil:
		err = p.call(ctx, "PUT", "/api/datasources/uid/iothub-pg", r.OrgID, ds, nil)
		if err != nil {
			return r, err
		}
	case notFound(err):
		if err := p.call(ctx, "POST", "/api/datasources", r.OrgID, ds, nil); err != nil {
			return r, err
		}
	default:
		return r, err
	}
	var dash map[string]any
	if err := json.Unmarshal(overview, &dash); err != nil {
		return r, err
	}
	delete(dash, "id")
	if err := p.call(ctx, "POST", "/api/dashboards/db", r.OrgID, map[string]any{"dashboard": dash, "overwrite": true,
		"message": "provisioned by IoT Hub"}, nil); err != nil {
		return r, err
	}
	r.URL = fmt.Sprintf("%s/?orgId=%d", p.cfg.PublicURL, r.OrgID)
	r.Provisioned = time.Now().UnixMilli()
	return r, nil
}

// Deprovision removes a tenant's organization (with its dashboards and
// data source), its Grafana users and its database role. Call before the
// tenant's schema is dropped.
func (p *Provisioner) Deprovision(ctx context.Context, tenant, schema string) error {
	var errs []error
	if users, err := p.users(ctx, tenant); err == nil {
		for _, u := range users {
			if err := p.call(ctx, "DELETE", fmt.Sprintf("/api/admin/users/%d", u.ID), 0, nil, nil); err != nil && !notFound(err) {
				errs = append(errs, err)
			}
		}
	} else {
		errs = append(errs, err)
	}
	if id, err := p.org(ctx, tenant, false); err == nil {
		if err := p.call(ctx, "DELETE", fmt.Sprintf("/api/orgs/%d", id), 0, nil, nil); err != nil {
			errs = append(errs, err)
		}
	} else if !notFound(err) {
		errs = append(errs, err)
	}
	if identRe.MatchString(schema) {
		if err := p.dropRole(ctx, schema); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// dropRole revokes what provisioning granted (on objects the hub owns, so
// a CREATEROLE admin can) and drops the role. DROP OWNED would need the
// role's own privileges.
func (p *Provisioner) dropRole(ctx context.Context, schema string) error {
	db, err := p.db(ctx)
	if err != nil {
		return err
	}
	defer db.Close()
	role := Role(schema)
	var exists, schemaExists bool
	var dbname, owner string
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1), current_database(),
		EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $2),
		COALESCE((SELECT nspowner::regrole::text FROM pg_namespace WHERE nspname = $2), '')`, role, schema).Scan(&exists, &dbname, &schemaExists, &owner); err != nil {
		return err
	}
	if !exists {
		return nil
	}
	q := func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
	stmts := []string{`REVOKE CONNECT ON DATABASE ` + q(dbname) + ` FROM ` + q(role)}
	if schemaExists {
		stmts = append(stmts,
			`ALTER DEFAULT PRIVILEGES FOR ROLE `+owner+` IN SCHEMA `+q(schema)+` REVOKE SELECT ON TABLES FROM `+q(role),
			`REVOKE ALL ON ALL TABLES IN SCHEMA `+q(schema)+` FROM `+q(role),
			`REVOKE USAGE ON SCHEMA `+q(schema)+` FROM `+q(role))
	}
	stmts = append(stmts, `DROP ROLE `+q(role))
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, s := range stmts {
		if _, err := tx.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("%s: %w", s, err)
		}
	}
	return tx.Commit()
}

// ---- users ----------------------------------------------------------------------

// User is a tenant's Grafana user.
type User struct {
	ID    int64  `json:"id"`
	Login string `json:"login"` // without the tenant prefix
	Email string `json:"email,omitempty"`
	Role  string `json:"role"`
}

var loginRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,40}$`)

func prefix(tenant string) string { return tenant + "." }

func (p *Provisioner) users(ctx context.Context, tenant string) ([]User, error) {
	var res struct {
		Users []struct {
			ID    int64  `json:"id"`
			Login string `json:"login"`
			Email string `json:"email"`
		} `json:"users"`
	}
	if err := p.call(ctx, "GET", "/api/users/search?perpage=1000&query="+url.QueryEscape(prefix(tenant)), 0, nil, &res); err != nil {
		return nil, err
	}
	var out []User
	for _, u := range res.Users {
		if l, ok := strings.CutPrefix(u.Login, prefix(tenant)); ok {
			out = append(out, User{ID: u.ID, Login: l, Email: u.Email})
		}
	}
	return out, nil
}

// Users lists a tenant's Grafana users with their roles.
func (p *Provisioner) Users(ctx context.Context, tenant string) ([]User, error) {
	org, err := p.org(ctx, tenant, false)
	if err != nil {
		return nil, err
	}
	var members []struct {
		UserID int64  `json:"userId"`
		Role   string `json:"role"`
	}
	if err := p.call(ctx, "GET", fmt.Sprintf("/api/orgs/%d/users", org), 0, nil, &members); err != nil {
		return nil, err
	}
	roles := map[int64]string{}
	for _, m := range members {
		roles[m.UserID] = m.Role
	}
	us, err := p.users(ctx, tenant)
	for i := range us {
		us[i].Role = roles[us[i].ID]
	}
	if us == nil {
		us = []User{}
	}
	return us, err
}

// ErrInvalid marks a bad request.
var ErrInvalid = errors.New("invalid")

// SetUser creates a tenant user (returning a one-time password) or
// changes an existing one's role. Role: Viewer or Editor.
func (p *Provisioner) SetUser(ctx context.Context, tenant, login, email, role string) (password string, err error) {
	if !loginRe.MatchString(login) {
		return "", fmt.Errorf("%w: login: a-z, 0-9, _ . - (up to 41)", ErrInvalid)
	}
	if role != "Viewer" && role != "Editor" {
		return "", fmt.Errorf("%w: role must be Viewer or Editor (organization admin stays with the platform)", ErrInvalid)
	}
	org, err := p.org(ctx, tenant, false)
	if err != nil {
		return "", fmt.Errorf("tenant has no Grafana yet (provision it first): %w", err)
	}
	full := prefix(tenant) + login
	var u struct {
		ID int64 `json:"id"`
	}
	err = p.call(ctx, "GET", "/api/users/lookup?loginOrEmail="+url.QueryEscape(full), 0, nil, &u)
	switch {
	case notFound(err):
		password = secret()
		var c struct {
			ID int64 `json:"id"`
		}
		in := map[string]any{"name": full, "login": full, "password": password, "OrgId": org}
		if email != "" {
			in["email"] = email
		}
		if err := p.call(ctx, "POST", "/api/admin/users", 0, in, &c); err != nil {
			return "", err
		}
		u.ID = c.ID
	case err != nil:
		return "", err
	}
	// Member of the tenant's org with the role…
	if err := p.call(ctx, "PATCH", fmt.Sprintf("/api/orgs/%d/users/%d", org, u.ID), 0, map[string]string{"role": role}, nil); err != nil {
		if !notFound(err) {
			return "", err
		}
		if err := p.call(ctx, "POST", fmt.Sprintf("/api/orgs/%d/users", org), 0, map[string]string{"loginOrEmail": full, "role": role}, nil); err != nil {
			return "", err
		}
	}
	// …and of no other: Grafana may auto-assign new users to the main org.
	var orgs []struct {
		OrgID int64 `json:"orgId"`
	}
	if err := p.call(ctx, "GET", fmt.Sprintf("/api/users/%d/orgs", u.ID), 0, nil, &orgs); err != nil {
		return "", err
	}
	if err := p.call(ctx, "POST", fmt.Sprintf("/api/users/%d/using/%d", u.ID, org), 0, nil, nil); err != nil {
		return "", err
	}
	for _, o := range orgs {
		if o.OrgID != org {
			if err := p.call(ctx, "DELETE", fmt.Sprintf("/api/orgs/%d/users/%d", o.OrgID, u.ID), 0, nil, nil); err != nil {
				return "", fmt.Errorf("removing from org %d: %w", o.OrgID, err)
			}
		}
	}
	return password, nil
}

// DeleteUser removes a tenant's Grafana user.
func (p *Provisioner) DeleteUser(ctx context.Context, tenant, login string) error {
	var u struct {
		ID int64 `json:"id"`
	}
	if err := p.call(ctx, "GET", "/api/users/lookup?loginOrEmail="+url.QueryEscape(prefix(tenant)+login), 0, nil, &u); err != nil {
		return err
	}
	return p.call(ctx, "DELETE", fmt.Sprintf("/api/admin/users/%d", u.ID), 0, nil, nil)
}

// NotFound reports a Grafana 404.
func NotFound(err error) bool { return notFound(err) }
