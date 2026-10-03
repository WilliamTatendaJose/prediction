package grafana

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/williamtatendajose/prediction/iot-hub/internal/tsdb"
)

// The embedded dashboard and the one deploy/ provisions must not drift.
func TestDashboardMatchesDeploy(t *testing.T) {
	b, err := os.ReadFile("../../deploy/grafana/dashboards/iothub-overview.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, overview) {
		t.Fatal("internal/grafana/dashboards/iothub-overview.json differs from deploy/grafana/dashboards: copy one to the other")
	}
}

// Integration with a real Grafana and PostgreSQL:
//
//	GRAFANA_TEST_URL=http://admin:adminpw@127.0.0.1:3300
//	IOTHUB_TEST_PG_ADMIN=postgres://hubsvc:hubpw@127.0.0.1:55432/saas  (a CREATEROLE, non-superuser role)
func setup(t *testing.T) (*Provisioner, string) {
	gu, pg := os.Getenv("GRAFANA_TEST_URL"), os.Getenv("IOTHUB_TEST_PG_ADMIN")
	if gu == "" || pg == "" {
		t.Skip("GRAFANA_TEST_URL and IOTHUB_TEST_PG_ADMIN not set")
	}
	user, pass, host := "", "", gu
	if i := strings.Index(gu, "://"); i > 0 {
		rest := gu[i+3:]
		if at := strings.Index(rest, "@"); at > 0 {
			user, pass, _ = strings.Cut(rest[:at], ":")
			host = gu[:i+3] + rest[at+1:]
		}
	}
	dbHost := pg[strings.Index(pg, "@")+1:]
	dbHost = dbHost[:strings.Index(dbHost, "/")]
	p, err := New(Config{URL: host, User: user, Password: pass, DBAdminURL: pg, DBHost: dbHost})
	if err != nil {
		t.Fatal(err)
	}
	return p, pg
}

// asUser runs SQL through Grafana's query API with a tenant user's login.
func asUser(t *testing.T, p *Provisioner, login, password string, org int64, sqlText string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"from": "now-1h", "to": "now", "queries": []any{map[string]any{
		"refId": "A", "datasource": map[string]string{"uid": "iothub-pg"}, "rawSql": sqlText, "format": "table", "rawQuery": true}}})
	req, _ := http.NewRequest("POST", p.cfg.URL+"/api/ds/query", bytes.NewReader(body))
	req.SetBasicAuth(login, password)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Grafana-Org-Id", fmt.Sprint(org))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func TestPerTenantGrafana(t *testing.T) {
	p, pg := setup(t)
	ctx := context.Background()
	tenants := map[string]float64{"acme": 11, "globex": 99}
	for tn, v := range tenants {
		sch := "t_" + tn
		p.Deprovision(ctx, tn, sch)
		tsdb.DropSchema(ctx, pg, sch)
		db, err := tsdb.OpenSchema(ctx, pg, sch) // as the hub does: creates the schema and tables
		if err != nil {
			t.Fatal(err)
		}
		if err := db.WritePoints(ctx, []tsdb.Point{{Sensor: "tank-1", Field: "level", TS: time.Now().UnixMilli(), Value: v}}); err != nil {
			t.Fatal(err)
		}
		db.Close()
		t.Cleanup(func() { p.Deprovision(ctx, tn, sch); tsdb.DropSchema(ctx, pg, sch) })
	}

	acme, err := p.Provision(ctx, "acme", "t_acme")
	if err != nil {
		t.Fatal(err)
	}
	globex, err := p.Provision(ctx, "globex", "t_globex")
	if err != nil {
		t.Fatal(err)
	}
	if acme.OrgID == globex.OrgID || acme.OrgID <= 1 {
		t.Fatalf("orgs %d %d", acme.OrgID, globex.OrgID)
	}
	// Idempotent: a second run repairs, not duplicates (password rotated).
	again, err := p.Provision(ctx, "acme", "t_acme")
	if err != nil || again.OrgID != acme.OrgID {
		t.Fatalf("re-provision: %+v %v", again, err)
	}

	pw, err := p.SetUser(ctx, "acme", "ana", "", "Editor")
	if err != nil || pw == "" {
		t.Fatalf("user: %v", err)
	}
	if _, err := p.SetUser(ctx, "acme", "boss", "", "Admin"); err == nil {
		t.Fatal("tenants must not grant org Admin")
	}
	if _, err := p.SetUser(ctx, "acme", "x@y", "", "Viewer"); err == nil {
		t.Fatal("bad login accepted")
	}
	gpw, _ := p.SetUser(ctx, "globex", "gus", "", "Editor")

	// The user sees their tenant's data through the provisioned source…
	code, out := asUser(t, p, "acme.ana", pw, acme.OrgID, `SELECT value FROM readings`)
	if code != 200 || !strings.Contains(out, "11") || strings.Contains(out, "99") {
		t.Fatalf("own data: %d %s", code, out)
	}
	// …and nothing else, whatever SQL they write.
	for name, q := range map[string]string{
		"other schema":   `SELECT value FROM t_globex.readings`,
		"write":          `DELETE FROM readings`,
		"create":         `CREATE TABLE stolen AS SELECT 1`,
		"change role":    `SET ROLE grafana_t_globex`,
		"files":          `SELECT pg_read_file('/etc/passwd')`,
		"undo read-only": `SET default_transaction_read_only = off; DELETE FROM readings`,
	} {
		_, out := asUser(t, p, "acme.ana", pw, acme.OrgID, q)
		if !strings.Contains(out, "error") || strings.Contains(out, "99") {
			t.Errorf("%s was not refused: %s", name, out)
		}
	}
	// The data survived those attempts.
	if _, out := asUser(t, p, "acme.ana", pw, acme.OrgID, `SELECT count(*) AS n FROM readings`); !strings.Contains(out, `[1]`) {
		t.Errorf("acme data changed: %s", out)
	}
	// Another tenant's org, and the platform's main org, are out of reach.
	if code, _ := asUser(t, p, "acme.ana", pw, globex.OrgID, `SELECT 1`); code < 400 {
		t.Errorf("acme user queried in globex's org: %d", code)
	}
	if code, _ := asUser(t, p, "acme.ana", pw, 1, `SELECT 1`); code < 400 {
		t.Errorf("acme user queried in the main org: %d", code)
	}
	if code, out := asUser(t, p, "globex.gus", gpw, globex.OrgID, `SELECT value FROM readings`); code != 200 || !strings.Contains(out, "99") {
		t.Errorf("globex own data: %d %s", code, out)
	}
	// Dashboards are in each org; editing data sources is not allowed for
	// tenant editors (org admin stays with the platform).
	req, _ := http.NewRequest("GET", p.cfg.URL+"/api/dashboards/uid/iothub-overview", nil)
	req.SetBasicAuth("acme.ana", pw)
	req.Header.Set("X-Grafana-Org-Id", fmt.Sprint(acme.OrgID))
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != 200 {
		t.Errorf("dashboard: %v %v", err, res.StatusCode)
	}
	req, _ = http.NewRequest("PUT", p.cfg.URL+"/api/datasources/uid/iothub-pg", strings.NewReader(`{"name":"IoT Hub","type":"grafana-postgresql-datasource","url":"10.0.0.1:5432","access":"proxy"}`))
	req.SetBasicAuth("acme.ana", pw)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Grafana-Org-Id", fmt.Sprint(acme.OrgID))
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode < 400 {
		t.Errorf("editor changed the data source: %v %v", err, res.StatusCode)
	}
	us, err := p.Users(ctx, "acme")
	if err != nil || len(us) != 1 || us[0].Login != "ana" || us[0].Role != "Editor" {
		t.Fatalf("users %+v %v", us, err)
	}

	// Deprovision: org, users and role are gone.
	if err := p.Deprovision(ctx, "acme", "t_acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.org(ctx, "acme", false); !notFound(err) {
		t.Errorf("org still there: %v", err)
	}
	db, _ := sql.Open("pgx", pg)
	defer db.Close()
	var n int
	db.QueryRow(`SELECT count(*) FROM pg_roles WHERE rolname = 'grafana_t_acme'`).Scan(&n)
	if n != 0 {
		t.Error("role still there")
	}
	if code, _ := asUser(t, p, "acme.ana", pw, acme.OrgID, `SELECT 1`); code < 400 {
		t.Error("deleted user still works")
	}
}
