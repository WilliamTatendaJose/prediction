// Command iothub runs the IoT hub: REST API, embedded MQTT broker, live
// stream and dashboard in one process. Configuration is via flags or the
// matching IOTHUB_* environment variables.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/williamtatendajose/prediction/iot-hub/internal/api"
	"github.com/williamtatendajose/prediction/iot-hub/internal/broker"
	"github.com/williamtatendajose/prediction/iot-hub/internal/ingest"
	"github.com/williamtatendajose/prediction/iot-hub/internal/store"
	"github.com/williamtatendajose/prediction/iot-hub/internal/stream"
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
	token := flag.String("token", env("IOTHUB_TOKEN", ""), "shared secret for writes (HTTP Bearer / MQTT password)")
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

	hub := stream.NewHub(64)
	pipe := &ingest.Pipeline{Store: st, Hub: hub}
	srv := &api.Server{Store: st, Hub: hub, Pipeline: pipe, Token: *token, Web: web.FS}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	persistDone := make(chan struct{})
	go func() {
		st.RunPersist(ctx, time.Second, func(f string, a ...any) { log.Error(fmt.Sprintf(f, a...)) })
		close(persistDone)
	}()

	var mq *broker.Broker
	if *mqttAddr != "" || *mqttWS != "" {
		var err error
		mq, err = broker.New(broker.Config{
			TCPAddr: *mqttAddr, WSAddr: *mqttWS, Prefix: *prefix, Token: *token,
			Logger: log.With("component", "mqtt"),
		}, pipe)
		if err != nil {
			log.Error("mqtt", "err", err)
			os.Exit(1)
		}
		srv.OnIngest = mq.Republish
		if err := mq.Serve(); err != nil {
			log.Error("mqtt serve", "err", err)
			os.Exit(1)
		}
		log.Info("mqtt listening", "tcp", *mqttAddr, "ws", *mqttWS, "topics", *prefix+"/{sensor}[/{field}]")
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
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
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
}
