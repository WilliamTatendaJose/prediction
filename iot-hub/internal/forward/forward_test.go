package forward_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/williamtatendajose/prediction/iot-hub/internal/analytics"
	"github.com/williamtatendajose/prediction/iot-hub/internal/api"
	"github.com/williamtatendajose/prediction/iot-hub/internal/auth"
	"github.com/williamtatendajose/prediction/iot-hub/internal/forward"
	"github.com/williamtatendajose/prediction/iot-hub/internal/ingest"
	"github.com/williamtatendajose/prediction/iot-hub/internal/store"
	"github.com/williamtatendajose/prediction/iot-hub/internal/stream"
)

// cloud is a real hub API with a keys device for the edge, a record of
// every reading it ingested (to catch duplicates and gaps), and switches
// to inject faults between edge and cloud.
type cloud struct {
	url   string
	creds *auth.Store
	key   string

	mu   sync.Mutex
	seen map[string]int // sensor@ts -> times ingested
	ts   []int64        // arrival order

	down      atomic.Bool  // answer 503
	dropReply atomic.Int32 // process, then drop the connection (lost reply)
	quota     atomic.Int64 // messages left; <0 unlimited
	requests  atomic.Int64
}

func newCloud(t *testing.T) *cloud {
	c := &cloud{seen: map[string]int{}}
	c.quota.Store(-1)
	st := store.New(store.Options{AutoRegister: true, Capacity: 16, MaxSensors: 50})
	pipe := &ingest.Pipeline{Store: st, Hub: stream.NewHub(4)}
	pipe.Observe = func(r store.Reading) {
		c.mu.Lock()
		c.seen[fmt.Sprintf("%s@%d", r.Sensor, r.TS)]++
		c.ts = append(c.ts, r.TS)
		c.mu.Unlock()
	}
	pipe.Admit = func() error {
		for {
			q := c.quota.Load()
			if q < 0 {
				return nil
			}
			if q == 0 {
				return ingest.ErrQuota
			}
			if c.quota.CompareAndSwap(q, q-1) {
				return nil
			}
		}
	}
	c.creds = auth.New(filepath.Join(t.TempDir(), "c.json"), "super").Tenant("acme")
	sec, err := c.creds.Create(auth.Spec{ID: "edge-1", Role: auth.Service, Sensors: []string{"plant1.*"}, Auth: "keys"})
	if err != nil {
		t.Fatal(err)
	}
	c.key = sec.PrimaryKey
	srv := &api.Server{Store: st, Hub: pipe.Hub, Pipeline: pipe, Auth: c.creds, Analytics: &analytics.Service{Store: st}}
	h := srv.Handler()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.requests.Add(1)
		if c.down.Load() {
			http.Error(w, `{"error":"maintenance"}`, http.StatusServiceUnavailable)
			return
		}
		if c.dropReply.Load() > 0 {
			c.dropReply.Add(-1)
			h.ServeHTTP(httptest.NewRecorder(), r) // processed…
			if hj, ok := w.(http.Hijacker); ok {   // …but the reply never arrives
				conn, _, _ := hj.Hijack()
				conn.Close()
			}
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	c.url = ts.URL
	return c
}

func (c *cloud) stats() (unique, dups int, ordered bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, n := range c.seen {
		unique++
		dups += n - 1
	}
	ordered = true
	for i := 1; i < len(c.ts); i++ {
		if c.ts[i] < c.ts[i-1] {
			ordered = false
		}
	}
	return
}

func edge(t *testing.T, c *cloud, dir string, mod func(*forward.Config)) (*forward.Forwarder, context.CancelFunc) {
	cfg, err := forward.ParseConnectionString(fmt.Sprintf("HostName=%s;TenantId=acme;DeviceId=edge-1;SharedAccessKey=%s", c.url, c.key))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Dir, cfg.Prefix, cfg.BatchLines = dir, "plant1.", 100
	cfg.Client = &http.Client{Timeout: 2 * time.Second}
	if mod != nil {
		mod(&cfg)
	}
	f, err := forward.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go f.Run(ctx)
	return f, func() { cancel(); <-f.Done() }
}

var t0 = time.Date(2026, 10, 2, 6, 0, 0, 0, time.UTC).UnixMilli()

func add(f *forward.Forwarder, from, n int) {
	for i := from; i < from+n; i++ {
		f.Add(store.Reading{Sensor: fmt.Sprintf("tank-%d", i%3), TS: t0 + int64(i)*1000, Values: map[string]any{"level": float64(i)}})
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestOutageLostRepliesAndRestart(t *testing.T) {
	c := newCloud(t)
	dir := t.TempDir()

	// The link is down: readings queue on disk.
	c.down.Store(true)
	f, stop := edge(t, c, dir, nil)
	add(f, 0, 1500)
	waitFor(t, "queue on disk", func() bool { return f.Status().QueuedBytes > 50_000 })
	if u, _, _ := c.stats(); u != 0 {
		t.Fatal("nothing should arrive while down")
	}
	// The edge restarts during the outage: the queue survives.
	stop()
	f, stop = edge(t, c, dir, nil)
	add(f, 1500, 500)

	// Back up, but the first two replies are lost after the cloud ingested
	// the batch: the edge retries the same batch ids and the cloud skips
	// what it already has.
	c.dropReply.Store(2)
	c.down.Store(false)
	waitFor(t, "all 2000 delivered", func() bool { u, _, _ := c.stats(); return u == 2000 })
	time.Sleep(300 * time.Millisecond)
	u, dups, ordered := c.stats()
	if u != 2000 || dups != 0 || !ordered {
		t.Fatalf("unique %d duplicates %d in order %v", u, dups, ordered)
	}
	st := f.Status()
	if st.Dropped != 0 || !st.Connected || st.QueuedBytes != 0 {
		t.Fatalf("status %+v", st)
	}
	// Sensor ids carry the site prefix.
	c.mu.Lock()
	_, ok := c.seen[fmt.Sprintf("plant1.tank-1@%d", t0+1000)]
	c.mu.Unlock()
	if !ok {
		t.Fatal("prefix not applied")
	}
	stop()
}

func TestQuotaRevokedKeyAndCap(t *testing.T) {
	c := newCloud(t)
	// The cloud's quota stops mid-batch: the edge continues from there.
	c.quota.Store(150)
	f, stop := edge(t, c, t.TempDir(), nil)
	add(f, 0, 400)
	waitFor(t, "quota reached", func() bool { u, _, _ := c.stats(); return u == 150 })
	c.quota.Store(-1)
	waitFor(t, "rest delivered after the quota frees", func() bool { u, _, _ := c.stats(); return u == 400 })
	if _, d, ordered := c.stats(); d != 0 || !ordered {
		t.Fatalf("duplicates %d ordered %v", d, ordered)
	}
	stop()

	// A revoked key: the cloud refuses, the edge keeps the data.
	c2 := newCloud(t)
	disabled := true
	c2.creds.Update("edge-1", auth.Update{Disabled: &disabled})
	f, stop = edge(t, c2, t.TempDir(), nil)
	add(f, 0, 50)
	waitFor(t, "refusal reported", func() bool { return f.Status().LastError != "" })
	if f.Status().Dropped != 0 || f.Status().QueuedBytes == 0 {
		t.Fatalf("data must be kept: %+v", f.Status())
	}
	stop()

	// Disk cap: a long outage with a tiny queue drops the oldest, counted.
	c3 := newCloud(t)
	c3.down.Store(true)
	f, stop = edge(t, c3, t.TempDir(), func(cfg *forward.Config) { cfg.MaxBytes = 40_000; cfg.SegmentSize = 8_000 })
	add(f, 0, 2000)
	waitFor(t, "oldest dropped", func() bool { return f.Status().Dropped > 0 })
	time.Sleep(1200 * time.Millisecond)
	st := f.Status()
	if st.QueuedBytes > 50_000 {
		t.Fatalf("queue over its cap: %+v", st)
	}
	c3.down.Store(false)
	deadline := time.Now().Add(40 * time.Second)
	for {
		u, _, _ := c3.stats()
		if int64(u)+f.Status().Dropped == 2000 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("delivered %d + dropped %d != 2000; status %+v", u, f.Status().Dropped, f.Status())
		}
		time.Sleep(100 * time.Millisecond)
	}
	u, d, ordered := c3.stats()
	if d != 0 || !ordered || u == 0 {
		t.Fatalf("after cap: unique %d dups %d ordered %v dropped %d", u, d, ordered, f.Status().Dropped)
	}
	stop()
	// Lines outside the edge's allowed sensors are rejected by the cloud
	// and skipped (never block the queue).
	c4 := newCloud(t)
	f, stop = edge(t, c4, t.TempDir(), func(cfg *forward.Config) { cfg.Prefix = "" })
	add(f, 0, 30)
	waitFor(t, "rejections counted", func() bool { return f.Status().Rejected == 30 })
	if u, _, _ := c4.stats(); u != 0 {
		t.Fatal("unprefixed sensors must be refused for this edge")
	}
	stop()
}
