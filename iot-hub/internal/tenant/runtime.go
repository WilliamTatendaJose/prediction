// Package tenant runs one isolated hub per tenant (like one Azure IoT Hub
// instance per customer): its own sensor registry, dashboard, live buffers,
// anomaly detector, alarm state, live stream, notifications, reports,
// backups and database. Nothing in a Runtime can reach another tenant's
// data; the platform's gateway and broker decide which Runtime a caller
// may use.
package tenant

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/williamtatendajose/prediction/iot-hub/internal/alarm"
	"github.com/williamtatendajose/prediction/iot-hub/internal/analytics"
	"github.com/williamtatendajose/prediction/iot-hub/internal/anomaly"
	"github.com/williamtatendajose/prediction/iot-hub/internal/api"
	"github.com/williamtatendajose/prediction/iot-hub/internal/auth"
	"github.com/williamtatendajose/prediction/iot-hub/internal/backup"
	"github.com/williamtatendajose/prediction/iot-hub/internal/calc"
	"github.com/williamtatendajose/prediction/iot-hub/internal/ingest"
	"github.com/williamtatendajose/prediction/iot-hub/internal/notify"
	"github.com/williamtatendajose/prediction/iot-hub/internal/report"
	"github.com/williamtatendajose/prediction/iot-hub/internal/store"
	"github.com/williamtatendajose/prediction/iot-hub/internal/stream"
	"github.com/williamtatendajose/prediction/iot-hub/internal/tsdb"
)

// Hooks connect a runtime to shared transports (set by the platform).
type Hooks struct {
	Publish      func(tenant string, r store.Reading) // republish to MQTT
	PublishEvent func(tenant string, e anomaly.Event) // anomaly to MQTT
	Kick         func(tenant, id string)              // drop MQTT sessions
}

// Options configure a runtime. The platform fills them from flags and the
// tenant's quota.
type Options struct {
	ConfigPath string // sensors + dashboard JSON ("" = memory only)
	DBURL      string // "" = memory only; path.db, sqlite:path, postgres://...
	PGSchema   string // Postgres schema for this tenant ("" = default search path)

	Capacity, MaxSensors, MaxFields int
	AutoRegister                    bool
	RawRetention, RollupRetention   time.Duration

	Anomaly   bool
	AnomalyCf anomaly.Config

	Notify        notify.Config // Targets empty = no notifications
	ReportTargets []*notify.Target
	Shifts        []string
	Location      *time.Location
	PublicURL     string

	BackupDir   string
	BackupEvery time.Duration
	BackupKeep  int

	Creds         *auth.Store // this tenant's view
	PublicRead    bool
	SecureCookies bool
	TLS           bool
	SelfService   func() bool
	MaxDevices    func() int
	Limiter       *Limiter // nil = unlimited

	Web    fs.FS // dashboard files (single-tenant; the gateway serves them otherwise)
	Hooks  Hooks
	Logger *slog.Logger
}

// Runtime is one tenant's running hub.
type Runtime struct {
	ID        string
	Store     *store.Store
	Hub       *stream.Hub
	Pipe      *ingest.Pipeline
	Detector  *anomaly.Detector
	Alarms    *alarm.Manager
	Analytics *analytics.Service
	DB        tsdb.DB
	Writer    *tsdb.Writer
	Notifier  *notify.Notifier
	API       *api.Server
	Limiter   *Limiter
	Handler   http.Handler

	log      *slog.Logger
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	stopBg   context.CancelFunc // writer + notifier outlive cancel until Close
	closeOne sync.Once
}

// Open builds and starts a tenant's runtime.
func Open(parent context.Context, id string, o Options) (*Runtime, error) {
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	log = log.With("tenant", id)
	loc := o.Location
	if loc == nil {
		loc = time.Local
	}
	st := store.New(store.Options{Path: o.ConfigPath, Capacity: o.Capacity, MaxSensors: o.MaxSensors,
		MaxFields: o.MaxFields, AutoRegister: o.AutoRegister})
	if err := st.Load(); err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	ctx, cancel := context.WithCancel(parent)
	bg, stopBg := context.WithCancel(context.Background())
	r := &Runtime{ID: id, Store: st, Hub: stream.NewHub(64), log: log, cancel: cancel, stopBg: stopBg, Limiter: o.Limiter}
	fail := func(err error) (*Runtime, error) {
		r.Close()
		return nil, err
	}
	r.Pipe = &ingest.Pipeline{Store: st, Hub: r.Hub}
	if o.Limiter != nil {
		r.Pipe.Admit = o.Limiter.Admit
	}
	r.Analytics = &analytics.Service{Store: st}
	r.API = &api.Server{Store: st, Hub: r.Hub, Pipeline: r.Pipe, Analytics: r.Analytics, Shifts: o.Shifts,
		Location: loc, Auth: o.Creds, PublicRead: o.PublicRead, SecureCookies: o.SecureCookies, TLS: o.TLS,
		PublicURL: o.PublicURL, SelfService: o.SelfService, MaxDevices: o.MaxDevices, Web: o.Web}

	if o.DBURL != "" {
		if p, ok := strings.CutPrefix(o.DBURL, "sqlite:"); ok || !strings.Contains(o.DBURL, "://") {
			if p == "" {
				p = o.DBURL
			}
			_ = os.MkdirAll(filepath.Dir(p), 0o755)
		}
		db, err := tsdb.OpenSchema(ctx, o.DBURL, o.PGSchema)
		if err != nil {
			return fail(fmt.Errorf("database: %w", err))
		}
		r.DB = db
		_ = db.CloseOpenEvents(ctx, time.Now().UnixMilli()) // episodes cut short by the last shutdown
		w := tsdb.NewWriter(db, 16384)
		w.RawRetention, w.RollupRetention = o.RawRetention, o.RollupRetention
		w.Logf = func(f string, a ...any) { log.Error(fmt.Sprintf(f, a...)) }
		r.Writer = w
		// The writer outlives ctx: it stops in Close, after transports, so
		// in-flight readings still reach the database.
		go w.Run(bg)
		r.Pipe.Writer, r.Analytics.DB, r.API.Writer = w, db, w
	}

	var alarmStore tsdb.AlarmStore
	if r.DB != nil {
		alarmStore = r.DB
	}
	r.Alarms = alarm.New(alarmStore)
	if err := r.Alarms.Load(ctx); err != nil {
		return fail(fmt.Errorf("load alarm state: %w", err))
	}
	r.Alarms.OnChange = func(kind string) {
		r.Hub.Publish(&stream.Msg{Event: "alarms", Data: []byte(`{"kind":"` + kind + `"}`)})
	}
	r.Pipe.Alarms, r.Analytics.Alarms, r.API.Alarms = r.Alarms, r.Alarms, r.Alarms
	r.Pipe.Calc = calc.NewEngine()
	r.Pipe.Calc.Errors = func(sensor, field string, err error) {
		log.Debug("calculated field", "sensor", sensor, "field", field, "err", err)
	}

	if len(o.Notify.Targets) > 0 {
		cfg := o.Notify
		cfg.Names = func(id string) string {
			if sv, err := st.Get(id); err == nil && sv.Name != "" {
				return sv.Name
			}
			return id
		}
		if cfg.Logf == nil {
			cfg.Logf = func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...)) }
		}
		r.Notifier = notify.New(cfg)
		go r.Notifier.Run(bg)
		r.API.Notifier = r.Notifier
	}
	if len(o.ReportTargets) > 0 {
		r.API.Reporter = notify.New(notify.Config{Targets: o.ReportTargets, Secret: o.Notify.Secret, BaseURL: o.PublicURL})
	}
	if o.Anomaly {
		r.Detector = anomaly.New(o.AnomalyCf)
		r.Pipe.Detector, r.Analytics.Detector, r.API.Detector = r.Detector, r.Detector, r.Detector
	}

	h := o.Hooks
	r.Pipe.OnEvent = func(e anomaly.Event) {
		if h.PublishEvent != nil {
			h.PublishEvent(id, e)
		}
		if r.Notifier != nil {
			r.Notifier.Notify(e)
		}
	}
	if h.Publish != nil {
		r.API.OnIngest = func(rd store.Reading) { h.Publish(id, rd) }
	}
	if h.Kick != nil {
		r.API.OnRevoke = func(dev string) { h.Kick(id, dev) }
	}

	if r.DB != nil {
		restoreLive(ctx, r.DB, st, r.Detector, o.Capacity, log)
	}
	r.goRun(func() { st.RunPersist(ctx, time.Second, func(f string, a ...any) { log.Error(fmt.Sprintf(f, a...)) }) })
	if det := r.Detector; det != nil {
		r.goRun(func() {
			t := time.NewTicker(10 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case now := <-t.C:
					r.Pipe.Emit(det.CheckStale(now.UnixMilli()))
				}
			}
		})
	}
	if r.API.Reporter != nil && len(o.Shifts) > 0 {
		r.goRun(func() {
			report.Schedule(ctx, o.Shifts, loc, func(from, to time.Time) {
				rctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				rep, err := report.Build(rctx, st, r.Analytics, from.UnixMilli(), to.UnixMilli(), "", loc)
				if err != nil {
					log.Error("shift report", "err", err)
					return
				}
				rep.Link = o.PublicURL
				res := r.API.Reporter.SendReport(rctx, rep.Title+" — "+rep.Period, rep.Text(loc), rep.HTML(loc), rep)
				log.Info("shift report sent", "period", rep.Period, "results", res)
			}, func(f string, a ...any) { log.Error(fmt.Sprintf(f, a...)) })
		})
	}
	if o.BackupDir != "" {
		r.API.Backups = &backup.Scheduler{Dir: o.BackupDir, Every: o.BackupEvery, Keep: o.BackupKeep, DB: r.DB,
			Config: func() backup.Config { return backup.Export(st, o.Creds, true) },
			Logf:   func(f string, a ...any) { log.Info(fmt.Sprintf(f, a...)) }}
		r.goRun(func() { r.API.Backups.Run(ctx) })
	}
	r.Handler = r.API.Handler()
	return r, nil
}

func (r *Runtime) goRun(f func()) {
	r.wg.Add(1)
	go func() { defer r.wg.Done(); f() }()
}

// Close stops the runtime: background loops, then the notifier and the
// database writer (final flush), then the database. Call after transports
// stop routing to it.
func (r *Runtime) Close() {
	r.closeOne.Do(func() {
		r.Hub.Close()
		r.cancel()
		r.wg.Wait() // includes the final config save
		r.stopBg()
		if r.Notifier != nil {
			<-r.Notifier.Done()
		}
		if r.Writer != nil {
			<-r.Writer.Done()
		}
		if r.DB != nil {
			r.DB.Close()
		}
	})
}

// SetRetention applies a quota change.
func (r *Runtime) SetRetention(raw, rollup time.Duration) {
	if r.Writer != nil {
		r.Writer.SetRetention(raw, rollup)
	}
}

func restoreLive(ctx context.Context, db tsdb.DB, st *store.Store, det *anomaly.Detector, limit int, log *slog.Logger) {
	t0 := time.Now()
	series, points := 0, 0
	gaps := map[string]float64{} // sensor -> typical reporting interval (ms)
	err := db.Latest(ctx, limit, func(sensor, field string, ts []int64, vals []float64, text string, textTS int64) {
		if !st.Restore(sensor, field, ts, vals, text, textTS) {
			return
		}
		series++
		points += len(ts)
		if det == nil || len(vals) == 0 {
			return
		}
		sv, _ := st.Get(sensor)
		if sv.Fields[field].Type != "bool" { // booleans are not spike-checked
			det.Warm(sensor, field, vals, st.Rule(sensor, field))
		}
		if len(ts) >= 4 {
			d := make([]float64, 0, len(ts)-1)
			for i := 1; i < len(ts); i++ {
				d = append(d, float64(ts[i]-ts[i-1]))
			}
			sort.Float64s(d)
			if m := d[len(d)/2]; m > 0 && (gaps[sensor] == 0 || m < gaps[sensor]) {
				gaps[sensor] = m // median gap: robust to outages in the history
			}
		}
	})
	if err != nil {
		log.Error("restore live state", "err", err)
		return
	}
	if det != nil {
		now := time.Now().UnixMilli()
		for _, sv := range st.List() {
			det.Resume(sv.ID, gaps[sv.ID], now)
		}
	}
	log.Info("restored live state", "series", series, "points", points, "took", time.Since(t0).Round(time.Millisecond))
}
