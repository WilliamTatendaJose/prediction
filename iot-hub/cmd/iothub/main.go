// Command iothub runs the IoT hub: REST API, embedded MQTT broker, live
// stream and dashboard in one process. Configuration is via flags or the
// matching IOTHUB_* environment variables.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/williamtatendajose/prediction/iot-hub/internal/alarm"
	"github.com/williamtatendajose/prediction/iot-hub/internal/analytics"
	"github.com/williamtatendajose/prediction/iot-hub/internal/anomaly"
	"github.com/williamtatendajose/prediction/iot-hub/internal/api"
	"github.com/williamtatendajose/prediction/iot-hub/internal/auth"
	"github.com/williamtatendajose/prediction/iot-hub/internal/backup"
	"github.com/williamtatendajose/prediction/iot-hub/internal/broker"
	"github.com/williamtatendajose/prediction/iot-hub/internal/calc"
	"github.com/williamtatendajose/prediction/iot-hub/internal/connect"
	"github.com/williamtatendajose/prediction/iot-hub/internal/ingest"
	"github.com/williamtatendajose/prediction/iot-hub/internal/notify"
	"github.com/williamtatendajose/prediction/iot-hub/internal/report"
	"github.com/williamtatendajose/prediction/iot-hub/internal/store"
	"github.com/williamtatendajose/prediction/iot-hub/internal/stream"
	"github.com/williamtatendajose/prediction/iot-hub/internal/tsdb"
	"github.com/williamtatendajose/prediction/iot-hub/web"
)

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

func envDur(key string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(key)); err == nil {
		return d
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if f, err := strconv.ParseFloat(os.Getenv(key), 64); err == nil {
		return f
	}
	return def
}

func main() {
	httpAddr := flag.String("http", env("IOTHUB_HTTP", ":8080"), "HTTP listen address")
	mqttAddr := flag.String("mqtt", env("IOTHUB_MQTT", ":1883"), "MQTT TCP listen address (empty disables)")
	mqttWS := flag.String("mqtt-ws", env("IOTHUB_MQTT_WS", ""), "MQTT-over-WebSocket listen address (empty disables)")
	prefix := flag.String("prefix", env("IOTHUB_PREFIX", "iot"), "MQTT topic prefix")
	dataPath := flag.String("data", env("IOTHUB_DATA", "data/iothub.json"), "config persistence file (empty disables)")
	capacity := flag.Int("points", envInt("IOTHUB_POINTS", 1024), "readings kept in memory per sensor field")
	maxSensors := flag.Int("max-sensors", envInt("IOTHUB_MAX_SENSORS", 500), "maximum sensors")
	maxFields := flag.Int("max-fields", envInt("IOTHUB_MAX_FIELDS", 16), "maximum fields per sensor")
	autoReg := flag.Bool("auto-register", env("IOTHUB_AUTO_REGISTER", "true") == "true", "create sensors/fields on first data")
	token := flag.String("token", env("IOTHUB_TOKEN", ""), "admin token; enables authentication (HTTP Bearer / MQTT password)")
	authFile := flag.String("auth-file", env("IOTHUB_AUTH_FILE", "data/devices.json"), "per-device credentials (token hashes)")
	publicRead := flag.Bool("public-read", env("IOTHUB_PUBLIC_READ", "") == "true", "allow reads without a token")
	tlsCert := flag.String("tls-cert", env("IOTHUB_TLS_CERT", ""), "TLS certificate (PEM); enables HTTPS and MQTT over TLS")
	tlsKey := flag.String("tls-key", env("IOTHUB_TLS_KEY", ""), "TLS private key (PEM)")
	mqttTLS := flag.String("mqtts", env("IOTHUB_MQTTS", ":8883"), "MQTT over TLS address (used when -tls-cert is set)")
	dbURL := flag.String("db", env("IOTHUB_DB", "data/readings.db"), "database: path.db, sqlite:path, postgres://... (empty = memory only)")
	rawKeep := flag.Duration("raw-retention", envDur("IOTHUB_RAW_RETENTION", 7*24*time.Hour), "keep raw readings this long (0 = forever)")
	rollupKeep := flag.Duration("rollup-retention", envDur("IOTHUB_ROLLUP_RETENTION", 365*24*time.Hour), "keep 1-minute rollups this long (0 = forever)")
	anomalyOn := flag.Bool("anomaly", env("IOTHUB_ANOMALY", "true") == "true", "streaming anomaly detection")
	anomalyZ := flag.Float64("anomaly-z", envFloat("IOTHUB_ANOMALY_Z", 5), "spike threshold in standard deviations")
	anomalyWindow := flag.Int("anomaly-window", envInt("IOTHUB_ANOMALY_WINDOW", 300), "EWMA span in samples")
	anomalyPersist := flag.Int("anomaly-persist", envInt("IOTHUB_ANOMALY_PERSIST", 2), "consecutive outliers before a spike opens")
	anomalyWarmup := flag.Int("anomaly-warmup", envInt("IOTHUB_ANOMALY_WARMUP", 30), "samples before spike detection starts")
	staleMin := flag.Duration("stale-min", envDur("IOTHUB_STALE_MIN", time.Minute), "minimum silence before a sensor is stale (0 disables)")
	var notifySpecs []string
	for _, f := range strings.Fields(os.Getenv("IOTHUB_NOTIFY")) {
		notifySpecs = append(notifySpecs, f)
	}
	flag.Func("notify", "notification target kind=URL (repeatable; or IOTHUB_NOTIFY, space-separated). kinds: webhook slack teams discord telegram email", func(v string) error {
		notifySpecs = append(notifySpecs, v)
		return nil
	})
	reportSpecs := strings.Fields(os.Getenv("IOTHUB_REPORT_TO"))
	flag.Func("report-to", "shift report target kind=URL, same kinds as -notify (repeatable; or IOTHUB_REPORT_TO). Sent at every shift change", func(v string) error {
		reportSpecs = append(reportSpecs, v)
		return nil
	})
	backupDir := flag.String("backup-dir", env("IOTHUB_BACKUP_DIR", ""), "directory for scheduled backups: SQLite snapshot + config (empty = off; use another disk)")
	backupEvery := flag.Duration("backup-every", envDur("IOTHUB_BACKUP_EVERY", 24*time.Hour), "time between scheduled backups")
	backupKeep := flag.Int("backup-keep", envInt("IOTHUB_BACKUP_KEEP", 7), "backups of each kind to keep")
	notifySecret := flag.String("notify-secret", env("IOTHUB_NOTIFY_SECRET", ""), "HMAC secret for generic webhook signatures")
	notifyKinds := flag.String("notify-kinds", env("IOTHUB_NOTIFY_KINDS", ""), "anomaly kinds to notify, comma-separated (empty = all)")
	notifyResolved := flag.Bool("notify-resolved", env("IOTHUB_NOTIFY_RESOLVED", "true") == "true", "also notify when an episode resolves")
	notifyCooldown := flag.Duration("notify-cooldown", envDur("IOTHUB_NOTIFY_COOLDOWN", 10*time.Minute), "hold repeat alerts for the same sensor/field/kind")
	notifyRate := flag.Int("notify-per-minute", envInt("IOTHUB_NOTIFY_PER_MINUTE", 20), "maximum notifications per minute")
	publicURL := flag.String("public-url", env("IOTHUB_PUBLIC_URL", ""), "dashboard URL used in notification links")
	shifts := flag.String("shifts", env("IOTHUB_SHIFTS", "06:00,14:00,22:00"), "shift start times for OEE (comma-separated HH:MM; empty disables)")
	tz := flag.String("tz", env("IOTHUB_TZ", ""), "plant time zone for shifts and daily reports, e.g. Africa/Harare (default: system)")
	connectorsFile := flag.String("connectors", env("IOTHUB_CONNECTORS", ""), "Modbus/OPC UA connectors config (JSON); empty disables")
	debug := flag.Bool("debug", env("IOTHUB_DEBUG", "") == "true", "debug logging")
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	st := store.New(store.Options{
		Path: *dataPath, Capacity: *capacity, MaxSensors: *maxSensors,
		MaxFields: *maxFields, AutoRegister: *autoReg,
	})
	if err := st.Load(); err != nil {
		log.Error("load config", "err", err)
		os.Exit(1)
	}
	log.Info("memory bound for readings",
		"maxMB", float64(*maxSensors**maxFields**capacity*12)/(1<<20))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	hub := stream.NewHub(64)
	pipe := &ingest.Pipeline{Store: st, Hub: hub}
	an := &analytics.Service{Store: st}
	creds := auth.New(*authFile, *token)
	if err := creds.Load(); err != nil {
		log.Error("load credentials", "err", err)
		os.Exit(1)
	}
	if !creds.Enabled() {
		log.Warn("AUTHENTICATION IS OFF: anyone who can reach this hub can read and write. Set -token to enable it.")
	}
	var tlsCfg *tls.Config
	if *tlsCert != "" {
		cert, err := tls.LoadX509KeyPair(*tlsCert, *tlsKey)
		if err != nil {
			log.Error("tls", "err", err)
			os.Exit(1)
		}
		tlsCfg = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		if *mqttAddr != "" {
			log.Warn("plain MQTT is still enabled; tokens cross the network unencrypted. Use -mqtt \"\" to serve MQTT over TLS only")
		}
	}
	if *tz != "" {
		loc, err := time.LoadLocation(*tz)
		if err != nil {
			log.Error("tz", "err", err)
			os.Exit(1)
		}
		time.Local = loc
	}
	var shiftStarts []string
	for _, s := range strings.Split(*shifts, ",") {
		if s = strings.TrimSpace(s); s != "" {
			if _, err := time.Parse("15:04", s); err != nil {
				log.Error("shifts", "err", fmt.Errorf("bad shift start %q (want HH:MM)", s))
				os.Exit(1)
			}
			shiftStarts = append(shiftStarts, s)
		}
	}
	srv := &api.Server{Store: st, Hub: hub, Pipeline: pipe, Web: web.FS, Analytics: an, Shifts: shiftStarts,
		Auth: creds, PublicRead: *publicRead, SecureCookies: tlsCfg != nil, TLS: tlsCfg != nil, PublicURL: *publicURL}

	var writer *tsdb.Writer
	stopWriter := func() {}
	stopNotifier := func() {}
	if *dbURL != "" {
		if p, ok := strings.CutPrefix(*dbURL, "sqlite:"); ok || !strings.Contains(*dbURL, "://") {
			if p == "" {
				p = *dbURL
			}
			_ = os.MkdirAll(filepath.Dir(p), 0o755)
		}
		db, err := tsdb.Open(ctx, *dbURL)
		if err != nil {
			log.Error("database", "err", err)
			os.Exit(1)
		}
		defer db.Close()
		_ = db.CloseOpenEvents(ctx, time.Now().UnixMilli()) // episodes cut short by the last shutdown
		writer = tsdb.NewWriter(db, 16384)
		writer.RawRetention, writer.RollupRetention = *rawKeep, *rollupKeep
		writer.Logf = func(f string, a ...any) { log.Error(fmt.Sprintf(f, a...)) }
		// The writer outlives ctx: it is stopped only after the HTTP server and
		// broker have closed, so in-flight readings still reach the database.
		wctx, wstop := context.WithCancel(context.Background())
		stopWriter = wstop
		go writer.Run(wctx)
		pipe.Writer, an.DB, srv.Writer = writer, db, writer
		log.Info("database", "url", redact(*dbURL), "rawRetention", *rawKeep, "rollupRetention", *rollupKeep)
	}

	// Alarm handling: acks, notes, shelves, audit log (persisted with a DB).
	var alarmStore tsdb.AlarmStore
	if an.DB != nil {
		alarmStore = an.DB
	}
	alarms := alarm.New(alarmStore)
	if err := alarms.Load(ctx); err != nil {
		log.Error("load alarm state", "err", err)
		os.Exit(1)
	}
	alarms.OnChange = func(kind string) {
		hub.Publish(&stream.Msg{Event: "alarms", Data: []byte(`{"kind":"` + kind + `"}`)})
	}
	pipe.Alarms, an.Alarms, srv.Alarms = alarms, alarms, alarms
	pipe.Calc = calc.NewEngine()
	pipe.Calc.Errors = func(sensor, field string, err error) {
		log.Debug("calculated field", "sensor", sensor, "field", field, "err", err)
	}

	var notifier *notify.Notifier
	if len(notifySpecs) > 0 {
		var targets []*notify.Target
		for _, spec := range notifySpecs {
			t, err := notify.ParseTarget(spec)
			if err != nil {
				log.Error("notify", "err", err)
				os.Exit(1)
			}
			targets = append(targets, t)
		}
		kinds := map[string]bool{}
		for _, k := range strings.Split(*notifyKinds, ",") {
			if k = strings.TrimSpace(k); k != "" {
				kinds[k] = true
			}
		}
		notifier = notify.New(notify.Config{
			Targets: targets, Secret: *notifySecret, Kinds: kinds, Resolved: *notifyResolved,
			Cooldown: *notifyCooldown, PerMinute: *notifyRate, BaseURL: *publicURL,
			Names: func(id string) string {
				if sv, err := st.Get(id); err == nil && sv.Name != "" {
					return sv.Name
				}
				return id
			},
			Logf: func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...)) },
		})
		// Like the DB writer, it outlives ctx so alerts raised during
		// shutdown still go out.
		nctx, nstop := context.WithCancel(context.Background())
		stopNotifier = nstop
		go notifier.Run(nctx)
		srv.Notifier = notifier
		pipe.OnEvent = notifier.Notify
		names := make([]string, len(targets))
		for i, t := range targets {
			names[i] = t.Redacted()
		}
		log.Info("notifications", "targets", names)
	}

	if *backupDir != "" {
		if *backupEvery < time.Minute {
			log.Error("backup-every must be at least 1m")
			os.Exit(2)
		}
		srv.Backups = &backup.Scheduler{Dir: *backupDir, Every: *backupEvery, Keep: *backupKeep, DB: an.DB,
			Config: func() backup.Config { return backup.Export(st, creds, true) },
			Logf:   func(f string, a ...any) { log.Info(fmt.Sprintf(f, a...)) }}
		go srv.Backups.Run(ctx)
		snap := "config only (database snapshots need SQLite; use pg_dump)"
		if sn, ok := an.DB.(tsdb.Snapshotter); ok && sn.CanSnapshot() {
			snap = "database + config"
		} else if an.DB == nil {
			snap = "config only (no database)"
		}
		log.Info("backups", "dir", *backupDir, "every", *backupEvery, "keep", *backupKeep, "contents", snap)
	}

	if len(reportSpecs) > 0 {
		var targets []*notify.Target
		names := make([]string, 0, len(reportSpecs))
		for _, spec := range reportSpecs {
			t, err := notify.ParseTarget(spec)
			if err != nil {
				log.Error("report-to", "err", err)
				os.Exit(2)
			}
			targets = append(targets, t)
			names = append(names, t.Redacted())
		}
		srv.Reporter = notify.New(notify.Config{Targets: targets, Secret: *notifySecret, BaseURL: *publicURL})
		if len(shiftStarts) > 0 {
			go report.Schedule(ctx, shiftStarts, time.Local, func(from, to time.Time) {
				rctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				rep, err := report.Build(rctx, st, an, from.UnixMilli(), to.UnixMilli(), "", time.Local)
				if err != nil {
					log.Error("shift report", "err", err)
					return
				}
				rep.Link = *publicURL
				res := srv.Reporter.SendReport(rctx, rep.Title+" — "+rep.Period, rep.Text(time.Local), rep.HTML(time.Local), rep)
				log.Info("shift report sent", "period", rep.Period, "results", res)
			}, func(f string, a ...any) { log.Error(fmt.Sprintf(f, a...)) })
		}
		log.Info("shift reports", "targets", names, "shifts", shiftStarts)
	}

	if *anomalyOn {
		det := anomaly.New(anomaly.Config{Z: *anomalyZ, Window: *anomalyWindow, Warmup: *anomalyWarmup, Persist: *anomalyPersist, StaleMin: *staleMin})
		pipe.Detector, an.Detector, srv.Detector = det, det, det
	}
	persistDone := make(chan struct{})
	go func() {
		st.RunPersist(ctx, time.Second, func(f string, a ...any) { log.Error(fmt.Sprintf(f, a...)) })
		close(persistDone)
	}()

	var mq *broker.Broker
	// Restore live state from the database before any transport starts, so
	// tiles show the last known values (and recent history) immediately
	// after a restart instead of staying blank until each sensor reports.
	if an.DB != nil {
		restoreLive(ctx, an.DB, st, pipe.Detector, *capacity, log)
	}

	mqttsAddr := ""
	if tlsCfg != nil {
		mqttsAddr = *mqttTLS
	}
	if *mqttAddr != "" || *mqttWS != "" || mqttsAddr != "" {
		var err error
		mq, err = broker.New(broker.Config{
			TCPAddr: *mqttAddr, WSAddr: *mqttWS, TLSAddr: mqttsAddr, TLS: tlsCfg, Prefix: *prefix, Auth: creds,
			Logger: log.With("component", "mqtt"),
		}, pipe)
		if err != nil {
			log.Error("mqtt", "err", err)
			os.Exit(1)
		}
		srv.OnIngest = mq.Republish
		srv.OnRevoke = func(id string) { mq.Kick(id) }
		if notifier != nil {
			pipe.OnEvent = func(e anomaly.Event) { mq.PublishEvent(e); notifier.Notify(e) }
		} else {
			pipe.OnEvent = mq.PublishEvent
		}
		if err := mq.Serve(); err != nil {
			log.Error("mqtt serve", "err", err)
			os.Exit(1)
		}
		log.Info("mqtt listening", "tcp", *mqttAddr, "tls", mqttsAddr, "ws", *mqttWS, "topics", *prefix+"/{sensor}[/{field}]")
	}

	if *connectorsFile != "" {
		cc, err := connect.Load(*connectorsFile)
		if err != nil {
			log.Error("connectors", "err", err)
			os.Exit(1)
		}
		for i := range cc.OPCUA {
			cc.OPCUA[i].CertDir = filepath.Dir(*authFile)
		}
		clog := log.With("component", "connect")
		conns := connect.Start(ctx, cc, connect.Target{
			Ingest: func(sensor string, ts int64, values map[string]any) {
				r, err := pipe.HandleValues(sensor, ts, values)
				if err != nil {
					clog.Debug("ingest rejected", "sensor", sensor, "err", err)
					return
				}
				if srv.OnIngest != nil {
					srv.OnIngest(r) // PLC data appears on MQTT like any other reading
				}
			},
			Define: func(sensor string, fields map[string]connect.FieldInfo) {
				defineFields(st, sensor, fields, clog)
			},
			Logf: func(f string, a ...any) { clog.Warn(fmt.Sprintf(f, a...)) },
		})
		srv.Connectors = func() []connect.Status {
			out := make([]connect.Status, len(conns))
			for i, c := range conns {
				out[i] = c.Status()
			}
			return out
		}
		log.Info("connectors started", "modbus", len(cc.Modbus), "opcua", len(cc.OPCUA))
	}

	// Started only after the pipeline is fully wired (OnEvent above).
	if det := pipe.Detector; det != nil {
		go func() {
			t := time.NewTicker(10 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case now := <-t.C:
					pipe.Emit(det.CheckStale(now.UnixMilli()))
				}
			}
		}()
	}

	httpSrv := &http.Server{
		Addr:              *httpAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		// Request contexts end on shutdown, so open SSE streams close promptly.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	go func() {
		log.Info("http listening", "addr", *httpAddr)
		var err error
		if tlsCfg != nil {
			httpSrv.TLSConfig = tlsCfg
			err = httpSrv.ListenAndServeTLS("", "")
		} else {
			err = httpSrv.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutCtx)
	if mq != nil {
		_ = mq.Close()
	}
	<-persistDone
	stopNotifier()
	if srv.Notifier != nil {
		<-srv.Notifier.Done()
	}
	stopWriter()
	if writer != nil {
		<-writer.Done() // final flush before the deferred db.Close
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

// defineFields adds units/labels declared by a connector to the sensor
// definition, without overwriting anything an admin has set.
func defineFields(st *store.Store, sensor string, fields map[string]connect.FieldInfo, log *slog.Logger) {
	def := store.Sensor{ID: sensor, Name: sensor, Kind: "plc", Fields: map[string]store.Field{}}
	if cur, err := st.Get(sensor); err == nil {
		def = cur.Sensor
	}
	for name, fi := range fields {
		f := def.Fields[name]
		if f.Unit == "" {
			f.Unit = fi.Unit
		}
		if f.Label == "" {
			f.Label = fi.Label
		}
		def.Fields[name] = f
	}
	if err := st.Upsert(def); err != nil {
		log.Warn("define sensor", "sensor", sensor, "err", err)
	}
}

// redact hides a password in a database URL for logging.
func redact(u string) string {
	if p, err := url.Parse(u); err == nil && p.User != nil {
		if _, ok := p.User.Password(); ok {
			p.User = url.UserPassword(p.User.Username(), "xxx")
			return p.String()
		}
	}
	return u
}
