// Package forward is store-and-forward for an edge hub: every reading is
// appended to a queue on disk and uploaded in batches to a cloud hub
// (POST /api/ingest/batch). When the link is down the queue grows; when
// it comes back the backlog goes up in order, oldest first, at the cloud's
// pace (as Azure IoT Edge does). Nothing is acknowledged before the cloud
// confirms it; a retried batch is recognised by its id and not ingested
// twice; the queue is capped on disk, and when it must drop it drops the
// oldest data and counts it.
package forward

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/williamtatendajose/prediction/iot-hub/internal/auth"
	"github.com/williamtatendajose/prediction/iot-hub/internal/store"
)

type Config struct {
	URL   string // cloud hub base URL, e.g. https://cloud.example.com
	Token string // bearer token, or the SAS fields below
	// From a connection string: the edge signs its own SAS tokens.
	Tenant, DeviceID, Key string

	Dir         string // queue directory
	MaxBytes    int64  // disk cap (default 1 GiB)
	BatchLines  int    // lines per upload (default 500)
	SegmentSize int64  // bytes per segment file (default 4 MiB)
	Sensors     string // glob of sensors to forward (default "*")
	Prefix      string // prepended to sensor ids in the cloud (site name)
	Client      *http.Client
	Logf        func(string, ...any)
}

// ParseConnectionString reads "HostName=…;TenantId=…;DeviceId=…;SharedAccessKey=…".
// HostName may carry a scheme (http:// for tests); https otherwise.
func ParseConnectionString(cs string) (Config, error) {
	var c Config
	for _, part := range strings.Split(cs, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch strings.ToLower(k) {
		case "hostname":
			c.URL = v
		case "tenantid":
			c.Tenant = v
		case "deviceid":
			c.DeviceID = v
		case "sharedaccesskey":
			c.Key = v
		}
	}
	if c.URL == "" || c.Tenant == "" || c.DeviceID == "" || c.Key == "" {
		return c, errors.New("connection string needs HostName, TenantId, DeviceId and SharedAccessKey")
	}
	if !strings.Contains(c.URL, "://") {
		c.URL = "https://" + c.URL
	}
	return c, nil
}

// Status is reported by GET /api/forward.
type Status struct {
	URL         string `json:"url"`
	QueuedBytes int64  `json:"queuedBytes"`
	Sent        int64  `json:"sent"`     // readings accepted by the cloud
	Rejected    int64  `json:"rejected"` // refused by the cloud (bad or not allowed): skipped
	Dropped     int64  `json:"dropped"`  // lost on this side: queue over its cap
	LastSent    int64  `json:"lastSentAt,omitempty"`
	LastError   string `json:"lastError,omitempty"`
	Connected   bool   `json:"connected"`
}

type cursor struct {
	Seg int64 `json:"seg"`
	Off int64 `json:"off"`
}

type Forwarder struct {
	cfg    Config
	edgeID string
	in     chan []byte
	wake   chan struct{}

	mu     sync.Mutex // guards the files and the cursor
	cur    cursor
	wseq   int64
	wfile  *os.File
	wbuf   *bufio.Writer
	wsize  int64
	tokMu  sync.Mutex
	tok    string
	tokExp time.Time

	sent, rejected, dropped atomic.Int64
	lastSent                atomic.Int64
	lastErr                 atomic.Value
	connected               atomic.Bool
	done                    chan struct{}
}

func New(cfg Config) (*Forwarder, error) {
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = 1 << 30
	}
	if cfg.BatchLines <= 0 {
		cfg.BatchLines = 500
	}
	if cfg.SegmentSize <= 0 {
		cfg.SegmentSize = 4 << 20
	}
	if cfg.Sensors == "" {
		cfg.Sensors = "*"
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 30 * time.Second}
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	cfg.URL = strings.TrimRight(cfg.URL, "/")
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, err
	}
	f := &Forwarder{cfg: cfg, in: make(chan []byte, 65536), wake: make(chan struct{}, 1), done: make(chan struct{})}
	// A random id per edge keeps batch ids unique even when several sites
	// send to the same tenant.
	idPath := filepath.Join(cfg.Dir, "edge-id")
	if b, err := os.ReadFile(idPath); err == nil && len(b) > 0 {
		f.edgeID = strings.TrimSpace(string(b))
	} else {
		r := make([]byte, 8)
		if _, err := rand.Read(r); err != nil {
			return nil, err
		}
		f.edgeID = hex.EncodeToString(r)
		if err := os.WriteFile(idPath, []byte(f.edgeID), 0o600); err != nil {
			return nil, err
		}
	}
	if b, err := os.ReadFile(filepath.Join(cfg.Dir, "cursor.json")); err == nil {
		_ = json.Unmarshal(b, &f.cur)
	}
	segs, err := f.segments()
	if err != nil {
		return nil, err
	}
	// Write to a new segment after the last one and after the cursor:
	// never append to a file that may end mid-line, and never behind the
	// cursor (where the uploader would not look).
	f.wseq = max(f.cur.Seg+1, 1)
	if len(segs) > 0 {
		f.wseq = max(f.wseq, segs[len(segs)-1]+1)
	}
	if f.cur.Seg == 0 {
		f.cur.Seg = f.wseq
		if len(segs) > 0 {
			f.cur.Seg = segs[0]
		}
	}
	return f, nil
}

func segName(seq int64) string { return fmt.Sprintf("seg-%020d.ndjson", seq) }

func (f *Forwarder) segments() ([]int64, error) {
	ents, err := os.ReadDir(f.cfg.Dir)
	if err != nil {
		return nil, err
	}
	var out []int64
	for _, e := range ents {
		n := e.Name()
		if strings.HasPrefix(n, "seg-") && strings.HasSuffix(n, ".ndjson") {
			if seq, err := strconv.ParseInt(n[4:len(n)-7], 10, 64); err == nil {
				out = append(out, seq)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// MaxLine is the largest reading forwarded (the cloud accepts 64 KiB lines).
const MaxLine = 60 << 10

type line struct {
	S string         `json:"s"`
	T int64          `json:"t"`
	V map[string]any `json:"v"`
}

// Add queues a reading (called on the ingest path: never blocks).
func (f *Forwarder) Add(r store.Reading) {
	if ok, _ := path.Match(f.cfg.Sensors, r.Sensor); !ok {
		return
	}
	b, err := json.Marshal(line{S: f.cfg.Prefix + r.Sensor, T: r.TS, V: r.Values})
	if err != nil || len(b) > MaxLine {
		f.rejected.Add(1) // one oversized reading must not block the queue
		return
	}
	select {
	case f.in <- append(b, '\n'):
	default:
		f.dropped.Add(1) // the disk writer is stalled
	}
}

// Run writes and uploads until ctx ends; queued lines are flushed to disk
// before it returns.
func (f *Forwarder) Run(ctx context.Context) {
	defer close(f.done)
	up := make(chan struct{})
	go func() { defer close(up); f.upload(ctx) }()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			for n := len(f.in); n > 0; n-- {
				f.write(<-f.in)
			}
			f.flush()
			f.mu.Lock()
			if f.wfile != nil {
				f.wfile.Close()
			}
			f.mu.Unlock()
			<-up
			return
		case b := <-f.in:
			f.write(b)
		case <-t.C:
			f.flush()
		}
	}
}

// Done is closed when Run has returned.
func (f *Forwarder) Done() <-chan struct{} { return f.done }

func (f *Forwarder) write(b []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.wfile == nil || f.wsize >= f.cfg.SegmentSize {
		f.rotateLocked()
		if f.wfile == nil {
			f.dropped.Add(1)
			return
		}
	}
	n, _ := f.wbuf.Write(b)
	f.wsize += int64(n)
	if f.wbuf.Buffered() >= 256<<10 {
		f.flushLocked()
	}
}

func (f *Forwarder) rotateLocked() {
	if f.wfile != nil {
		f.flushLocked()
		f.wfile.Close()
		f.wseq++
	}
	file, err := os.OpenFile(filepath.Join(f.cfg.Dir, segName(f.wseq)), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		f.lastErr.Store("queue: " + err.Error())
		f.wfile = nil
		return
	}
	f.wfile, f.wbuf, f.wsize = file, bufio.NewWriterSize(file, 64<<10), 0
	f.capLocked()
}

func (f *Forwarder) flush() {
	f.mu.Lock()
	f.flushLocked()
	f.mu.Unlock()
}

// flushLocked makes queued lines durable (fsync) and visible to the
// uploader: at most about a second of readings is at risk in a power cut.
func (f *Forwarder) flushLocked() {
	if f.wfile == nil || f.wbuf.Buffered() == 0 {
		return
	}
	if err := f.wbuf.Flush(); err != nil {
		f.lastErr.Store("queue: " + err.Error())
	}
	_ = f.wfile.Sync()
	select {
	case f.wake <- struct{}{}:
	default:
	}
	f.capLocked()
}

// capLocked deletes the oldest segments while the queue is over its cap.
func (f *Forwarder) capLocked() {
	segs, err := f.segments()
	if err != nil {
		return
	}
	var total int64
	sizes := map[int64]int64{}
	for _, s := range segs {
		if fi, err := os.Stat(filepath.Join(f.cfg.Dir, segName(s))); err == nil {
			sizes[s] = fi.Size()
			total += fi.Size()
		}
	}
	for _, s := range segs {
		if total <= f.cfg.MaxBytes || s >= f.wseq {
			break
		}
		p := filepath.Join(f.cfg.Dir, segName(s))
		lost := countLines(p, map[bool]int64{true: f.cur.Off}[s == f.cur.Seg])
		if s < f.cur.Seg {
			lost = 0 // already sent
		}
		if os.Remove(p) == nil {
			total -= sizes[s]
			f.dropped.Add(lost)
			if s >= f.cur.Seg {
				f.cur = cursor{Seg: s + 1}
				f.saveCursorLocked()
			}
			if lost > 0 {
				f.cfg.Logf("forward queue over %d MB: dropped %d unsent readings (oldest first)", f.cfg.MaxBytes>>20, lost)
			}
		}
	}
}

func countLines(p string, from int64) int64 {
	b, err := os.ReadFile(p)
	if err != nil || from > int64(len(b)) {
		return 0
	}
	return int64(bytes.Count(b[from:], []byte{'\n'}))
}

func (f *Forwarder) saveCursorLocked() {
	b, _ := json.Marshal(f.cur)
	tmp := filepath.Join(f.cfg.Dir, "cursor.json.tmp")
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, filepath.Join(f.cfg.Dir, "cursor.json"))
	}
}

// next reads the next batch: whole lines from the cursor, up to the batch
// size or 1 MiB. Finished segments are deleted.
func (f *Forwarder) next() (data []byte, lines int, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for {
		p := filepath.Join(f.cfg.Dir, segName(f.cur.Seg))
		file, err := os.Open(p)
		if err != nil {
			if f.cur.Seg < f.wseq { // a gap (deleted by the cap): move on
				f.cur = cursor{Seg: f.cur.Seg + 1}
				continue
			}
			return nil, 0, ""
		}
		_, _ = file.Seek(f.cur.Off, io.SeekStart)
		r := bufio.NewReaderSize(file, 64<<10)
		var buf bytes.Buffer
		for lines < f.cfg.BatchLines && buf.Len() < 1<<20 {
			l, err := r.ReadBytes('\n')
			if err != nil { // EOF or a partly written last line
				break
			}
			buf.Write(l)
			lines++
		}
		file.Close()
		if lines > 0 {
			return buf.Bytes(), lines, fmt.Sprintf("%s:%d:%d", f.edgeID, f.cur.Seg, f.cur.Off)
		}
		if f.cur.Seg < f.wseq { // this segment is complete and fully sent
			_ = os.Remove(p)
			f.cur = cursor{Seg: f.cur.Seg + 1}
			f.saveCursorLocked()
			continue
		}
		return nil, 0, ""
	}
}

// advance moves the cursor past n lines of the batch just sent.
func (f *Forwarder) advance(data []byte, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	off := 0
	for i := 0; i < n; i++ {
		j := bytes.IndexByte(data[off:], '\n')
		if j < 0 {
			break
		}
		off += j + 1
	}
	f.cur.Off += int64(off)
	f.saveCursorLocked()
}

func (f *Forwarder) authHeader() (string, error) {
	if f.cfg.Key == "" {
		return "Bearer " + f.cfg.Token, nil
	}
	f.tokMu.Lock()
	defer f.tokMu.Unlock()
	if time.Until(f.tokExp) < time.Hour {
		exp := time.Now().Add(24 * time.Hour)
		tok, err := auth.SignSAS(auth.ResourceURI(f.cfg.Tenant, f.cfg.DeviceID), f.cfg.Key, exp)
		if err != nil {
			return "", err
		}
		f.tok, f.tokExp = tok, exp
	}
	return f.tok, nil
}

type reply struct {
	Processed int      `json:"processed"`
	Accepted  int      `json:"accepted"`
	Rejected  int      `json:"rejected"`
	Duplicate int      `json:"duplicate"`
	Errors    []string `json:"errors"`
	Error     string   `json:"error"`
}

func (f *Forwarder) send(ctx context.Context, data []byte, id string) (reply, int, error) {
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(data)
	zw.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.cfg.URL+"/api/ingest/batch", &gz)
	if err != nil {
		return reply{}, 0, err
	}
	a, err := f.authHeader()
	if err != nil {
		return reply{}, 0, err
	}
	req.Header.Set("Authorization", a)
	req.Header.Set("Content-Type", "application/x-ndjson")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("X-Batch-Id", id)
	res, err := f.cfg.Client.Do(req)
	if err != nil {
		return reply{}, 0, err
	}
	defer res.Body.Close()
	var rp reply
	_ = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&rp)
	return rp, res.StatusCode, nil
}

// MaxBackoff caps the wait between retries after failures.
const MaxBackoff = time.Minute

func (f *Forwarder) upload(ctx context.Context) {
	backoff := time.Second
	// idle waits for new data (or a second); new data wakes it early.
	idle := func() bool {
		t := time.NewTimer(time.Second)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
		case <-f.wake:
		}
		return true
	}
	// pause waits out a failure. New data must not cut it short, or a busy
	// edge would hammer a cloud that is down.
	pause := func(d time.Duration) bool {
		t := time.NewTimer(jitter(d))
		defer t.Stop()
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
			return true
		}
	}
	for ctx.Err() == nil {
		data, n, id := f.next()
		if n == 0 {
			if !idle() {
				return
			}
			continue
		}
		rp, code, err := f.send(ctx, data, id)
		switch {
		case err != nil:
			f.fail(err.Error())
		case (code == http.StatusOK || code == http.StatusTooManyRequests) && (rp.Processed > 0 || code == http.StatusTooManyRequests):
			done := min(rp.Processed, n)
			f.sent.Add(int64(rp.Accepted))
			f.rejected.Add(int64(rp.Rejected))
			if rp.Rejected > 0 {
				f.cfg.Logf("forward: cloud rejected %d readings: %s", rp.Rejected, strings.Join(rp.Errors, "; "))
			}
			f.advance(data, done)
			f.lastSent.Store(time.Now().UnixMilli())
			f.connected.Store(true)
			f.lastErr.Store("")
			backoff = time.Second
			if code == http.StatusTooManyRequests {
				f.lastErr.Store("cloud quota reached: slowing down")
				if !pause(5 * time.Second) {
					return
				}
			}
			continue
		case code == http.StatusUnauthorized || code == http.StatusForbidden || code == http.StatusNotFound:
			// Configuration (revoked key, wrong tenant, suspended): keep the
			// data and try again later.
			f.fail(fmt.Sprintf("cloud refused (HTTP %d): %s", code, rp.Error))
			backoff = MaxBackoff
		default:
			f.fail(fmt.Sprintf("cloud HTTP %d: %s", code, rp.Error))
		}
		if !pause(backoff) {
			return
		}
		backoff = min(backoff*2, MaxBackoff)
	}
}

// jitter spreads retries ±20% so many edges don't retry in step after a
// cloud outage.
func jitter(d time.Duration) time.Duration {
	var b [2]byte
	_, _ = rand.Read(b[:])
	frac := float64(uint16(b[0])<<8|uint16(b[1]))/65535*0.4 - 0.2
	return d + time.Duration(float64(d)*frac)
}

func (f *Forwarder) fail(msg string) {
	if prev, _ := f.lastErr.Load().(string); prev != msg {
		f.cfg.Logf("forward: %s", msg)
	}
	f.lastErr.Store(msg)
	f.connected.Store(false)
}

// Status reports the queue and upload state.
func (f *Forwarder) Status() Status {
	f.mu.Lock()
	var queued int64
	if segs, err := f.segments(); err == nil {
		for _, s := range segs {
			if fi, err := os.Stat(filepath.Join(f.cfg.Dir, segName(s))); err == nil && s >= f.cur.Seg {
				queued += fi.Size()
				if s == f.cur.Seg {
					queued -= f.cur.Off
				}
			}
		}
	}
	if f.wbuf != nil {
		queued += int64(f.wbuf.Buffered())
	}
	f.mu.Unlock()
	last, _ := f.lastErr.Load().(string)
	return Status{URL: f.cfg.URL, QueuedBytes: queued, Sent: f.sent.Load(), Rejected: f.rejected.Load(),
		Dropped: f.dropped.Load(), LastSent: f.lastSent.Load(), LastError: last, Connected: f.connected.Load()}
}
