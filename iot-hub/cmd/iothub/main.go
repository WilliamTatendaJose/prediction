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

	"github.com/williamtatendajose/prediction/iot-hub/internal/analytics"
	"github.com/williamtatendajose/prediction/iot-hub/internal/anomaly"
	"github.com/williamtatendajose/prediction/iot-hub/internal/api"
	"github.com/williamtatendajose/prediction/iot-hub/internal/auth"
	"github.com/williamtatendajose/prediction/iot-hub/internal/broker"
	"github.com/williamtatendajose/prediction/iot-hub/internal/ingest"
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
	srv := &api.Server{Store: st, Hub: hub, Pipeline: pipe, Web: web.FS, Analytics: an,
		Auth: creds, PublicRead: *publicRead, SecureCookies: tlsCfg != nil, TLS: tlsCfg != nil}

	var writer *tsdb.Writer
	stopWriter := func() {}
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
		pipe.OnEvent = mq.PublishEvent
		if err := mq.Serve(); err != nil {
			log.Error("mqtt serve", "err", err)
			os.Exit(1)
		}
		log.Info("mqtt listening", "tcp", *mqttAddr, "tls", mqttsAddr, "ws", *mqttWS, "topics", *prefix+"/{sensor}[/{field}]")
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
	stopWriter()
	if writer != nil {
		<-writer.Done() // final flush before the deferred db.Close
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
