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
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/williamtatendajose/prediction/iot-hub/internal/analytics"
	"github.com/williamtatendajose/prediction/iot-hub/internal/anomaly"
	"github.com/williamtatendajose/prediction/iot-hub/internal/ingest"
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
	Token    string              // if set, required as Bearer token for writes
	OnIngest func(store.Reading) // optional, e.g. republish to MQTT
	Web      fs.FS

	Analytics *analytics.Service
	Detector  *anomaly.Detector // optional
	Writer    *tsdb.Writer      // optional, for health reporting
	started   time.Time
}

func (s *Server) Handler() http.Handler {
	s.started = time.Now()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.health)
	mux.HandleFunc("GET /api/sensors", s.listSensors)
	mux.HandleFunc("GET /api/sensors/{id}", s.getSensor)
	mux.HandleFunc("PUT /api/sensors/{id}", s.auth(s.putSensor))
	mux.HandleFunc("DELETE /api/sensors/{id}", s.auth(s.deleteSensor))
	mux.HandleFunc("POST /api/sensors/{id}/data", s.auth(s.ingest))
	mux.HandleFunc("POST /api/sensors/{id}/data/{field}", s.auth(s.ingest))
	mux.HandleFunc("GET /api/sensors/{id}/history", s.history)
	mux.HandleFunc("GET /api/sensors/{id}/series", s.series)
	mux.HandleFunc("GET /api/sensors/{id}/stats", s.stats)
	mux.HandleFunc("GET /api/anomalies", s.anomalies)
	mux.HandleFunc("GET /api/dashboard", s.getDashboard)
	mux.HandleFunc("PUT /api/dashboard", s.auth(s.putDashboard))
	mux.HandleFunc("GET /api/stream", s.stream)
	if s.Web != nil {
		mux.Handle("GET /", http.FileServerFS(s.Web))
	}
	return mux
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	if s.Token == "" {
		return next
	}
	want := []byte("Bearer " + s.Token)
	return func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			writeErr(w, http.StatusUnauthorized, errors.New("missing or invalid bearer token"))
			return
		}
		next(w, r)
	}
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
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "retry: 3000\n\n")
	flusher.Flush()

	ch := s.Hub.Subscribe()
	defer s.Hub.Unsubscribe(ch)
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
