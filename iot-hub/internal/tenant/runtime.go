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
	"sync/atomic"
	"time"

	"github.com/williamtatendajose/prediction/iot-hub/internal/alarm"
	"github.com/williamtatendajose/prediction/iot-hub/internal/analytics"
	"github.com/williamtatendajose/prediction/iot-hub/internal/anomaly"
	"github.com/williamtatendajose/prediction/iot-hub/internal/api"
	"github.com/williamtatendajose/prediction/iot-hub/internal/auth"
	"github.com/williamtatendajose/prediction/iot-hub/internal/backup"
	"github.com/williamtatendajose/prediction/iot-hub/internal/calc"
	"github.com/williamtatendajose/prediction/iot-hub/internal/escalate"
	"github.com/williamtatendajose/prediction/iot-hub/internal/ingest"
	"github.com/williamtatendajose/prediction/iot-hub/internal/jobs"
	"github.com/williamtatendajose/prediction/iot-hub/internal/notify"
	"github.com/williamtatendajose/prediction/iot-hub/internal/report"
	"github.com/williamtatendajose/prediction/iot-hub/internal/store"
	"github.com/williamtatendajose/prediction/iot-hub/internal/stream"
	"github.com/williamtatendajose/prediction/iot-hub/internal/tsdb"
	"github.com/williamtatendajose/prediction/iot-hub/internal/twin"
)

// Hooks connect a runtime to shared transports (set by the platform).
type Hooks struct {
	Publish      func(tenant string, r store.Reading) // republish to MQTT
	PublishEvent func(tenant string, e anomaly.Event) // anomaly to MQTT
	Kick         func(tenant, id string)              // drop MQTT sessions
	// Device transport for twins, methods and messages.
	DeviceListening func(tenant, id, sub string) bool
	DeviceConnected func(tenant, id string) bool
	SendToDevice    func(tenant, id, sub string, payload []byte) int
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

	// Seed is used until the tenant saves its own settings (SettingsPath).
	Seed         Settings
	SettingsPath string
	JobsPath     string
	TwinsPath    string
	MaxJobs      func() int
	PublicURL    string

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

	// Forward receives every stored reading (edge store-and-forward).
	Forward       func(store.Reading)
	ForwardStatus func() any
	Web           fs.FS // dashboard files (single-tenant; the gateway serves them otherwise)
	Hooks         Hooks
	Logger        *slog.Logger
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
	API       *api.Server
	Escalate  *escalate.Engine
	Jobs      *jobs.Engine
	Twins     *twin.Service
	Limiter   *Limiter
	Handler   http.Handler

	log        *slog.Logger
	ctx        context.Context
	settings   *settingsMgr
	notifier   atomic.Pointer[running]
	liveMu     sync.Mutex
	reportStop context.CancelFunc
	publicURL  string
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	stopBg     context.CancelFunc // writer + notifier outlive cancel until Close
	closeOne   sync.Once
}

// Open builds and starts a tenant's runtime.
func Open(parent context.Context, id string, o Options) (*Runtime, error) {
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	log = log.With("tenant", id)
	st := store.New(store.Options{Path: o.ConfigPath, Capacity: o.Capacity, MaxSensors: o.MaxSensors,
		MaxFields: o.MaxFields, AutoRegister: o.AutoRegister})
	if err := st.Load(); err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	ctx, cancel := context.WithCancel(parent)
	bg, stopBg := context.WithCancel(context.Background())
	r := &Runtime{ID: id, Store: st, Hub: stream.NewHub(64), log: log, cancel: cancel, stopBg: stopBg, Limiter: o.Limiter,
		ctx: ctx, publicURL: o.PublicURL}
	fail := func(err error) (*Runtime, error) {
		r.Close()
		return nil, err
	}
	r.Pipe = &ingest.Pipeline{Store: st, Hub: r.Hub}
	if o.Limiter != nil {
		r.Pipe.Admit = o.Limiter.Admit
	}
	r.Analytics = &analytics.Service{Store: st}
	r.API = &api.Server{Store: st, Hub: r.Hub, Pipeline: r.Pipe, Analytics: r.Analytics, Auth: o.Creds, PublicRead: o.PublicRead, SecureCookies: o.SecureCookies, TLS: o.TLS,
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

	// The detector also tracks alerts raised by stream jobs, so it exists
	// even when automatic detection is off.
	r.Detector = anomaly.New(o.AnomalyCf)
	r.Analytics.Detector, r.API.Detector = r.Detector, r.Detector
	if o.Anomaly {
		r.Pipe.Detector = r.Detector
	}

	h := o.Hooks
	r.Pipe.OnEvent = func(e anomaly.Event) {
		if h.PublishEvent != nil {
			h.PublishEvent(id, e)
		}
		if n := r.notifier.Load(); n != nil {
			n.n.Notify(e)
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
	if det := r.Pipe.Detector; det != nil {
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
	// Settings: alert targets, escalation, reports, shifts, time zone.
	r.Escalate = escalate.New(func() []anomaly.Event { return r.Alarms.Decorate(r.Detector.Active()) })
	r.settings = &settingsMgr{path: o.SettingsPath, apply: r.applySettings}
	r.API.Settings = r.settings
	r.API.Escalations = func() any { return r.Escalate.Recent() }
	seed, err := r.settings.load(o.Seed)
	if err != nil {
		return fail(err)
	}
	r.settings.mu.Lock()
	if err := r.settings.set(seed); err != nil {
		// A stored target that is no longer allowed (e.g. a private
		// address after switching to multi-tenant) must not keep the
		// tenant down: run without notifications and say why.
		log.Error("settings: running without notification targets until fixed", "err", err)
		_ = r.settings.set(Settings{Shifts: seed.Shifts, TimeZone: seed.TimeZone})
		r.settings.cur = seed // keep the file's content for the admin to fix
	}
	r.settings.mu.Unlock()
	r.goRun(func() { r.Escalate.Run(ctx, 30*time.Second) })

	// Stream jobs: windowed aggregates into derived sensors, alerts or
	// webhooks.
	hooks := make(chan hookJob, 256)
	r.Jobs = jobs.New(jobs.Outputs{
		Sensor: func(sensor string, ts int64, v map[string]any) error {
			rd, err := r.Pipe.HandleDerived(sensor, ts, v)
			if err == nil && r.API.OnIngest != nil {
				r.API.OnIngest(rd)
			}
			return err
		},
		Raise: func(key, sensor, field string, ts int64, v float64, msg string) {
			if e, ok := r.Detector.Raise(key, sensor, field, "rule", ts, v, msg); ok {
				r.Pipe.Emit([]anomaly.Event{e})
			}
		},
		Clear: func(key string, ts int64) {
			if e, ok := r.Detector.Clear(key, ts); ok {
				r.Pipe.Emit([]anomaly.Event{e})
			}
		},
		Webhook: func(target string, row jobs.Row) {
			select {
			case hooks <- hookJob{target, row}:
			default: // a slow endpoint must not stall ingestion
				r.log.Warn("job webhook queue full: row dropped", "job", row.Job)
			}
		},
	})
	maxJobs := o.MaxJobs
	if maxJobs == nil {
		maxJobs = func() int { return MaxJobsDefault }
	}
	jm := &jobsMgr{path: o.JobsPath, engine: r.Jobs, maxJobs: maxJobs, store: st, an: r.Analytics}
	if err := jm.load(); err != nil {
		return fail(err)
	}
	r.API.Jobs = jm
	r.Pipe.Observe = r.Jobs.Observe
	if fw := o.Forward; fw != nil {
		r.Pipe.Observe = func(rd store.Reading) { r.Jobs.Observe(rd); fw(rd) }
	}
	r.API.Forward = o.ForwardStatus

	// Device twins, direct methods and cloud-to-device messages.
	r.Twins = twin.New(o.TwinsPath, o.Creds.Exists)
	if err := r.Twins.LoadAll(); err != nil {
		return fail(err)
	}
	if h.SendToDevice != nil {
		r.Twins.SetTransport(twin.Transport{
			Listening: func(dev, sub string) bool { return h.DeviceListening(id, dev, sub) },
			Connected: func(dev string) bool { return h.DeviceConnected(id, dev) },
			Send:      func(dev, sub string, p []byte) int { return h.SendToDevice(id, dev, sub, p) },
		})
	}
	r.API.Twins = r.Twins
	r.goRun(func() { r.Twins.Run(ctx, func(f string, a ...any) { log.Error(fmt.Sprintf(f, a...)) }) })
	// Devices that missed their expected interval raise an "overdue" alarm.
	if det := r.Detector; det != nil {
		od := newOverdue(r.Twins, det, r.Pipe.Emit, func() map[string]bool {
			out := map[string]bool{}
			for _, d := range o.Creds.List() {
				if d.Disabled || d.Expires > 0 && d.Expires < time.Now().UnixMilli() {
					out[d.ID] = true
				}
			}
			return out
		}, time.Now().UnixMilli())
		r.Twins.OnData = od.data
		r.goRun(func() {
			t := time.NewTicker(10 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case now := <-t.C:
					od.check(now.UnixMilli())
				}
			}
		})
	}
	r.goRun(func() { r.Jobs.Run(ctx) })
	r.goRun(func() { r.sendHooks(ctx, hooks) })
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
		r.liveMu.Lock()
		if r.reportStop != nil {
			r.reportStop()
		}
		r.liveMu.Unlock()
		if n := r.notifier.Swap(nil); n != nil {
			n.stop()
		}
		if r.Writer != nil {
			<-r.Writer.Done()
		}
		if r.DB != nil {
			r.DB.Close()
		}
	})
}

type hookJob struct {
	target string
	row    jobs.Row
}

// sendHooks delivers job rows to settings targets, one at a time.
func (r *Runtime) sendHooks(ctx context.Context, in <-chan hookJob) {
	for {
		select {
		case <-ctx.Done():
			return
		case h := <-in:
			t, secret := r.settings.target(h.target)
			if t == nil {
				r.log.Warn("job webhook: unknown target", "job", h.row.Job, "target", h.target)
				continue
			}
			n := notify.New(notify.Config{Secret: secret})
			m := notify.Message{Status: "job", Title: "Stream job " + h.row.Job,
				Text: fmt.Sprintf("%s = %.4g (%s, %d readings)", h.row.Field, h.row.Value, h.row.Group, h.row.Count), Report: h.row, Severity: "info"}
			n.SendTo(ctx, []*notify.Target{t}, m)
		}
	}
}

// running is a notifier with its delivery loop.
type running struct {
	n      *notify.Notifier
	cancel context.CancelFunc
}

// stop lets the queue drain, then waits for the loop to end.
func (x *running) stop() {
	x.cancel()
	<-x.n.Done()
}

// applySettings swaps in notifiers, escalation and shift settings built
// from s. Called with the settings lock held.
func (r *Runtime) applySettings(s Settings, ts map[string]*notify.Target, loc *time.Location) {
	names := func(id string) string {
		if sv, err := r.Store.Get(id); err == nil && sv.Name != "" {
			return sv.Name
		}
		return id
	}
	logf := func(f string, a ...any) { r.log.Warn(fmt.Sprintf(f, a...)) }
	var next *running
	if len(s.Notify.Targets) > 0 {
		kinds := map[string]bool{}
		for _, k := range s.Notify.Kinds {
			kinds[k] = true
		}
		n := notify.New(notify.Config{Targets: pick(ts, s.Notify.Targets), Secret: s.WebhookSecret, Kinds: kinds,
			Resolved: s.Notify.Resolved, Cooldown: time.Duration(s.Notify.CooldownMin) * time.Minute,
			PerMinute: s.Notify.PerMinute, BaseURL: r.publicURL, Names: names, Logf: logf})
		ctx, cancel := context.WithCancel(context.Background())
		go n.Run(ctx)
		next = &running{n, cancel}
	}
	if old := r.notifier.Swap(next); old != nil {
		go old.stop() // queued alerts still go out
	}
	var rep *notify.Notifier
	if len(s.Reports.Targets) > 0 {
		rep = notify.New(notify.Config{Targets: pick(ts, s.Reports.Targets), Secret: s.WebhookSecret, BaseURL: r.publicURL})
	}
	var nn *notify.Notifier
	if next != nil {
		nn = next.n
	}
	r.API.SetLive(nn, rep, s.Shifts, loc)
	r.Escalate.SetPolicy(escalationPolicy(s, ts, notify.New(notify.Config{Secret: s.WebhookSecret, BaseURL: r.publicURL, Names: names})))

	r.liveMu.Lock()
	defer r.liveMu.Unlock()
	if r.reportStop != nil {
		r.reportStop()
		r.reportStop = nil
	}
	if rep != nil && len(s.Shifts) > 0 {
		ctx, cancel := context.WithCancel(r.ctx)
		r.reportStop = cancel
		shifts := append([]string(nil), s.Shifts...)
		r.goRun(func() {
			report.Schedule(ctx, shifts, loc, func(from, to time.Time) {
				rctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				rp, err := report.Build(rctx, r.Store, r.Analytics, from.UnixMilli(), to.UnixMilli(), "", loc)
				if err != nil {
					r.log.Error("shift report", "err", err)
					return
				}
				rp.Link = r.publicURL
				res := rep.SendReport(rctx, rp.Title+" — "+rp.Period, rp.Text(loc), rp.HTML(loc), rp)
				r.log.Info("shift report sent", "period", rp.Period, "results", res)
			}, func(f string, a ...any) { r.log.Error(fmt.Sprintf(f, a...)) })
		})
	}
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
