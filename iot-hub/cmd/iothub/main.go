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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/williamtatendajose/prediction/iot-hub/internal/anomaly"
	"github.com/williamtatendajose/prediction/iot-hub/internal/api"
	"github.com/williamtatendajose/prediction/iot-hub/internal/auth"
	"github.com/williamtatendajose/prediction/iot-hub/internal/broker"
	"github.com/williamtatendajose/prediction/iot-hub/internal/connect"
	"github.com/williamtatendajose/prediction/iot-hub/internal/forward"
	"github.com/williamtatendajose/prediction/iot-hub/internal/gateway"
	"github.com/williamtatendajose/prediction/iot-hub/internal/ingest"
	"github.com/williamtatendajose/prediction/iot-hub/internal/notify"
	"github.com/williamtatendajose/prediction/iot-hub/internal/store"
	"github.com/williamtatendajose/prediction/iot-hub/internal/tenant"
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
	tenancy := flag.String("tenancy", env("IOTHUB_TENANCY", "single"), "single (one hub) or multi (SaaS: isolated tenants, -token is the superadmin)")
	masterKey := flag.String("master-key", env("IOTHUB_MASTER_KEY", ""), "encrypts device keys at rest (long random secret; keep it outside the data directory)")
	tenantRate := flag.Float64("tenant-rate", envFloat("IOTHUB_TENANT_RATE", 100), "multi: default messages per second per tenant (0 = unlimited)")
	tenantDaily := flag.Int64("tenant-daily", int64(envInt("IOTHUB_TENANT_DAILY", 0)), "multi: default messages per day per tenant (0 = unlimited)")
	allowPrivate := flag.Bool("allow-private-targets", env("IOTHUB_ALLOW_PRIVATE_TARGETS", "") == "true", "multi: let tenants send notifications to private/internal addresses (off: blocked against SSRF)")
	fwdTo := flag.String("forward", env("IOTHUB_FORWARD", ""), "edge: forward readings to a cloud hub — a connection string (HostName=…;TenantId=…;DeviceId=…;SharedAccessKey=…) or a URL with -forward-token")
	fwdToken := flag.String("forward-token", env("IOTHUB_FORWARD_TOKEN", ""), "edge: bearer token for -forward URL")
	fwdDir := flag.String("forward-dir", env("IOTHUB_FORWARD_DIR", ""), "edge: queue directory (default next to -data)")
	fwdMax := flag.Int("forward-max-mb", envInt("IOTHUB_FORWARD_MAX_MB", 1024), "edge: queue cap on disk; beyond it the oldest unsent readings are dropped")
	fwdSensors := flag.String("forward-sensors", env("IOTHUB_FORWARD_SENSORS", "*"), "edge: which sensors to forward (glob)")
	fwdPrefix := flag.String("forward-prefix", env("IOTHUB_FORWARD_PREFIX", ""), "edge: prefix for sensor ids in the cloud, e.g. plant1. (keeps sites apart)")
	tenantDevices := flag.Int("tenant-devices", envInt("IOTHUB_TENANT_DEVICES", 1000), "multi: default maximum identities per tenant (0 = unlimited)")
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	log.Info("memory bound for readings per tenant",
		"maxMB", float64(*maxSensors**maxFields**capacity*12)/(1<<20))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	multi := *tenancy == "multi"
	if !multi && *tenancy != "single" {
		fatal(log, "tenancy", errors.New("must be single or multi"))
	}
	creds := auth.New(*authFile, *token)
	if err := creds.SetMasterKey(*masterKey); err != nil {
		fatal(log, "master key", err)
	}
	if err := creds.Load(); err != nil {
		fatal(log, "load credentials", err)
	}
	if *masterKey == "" && multi {
		log.Warn("no -master-key: device keys (SAS) are stored unencrypted in the credentials file (mode 0600)")
	}
	if !creds.Enabled() {
		log.Warn("AUTHENTICATION IS OFF: anyone who can reach this hub can read and write. Set -token to enable it.")
	}
	var tlsCfg *tls.Config
	if *tlsCert != "" {
		cert, err := tls.LoadX509KeyPair(*tlsCert, *tlsKey)
		if err != nil {
			fatal(log, "tls", err)
		}
		tlsCfg = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		if *mqttAddr != "" {
			log.Warn("plain MQTT is still enabled; tokens cross the network unencrypted. Use -mqtt \"\" to serve MQTT over TLS only")
		}
	}
	if *tz != "" {
		loc, err := time.LoadLocation(*tz)
		if err != nil {
			fatal(log, "tz", err)
		}
		time.Local = loc
	}
	var shiftStarts []string
	for _, s := range strings.Split(*shifts, ",") {
		if s = strings.TrimSpace(s); s != "" {
			if _, err := time.Parse("15:04", s); err != nil {
				fatal(log, "shifts", fmt.Errorf("bad shift start %q (want HH:MM)", s))
			}
			shiftStarts = append(shiftStarts, s)
		}
	}
	if *backupDir != "" && *backupEvery < time.Minute {
		fatal(log, "backup", errors.New("-backup-every must be at least 1m"))
	}

	// The broker is created after the tenants (it needs their pipelines);
	// tenants reach it through these hooks.
	var mq *broker.Broker
	hooks := tenant.Hooks{
		Publish: func(t string, r store.Reading) {
			if mq != nil {
				mq.RepublishTo(t, r)
			}
		},
		PublishEvent: func(t string, e anomaly.Event) {
			if mq != nil {
				mq.PublishEventTo(t, e)
			}
		},
		Kick: func(t, id string) {
			if mq != nil {
				mq.KickTenant(t, id)
			}
		},
	}
	base := tenant.Options{
		Capacity: *capacity, MaxSensors: *maxSensors, MaxFields: *maxFields, AutoRegister: *autoReg,
		RawRetention: *rawKeep, RollupRetention: *rollupKeep,
		Anomaly:   *anomalyOn,
		AnomalyCf: anomaly.Config{Z: *anomalyZ, Window: *anomalyWindow, Warmup: *anomalyWarmup, Persist: *anomalyPersist, StaleMin: *staleMin},
		Seed:      tenant.Settings{Shifts: shiftStarts, TimeZone: *tz},
		PublicURL: *publicURL,
		BackupDir: *backupDir, BackupEvery: *backupEvery, BackupKeep: *backupKeep,
		SecureCookies: tlsCfg != nil, TLS: tlsCfg != nil,
		Hooks: hooks, Logger: log,
	}

	var handler http.Handler
	var resolver broker.Resolver
	var pipe *ingest.Pipeline
	var closeTenants func()
	brokerAuth := creds
	if multi {
		if *token == "" {
			fatal(log, "tenancy", errors.New("multi-tenant mode needs -token (the superadmin token)"))
		}
		if *publicRead || *connectorsFile != "" || len(notifySpecs) > 0 || len(reportSpecs) > 0 || *fwdTo != "" {
			fatal(log, "tenancy", errors.New("-public-read, -connectors, -notify, -report-to and -forward are single-tenant (edge) options: tenants configure notifications themselves, and PLC connectors run on an edge hub that forwards to its tenant"))
		}
		notify.BlockPrivateNetworks(!*allowPrivate)
		plat := &tenant.Platform{
			Dir: filepath.Join(filepath.Dir(*dataPath), "tenants"), DBURL: *dbURL, Base: base, Creds: creds,
			Default: tenant.Quota{MessagesPerSecond: *tenantRate, MessagesPerDay: *tenantDaily, MaxDevices: *tenantDevices},
			OnStop: func(id string) {
				if mq != nil {
					mq.KickTenant(id, "")
				}
			},
		}
		if err := plat.Load(ctx); err != nil {
			fatal(log, "tenants", err)
		}
		static, err := api.StaticHandler(web.FS)
		if err != nil {
			fatal(log, "dashboard", err)
		}
		gw := &gateway.Gateway{Platform: plat, Creds: creds.Platform(), Static: static,
			SecureCookies: tlsCfg != nil, TLS: tlsCfg != nil, Logger: log}
		handler, resolver, closeTenants = gw.Handler(), platformResolver{plat}, plat.Close
		brokerAuth = creds.Platform()
		log.Info("multi-tenant", "tenants", len(plat.List()), "dir", plat.Dir)
	} else {
		o := base
		o.ConfigPath, o.DBURL, o.Creds, o.PublicRead = *dataPath, *dbURL, creds, *publicRead
		o.Web = web.FS
		// Flags seed the settings; once an admin saves settings through
		// the API, settings.json is the source and these flags are ignored.
		o.SettingsPath = filepath.Join(filepath.Dir(*dataPath), "settings.json")
		o.JobsPath = filepath.Join(filepath.Dir(*dataPath), "jobs.json")
		seed := &o.Seed
		seed.WebhookSecret = *notifySecret
		seed.Notify = tenant.NotifyCfg{Resolved: *notifyResolved, CooldownMin: int(notifyCooldown.Minutes()), PerMinute: *notifyRate}
		for _, k := range strings.Split(*notifyKinds, ",") {
			if k = strings.TrimSpace(k); k != "" {
				seed.Notify.Kinds = append(seed.Notify.Kinds, k)
			}
		}
		for i, spec := range notifySpecs {
			id := fmt.Sprintf("notify-%d", i+1)
			seed.Targets = append(seed.Targets, tenant.TargetSpec{ID: id, Spec: spec})
			seed.Notify.Targets = append(seed.Notify.Targets, id)
		}
		for i, spec := range reportSpecs {
			id := fmt.Sprintf("report-%d", i+1)
			seed.Targets = append(seed.Targets, tenant.TargetSpec{ID: id, Spec: spec})
			seed.Reports.Targets = append(seed.Reports.Targets, id)
		}
		if _, err := os.Stat(o.SettingsPath); err == nil && (len(notifySpecs) > 0 || len(reportSpecs) > 0) {
			log.Warn("settings.json exists: -notify and -report-to are ignored (manage targets with /api/settings)", "file", o.SettingsPath)
		}
		var fwd *forward.Forwarder
		if *fwdTo != "" {
			fc := forward.Config{URL: *fwdTo, Token: *fwdToken}
			if strings.Contains(*fwdTo, "SharedAccessKey=") {
				var err error
				if fc, err = forward.ParseConnectionString(*fwdTo); err != nil {
					fatal(log, "forward", err)
				}
			} else if *fwdToken == "" {
				fatal(log, "forward", errors.New("a -forward URL needs -forward-token (or use a connection string)"))
			}
			fc.Dir, fc.MaxBytes, fc.Sensors, fc.Prefix = *fwdDir, int64(*fwdMax)<<20, *fwdSensors, *fwdPrefix
			if fc.Dir == "" {
				fc.Dir = filepath.Join(filepath.Dir(*dataPath), "forward")
			}
			fc.Logf = func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...)) }
			var err error
			if fwd, err = forward.New(fc); err != nil {
				fatal(log, "forward", err)
			}
			o.Forward = fwd.Add
			o.ForwardStatus = func() any { return fwd.Status() }
			log.Info("forwarding to the cloud", "url", fc.URL, "tenant", fc.Tenant, "as", fc.DeviceID, "queue", fc.Dir, "maxMB", *fwdMax)
		}
		rt, err := tenant.Open(ctx, auth.DefaultTenant, o)
		if err != nil {
			fatal(log, "start", err)
		}
		if *dbURL != "" {
			log.Info("database", "url", redact(*dbURL), "rawRetention", *rawKeep, "rollupRetention", *rollupKeep)
		}
		if *backupDir != "" {
			log.Info("backups", "dir", *backupDir, "every", *backupEvery, "keep", *backupKeep)
		}
		handler, pipe, closeTenants = rt.Handler, rt.Pipe, rt.Close
		if fwd != nil {
			fctx, fstop := context.WithCancel(context.Background())
			go fwd.Run(fctx)
			closeTenants = func() { rt.Close(); fstop(); <-fwd.Done() } // flush the queue to disk last
		}
		if *connectorsFile != "" {
			startConnectors(ctx, *connectorsFile, filepath.Dir(*authFile), rt, log)
		}
	}

	mqttsAddr := ""
	if tlsCfg != nil {
		mqttsAddr = *mqttTLS
	}
	if *mqttAddr != "" || *mqttWS != "" || mqttsAddr != "" {
		var err error
		mq, err = broker.New(broker.Config{
			TCPAddr: *mqttAddr, WSAddr: *mqttWS, TLSAddr: mqttsAddr, TLS: tlsCfg, Prefix: *prefix, Auth: brokerAuth,
			Tenants: resolver, Logger: log.With("component", "mqtt"),
		}, pipe)
		if err != nil {
			fatal(log, "mqtt", err)
		}
		if err := mq.Serve(); err != nil {
			fatal(log, "mqtt serve", err)
		}
		topics := *prefix + "/{sensor}[/{field}]"
		if multi {
			topics = "{tenant}/" + topics + " (username {tenant}/{id})"
		}
		log.Info("mqtt listening", "tcp", *mqttAddr, "tls", mqttsAddr, "ws", *mqttWS, "topics", topics)
	}

	httpSrv := &http.Server{
		Addr:              *httpAddr,
		Handler:           handler,
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
	closeTenants() // final config saves, notifier drain, database flush
}

func fatal(log *slog.Logger, what string, err error) {
	log.Error(what, "err", err)
	os.Exit(1)
}

func redactAll(ts []*notify.Target) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Redacted()
	}
	return out
}

// platformResolver gives the broker each active tenant's pipeline.
type platformResolver struct{ p *tenant.Platform }

func (r platformResolver) Pipeline(t string) (*ingest.Pipeline, error) {
	rt, err := r.p.Runtime(t)
	if err != nil {
		return nil, err
	}
	if rt == nil {
		return nil, tenant.ErrNotFound
	}
	return rt.Pipe, nil
}

func startConnectors(ctx context.Context, file, certDir string, rt *tenant.Runtime, log *slog.Logger) {
	cc, err := connect.Load(file)
	if err != nil {
		fatal(log, "connectors", err)
	}
	for i := range cc.OPCUA {
		cc.OPCUA[i].CertDir = certDir
	}
	clog := log.With("component", "connect")
	conns := connect.Start(ctx, cc, connect.Target{
		Ingest: func(sensor string, ts int64, values map[string]any) {
			r, err := rt.Pipe.HandleValues(sensor, ts, values)
			if err != nil {
				clog.Debug("ingest rejected", "sensor", sensor, "err", err)
				return
			}
			if rt.API.OnIngest != nil {
				rt.API.OnIngest(r) // PLC data appears on MQTT like any other reading
			}
		},
		Define: func(sensor string, fields map[string]connect.FieldInfo) {
			defineFields(rt.Store, sensor, fields, clog)
		},
		Logf: func(f string, a ...any) { clog.Warn(fmt.Sprintf(f, a...)) },
	})
	rt.API.Connectors = func() []connect.Status {
		out := make([]connect.Status, len(conns))
		for i, c := range conns {
			out[i] = c.Status()
		}
		return out
	}
	log.Info("connectors started", "modbus", len(cc.Modbus), "opcua", len(cc.OPCUA))
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
