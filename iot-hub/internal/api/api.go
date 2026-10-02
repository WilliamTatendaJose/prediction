// Package api exposes the REST + SSE interface and serves the dashboard.
//
//	GET    /api/sensors                     list sensors with latest values
//	GET    /api/sensors/{id}                one sensor
//	PUT    /api/sensors/{id}                create/update a definition
//	DELETE /api/sensors/{id}
//	POST   /api/sensors/{id}/data           ingest (JSON object or bare value)
//	POST   /api/sensors/{id}/data/{field}   ingest one field (bare value)
//	GET    /api/sensors/{id}/history?field=&limit=&since=
//	GET    /api/dashboard | PUT /api/dashboard
//	GET    /api/sensors/{id}/series?field=&from=&to=&bucket=   bucketed min/avg/max
//	GET    /api/sensors/{id}/stats?field=&from=&to=            count/min/max/mean/std
//	GET    /api/anomalies?sensor=&from=&to=&limit=&active=1
//	GET    /api/stream                      SSE: readings, plus "anomaly" events
//	GET    /api/health
//
// from/to accept unix ms, RFC 3339, "now", or a duration before now such as
// -15m, -24h, -7d.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/williamtatendajose/prediction/iot-hub/internal/alarm"
	"github.com/williamtatendajose/prediction/iot-hub/internal/analytics"
	"github.com/williamtatendajose/prediction/iot-hub/internal/anomaly"
	"github.com/williamtatendajose/prediction/iot-hub/internal/auth"
	"github.com/williamtatendajose/prediction/iot-hub/internal/backup"
	"github.com/williamtatendajose/prediction/iot-hub/internal/connect"
	"github.com/williamtatendajose/prediction/iot-hub/internal/ingest"
	"github.com/williamtatendajose/prediction/iot-hub/internal/notify"
	"github.com/williamtatendajose/prediction/iot-hub/internal/store"
	"github.com/williamtatendajose/prediction/iot-hub/internal/stream"
	"github.com/williamtatendajose/prediction/iot-hub/internal/tsdb"
)

const (
	maxIngestBody    = 16 << 10
	maxConfigBody    = 256 << 10
	maxHistoryPoints = 5000
)

type Server struct {
	Store    *store.Store
	Hub      *stream.Hub
	Pipeline *ingest.Pipeline
	OnIngest func(store.Reading) // optional, e.g. republish to MQTT
	Web      fs.FS

	Analytics *analytics.Service
	Detector  *anomaly.Detector // optional
	Writer    *tsdb.Writer      // optional, for health reporting

	Auth          *auth.Store // nil = open
	PublicRead    bool        // reads need no token (writes still do)
	SecureCookies bool        // set when serving TLS
	TLS           bool        // adds HSTS
	OnRevoke      func(id string)
	Connectors    func() []connect.Status // optional
	Notifier      *notify.Notifier        // optional
	Alarms        *alarm.Manager
	Shifts        []string          // shift start times "06:00", for from=shift
	Location      *time.Location    // plant time zone (default local)
	Reporter      *notify.Notifier  // optional: shift report targets
	PublicURL     string            // linked from reports
	Backups       *backup.Scheduler // optional
	// SelfService: may tenant admins issue device credentials? nil = yes.
	SelfService func() bool
	// MaxDevices is the tenant's identity quota; nil or <= 0 = unlimited.
	MaxDevices func() int
	// Settings: alert targets, escalation, reports, shifts (optional).
	Settings    SettingsStore
	Escalations func() any
	Jobs        JobsStore
	Forward     func() any // edge store-and-forward status (optional)

	liveMu  sync.RWMutex // guards Notifier, Reporter, Shifts, Location after start
	batches batches      // recent batch ids (idempotent retries)
	started time.Time
}

func (s *Server) Handler() http.Handler {
	s.started = time.Now()
	if s.Auth == nil {
		s.Auth = auth.New("", "")
	}
	read := func(h http.HandlerFunc) http.HandlerFunc { return s.require(auth.Read, h) }
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.health)
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("POST /api/logout", s.logout)
	mux.HandleFunc("GET /api/me", s.me)
	mux.HandleFunc("GET /api/sensors", read(s.listSensors))
	mux.HandleFunc("GET /api/sensors/{id}", read(s.getSensor))
	mux.HandleFunc("PUT /api/sensors/{id}", s.require(auth.Define, s.putSensor))
	mux.HandleFunc("DELETE /api/sensors/{id}", s.require(auth.Define, s.deleteSensor))
	mux.HandleFunc("POST /api/sensors/{id}/data", s.require(auth.Ingest, s.ingest))
	mux.HandleFunc("POST /api/ingest/batch", s.requireAny(s.ingestBatch)) // per-line permission checks
	mux.HandleFunc("POST /api/sensors/{id}/data/{field}", s.require(auth.Ingest, s.ingest))
	mux.HandleFunc("GET /api/sensors/{id}/history", read(s.history))
	mux.HandleFunc("GET /api/sensors/{id}/series", read(s.series))
	mux.HandleFunc("GET /api/sensors/{id}/stats", read(s.stats))
	mux.HandleFunc("GET /api/sensors/{id}/forecast", read(s.forecastSeries))
	mux.HandleFunc("GET /api/anomalies", read(s.anomalies))
	mux.HandleFunc("GET /api/dashboard", read(s.getDashboard))
	mux.HandleFunc("PUT /api/dashboard", s.require(auth.Manage, s.putDashboard))
	mux.HandleFunc("GET /api/stream", read(s.stream))
	mux.HandleFunc("GET /api/connectors", read(s.connectors))
	if s.Alarms == nil {
		s.Alarms = alarm.New(nil)
	}
	mux.HandleFunc("POST /api/anomalies/{eid}/ack", s.require(auth.Operate, s.ackEvent))
	mux.HandleFunc("POST /api/anomalies/{eid}/notes", s.require(auth.Operate, s.addNote))
	mux.HandleFunc("GET /api/anomalies/{eid}/notes", read(s.listNotes))
	mux.HandleFunc("GET /api/shelves", read(s.listShelves))
	mux.HandleFunc("POST /api/shelves", s.require(auth.Operate, s.shelve))
	mux.HandleFunc("DELETE /api/shelves/{key}", s.require(auth.Operate, s.unshelve))
	mux.HandleFunc("GET /api/audit", s.require(auth.Manage, s.auditLog))
	mux.HandleFunc("GET /api/labels.csv", read(s.labels))
	mux.HandleFunc("GET /api/sensors/{id}/oee", read(s.oeeHandler))
	mux.HandleFunc("GET /api/oee", read(s.oeeOverview))
	mux.HandleFunc("GET /api/config", s.require(auth.Manage, s.exportConfig))
	mux.HandleFunc("POST /api/config", s.require(auth.Manage, s.importConfig))
	mux.HandleFunc("GET /api/backups", s.require(auth.Manage, s.backups))
	mux.HandleFunc("POST /api/backups", s.require(auth.Manage, s.backupNow))
	mux.HandleFunc("GET /api/backups/{name}", s.require(auth.Manage, s.backupFile))
	mux.HandleFunc("GET /api/reports", read(s.reports))
	mux.HandleFunc("POST /api/reports/send", s.require(auth.Manage, s.sendReport))
	mux.HandleFunc("GET /api/settings", s.require(auth.Manage, s.getSettings))
	mux.HandleFunc("PUT /api/settings", s.require(auth.Manage, s.putSettings))
	mux.HandleFunc("PUT /api/settings/targets/{target}", s.require(auth.Manage, s.putTarget))
	mux.HandleFunc("DELETE /api/settings/targets/{target}", s.require(auth.Manage, s.deleteTarget))
	mux.HandleFunc("POST /api/settings/targets/{target}/test", s.require(auth.Manage, s.testTarget))
	mux.HandleFunc("POST /api/calc/test", read(s.calcTest))
	mux.HandleFunc("GET /api/forward", s.require(auth.Manage, s.forwardStatus))
	mux.HandleFunc("GET /api/jobs", read(s.listJobs))
	mux.HandleFunc("POST /api/jobs/test", s.require(auth.Manage, s.testJob))
	mux.HandleFunc("PUT /api/jobs/{job}", s.require(auth.Manage, s.putJob))
	mux.HandleFunc("DELETE /api/jobs/{job}", s.require(auth.Manage, s.deleteJob))
	mux.HandleFunc("GET /api/notifications", s.require(auth.Manage, s.notifications))
	mux.HandleFunc("POST /api/notifications/test", s.require(auth.Manage, s.testNotification))
	mux.HandleFunc("GET /api/devices", s.require(auth.Manage, s.listDevices))
	mux.HandleFunc("POST /api/devices", s.require(auth.Manage, s.addDevice))
	mux.HandleFunc("DELETE /api/devices/{id}", s.require(auth.Manage, s.deleteDevice))
	mux.HandleFunc("PATCH /api/devices/{id}", s.require(auth.Manage, s.updateDevice))
	mux.HandleFunc("POST /api/devices/{id}/rotate", s.require(auth.Manage, s.rotateDevice))
	mux.HandleFunc("GET /api/devices/{id}/keys", s.require(auth.Manage, s.deviceKeys))
	mux.HandleFunc("POST /api/devices/{id}/sas", s.require(auth.Manage, s.deviceSAS))
	if s.Web != nil {
		// The dashboard shell holds no data, so it loads without a token
		// and shows a login form when the API answers 401.
		assets, err := loadAssets(s.Web)
		if err != nil {
			panic("dashboard assets: " + err.Error()) // embedded at build time; can't fail at run time
		}
		mux.HandleFunc("GET /", staticHandler(assets))
	}
	return secureHeaders(mux, s.TLS)
}

func (s *Server) connectors(w http.ResponseWriter, _ *http.Request) {
	if s.Connectors == nil {
		writeJSON(w, http.StatusOK, []connect.Status{})
		return
	}
	writeJSON(w, http.StatusOK, s.Connectors())
}

type notifyStatus struct {
	Target    string `json:"target"`
	Sent      uint64 `json:"sent"`
	Failed    uint64 `json:"failed"`
	LastError string `json:"lastError,omitempty"`
}

func (s *Server) notifications(w http.ResponseWriter, _ *http.Request) {
	out := map[string]any{"targets": []notifyStatus{}}
	if n := s.notifier(); n != nil {
		ts := []notifyStatus{}
		for _, t := range n.Targets() {
			ts = append(ts, notifyStatus{Target: t.Redacted(), Sent: t.Sent.Load(), Failed: t.Failed.Load(), LastError: t.LastError()})
		}
		out = map[string]any{"targets": ts, "suppressed": n.Suppressed.Load(), "dropped": n.Dropped.Load()}
	}
	writeJSON(w, http.StatusOK, out)
}

// testNotification sends a test message to every target and reports each
// result, so setup mistakes show up immediately rather than at 3 a.m.
func (s *Server) testNotification(w http.ResponseWriter, r *http.Request) {
	n := s.notifier()
	if n == nil {
		writeErr(w, http.StatusConflict, errors.New("no notification targets configured (-notify)"))
		return
	}
	res := map[string]string{}
	for _, t := range n.Targets() {
		if err := n.Test(r.Context(), t); err != nil {
			res[t.Redacted()] = err.Error()
		} else {
			res[t.Redacted()] = "ok"
		}
	}
	writeJSON(w, http.StatusOK, res)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func storeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, err)
	case errors.Is(err, store.ErrInvalid):
		writeErr(w, http.StatusBadRequest, err)
	case errors.Is(err, ingest.ErrQuota):
		writeErr(w, http.StatusTooManyRequests, err)
	case errors.Is(err, store.ErrLimit):
		writeErr(w, http.StatusInsufficientStorage, err)
	default:
		writeErr(w, http.StatusInternalServerError, err)
	}
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	h := map[string]any{
		"ok":            true,
		"uptimeSec":     int(time.Since(s.started).Seconds()),
		"sensors":       s.Store.Count(),
		"streamClients": s.Hub.Clients(),
		"streamDropped": s.Hub.Dropped.Load(),
		"heapMB":        float64(m.HeapAlloc) / (1 << 20),
		"goroutines":    runtime.NumGoroutine(),
	}
	if s.Detector != nil {
		h["anomaliesActive"] = len(s.Detector.Active())
	}
	if s.Writer != nil {
		db := map[string]any{
			"queued":  s.Writer.Queued(),
			"written": s.Writer.Written.Load(),
			"dropped": s.Writer.Dropped.Load(),
			"errors":  s.Writer.Errors.Load(),
		}
		if e := s.Writer.LastError(); e != "" {
			db["lastError"] = e
		}
		h["db"] = db
	}
	writeJSON(w, http.StatusOK, h)
}

func (s *Server) listSensors(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.Store.List())
}

func (s *Server) getSensor(w http.ResponseWriter, r *http.Request) {
	v, err := s.Store.Get(r.PathValue("id"))
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) putSensor(w http.ResponseWriter, r *http.Request) {
	var def store.Sensor
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxConfigBody)).Decode(&def); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	def.ID = r.PathValue("id")
	if err := s.Store.Upsert(def); err != nil {
		storeErr(w, err)
		return
	}
	if b, err := json.Marshal(def.Fields); err == nil {
		s.audit(r, "sensor.define", def.ID, string(b))
	}
	if s.Pipeline != nil && s.Pipeline.Calc != nil {
		s.Pipeline.Calc.Forget(def.ID) // integrals restart from the stored total
	}
	v, _ := s.Store.Get(def.ID)
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) deleteSensor(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.Delete(r.PathValue("id")); err != nil {
		storeErr(w, err)
		return
	}
	if s.Detector != nil {
		s.Detector.Forget(r.PathValue("id"))
	}
	s.audit(r, "sensor.delete", r.PathValue("id"), "")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) ingest(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxIngestBody))
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, err)
		return
	}
	rd, err := s.Pipeline.Handle(r.PathValue("id"), r.PathValue("field"), body)
	if err != nil {
		storeErr(w, err)
		return
	}
	if s.OnIngest != nil {
		s.OnIngest(rd)
	}
	writeJSON(w, http.StatusAccepted, rd)
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	field := q.Get("field")
	if field == "" {
		field = "value"
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > maxHistoryPoints {
		limit = maxHistoryPoints
	}
	since, _ := strconv.ParseInt(q.Get("since"), 10, 64)
	ts, vals, err := s.Store.History(r.PathValue("id"), field, limit, since)
	if err != nil {
		storeErr(w, err)
		return
	}
	// Columnar arrays are ~40% smaller than [{t,v}] objects and map directly
	// onto typed arrays in the browser.
	writeJSON(w, http.StatusOK, map[string]any{"field": field, "t": ts, "v": vals})
}

func (s *Server) getDashboard(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(s.Store.Dashboard())
}

func (s *Server) putDashboard(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxConfigBody))
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, err)
		return
	}
	if err := s.Store.SetDashboard(body); err != nil {
		storeErr(w, err)
		return
	}
	s.audit(r, "dashboard.save", "dashboard", fmt.Sprintf("%d bytes", len(body)))
	w.WriteHeader(http.StatusNoContent)
}

// stream sends readings (default event) and anomaly episodes (event
// "anomaly") as SSE. Optional ?sensors=a,b filters server-side.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, errors.New("streaming unsupported"))
		return
	}
	var filter map[string]bool
	if f := r.URL.Query().Get("sensors"); f != "" {
		filter = map[string]bool{}
		for _, id := range strings.Split(f, ",") {
			filter[id] = true
		}
	}
	// Subscribe before answering: once the client sees the response, every
	// later reading must reach it.
	ch := s.Hub.Subscribe()
	defer s.Hub.Unsubscribe(ch)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "retry: 3000\n\n")
	flusher.Flush()
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	write := func(m *stream.Msg) {
		if filter != nil && !filter[m.Sensor] {
			return
		}
		if m.Event != "" {
			_, _ = io.WriteString(w, "event: "+m.Event+"\n")
		}
		_, _ = io.WriteString(w, "data: ")
		_, _ = w.Write(m.Data)
		_, _ = io.WriteString(w, "\n\n")
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.Hub.Done():
			return
		case <-ping.C:
			_, _ = io.WriteString(w, ": ping\n\n")
			flusher.Flush()
		case m := <-ch:
			write(m)
			// Drain whatever else is queued before flushing: one syscall per
			// burst instead of one per message.
			for n := len(ch); n > 0; n-- {
				write(<-ch)
			}
			flusher.Flush()
		}
	}
}
