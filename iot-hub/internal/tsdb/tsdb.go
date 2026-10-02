// Package tsdb persists readings and anomaly episodes to a SQL database
// (SQLite embedded, or PostgreSQL/TimescaleDB) and answers range queries.
//
// Writes never block ingestion: points go into a bounded queue and a single
// goroutine flushes them in multi-row batches. Each batch also updates
// 1-minute rollups (count, sum, sum of squares, min, max), so long-range
// charts and statistics read rollups instead of scanning raw rows.
package tsdb

import (
	"context"
	"math"
	"sync/atomic"
	"time"

	"github.com/williamtatendajose/prediction/iot-hub/internal/anomaly"
)

// Point is one stored sample. Text points (string state changes) have
// IsText set and no numeric value.
type Point struct {
	Sensor, Field string
	TS            int64
	Value         float64
	Text          string
	IsText        bool
}

type Bucket struct {
	T   int64   `json:"t"`
	N   int64   `json:"n"`
	Min float64 `json:"min"`
	Max float64 `json:"max"`
	Avg float64 `json:"avg"`
}

type Stats struct {
	N    int64   `json:"n"`
	Min  float64 `json:"min"`
	Max  float64 `json:"max"`
	Mean float64 `json:"mean"`
	Std  float64 `json:"std"`
}

// StatsFromSums turns mergeable aggregates into Stats (population std).
func StatsFromSums(n int64, sum, sumsq, mn, mx float64) Stats {
	if n == 0 {
		return Stats{}
	}
	mean := sum / float64(n)
	return Stats{N: n, Min: mn, Max: mx, Mean: mean, Std: math.Sqrt(math.Max(0, sumsq/float64(n)-mean*mean))}
}

type EventQuery struct {
	Sensor     string
	From, To   int64
	Limit      int
	ActiveOnly bool
}

type DB interface {
	WritePoints(ctx context.Context, pts []Point) error
	SaveEvent(ctx context.Context, e anomaly.Event) error
	// Series returns bucketed aggregates; bucket is in ms. Rollups are used
	// when bucket is a whole number of minutes.
	Series(ctx context.Context, sensor, field string, from, to, bucket int64) ([]Bucket, error)
	Stats(ctx context.Context, sensor, field string, from, to int64) (Stats, error)
	Events(ctx context.Context, q EventQuery) ([]anomaly.Event, error)
	Event(ctx context.Context, id string) (anomaly.Event, bool, error)
	// Latest calls fn for every series with its newest numeric points (up to
	// limit, oldest first) and its newest text value, for restoring live
	// state after a restart.
	Latest(ctx context.Context, limit int, fn func(sensor, field string, ts []int64, vals []float64, text string, textTS int64)) error
	// CloseOpenEvents ends episodes left open by a previous run.
	CloseOpenEvents(ctx context.Context, at int64) error
	Prune(ctx context.Context, rawBefore, rollupBefore, eventsBefore int64) error
	AlarmStore
	Close() error
}

// Writer batches points and events into a DB on one goroutine.
type Writer struct {
	DB       DB
	Batch    int           // max points per flush (default 500)
	Interval time.Duration // max delay before a flush (default 1s)
	// Retention; zero keeps forever.
	RawRetention, RollupRetention time.Duration
	Logf                          func(string, ...any)

	pts     chan Point
	evs     chan anomaly.Event
	done    chan struct{}
	Written atomic.Uint64
	Dropped atomic.Uint64
	Errors  atomic.Uint64
	lastErr atomic.Value // string
}

func NewWriter(db DB, queue int) *Writer {
	return &Writer{DB: db, pts: make(chan Point, queue), evs: make(chan anomaly.Event, 256), done: make(chan struct{})}
}

// Add queues a point; if the queue is full the point is dropped and counted
// rather than stalling MQTT/HTTP ingestion.
func (w *Writer) Add(p Point) {
	select {
	case w.pts <- p:
	default:
		w.Dropped.Add(1)
	}
}

func (w *Writer) AddEvent(e anomaly.Event) {
	select {
	case w.evs <- e:
	default:
		w.Dropped.Add(1)
	}
}

func (w *Writer) Queued() int { return len(w.pts) }

func (w *Writer) LastError() string {
	s, _ := w.lastErr.Load().(string)
	return s
}

func (w *Writer) fail(what string, err error) {
	w.Errors.Add(1)
	w.lastErr.Store(what + ": " + err.Error())
	if w.Logf != nil {
		w.Logf("tsdb %s: %v", what, err)
	}
}

// Run flushes until ctx is cancelled, then drains the queue and returns.
// Wait for Done() before closing the DB.
func (w *Writer) Run(ctx context.Context) {
	defer close(w.done)
	if w.Batch <= 0 {
		w.Batch = 500
	}
	if w.Interval <= 0 {
		w.Interval = time.Second
	}
	buf := make([]Point, 0, w.Batch)
	flush := func() {
		if len(buf) == 0 {
			return
		}
		// A fresh context so the final flush still runs during shutdown.
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := w.DB.WritePoints(c, buf); err != nil {
			w.Dropped.Add(uint64(len(buf)))
			w.fail("write", err)
		} else {
			w.Written.Add(uint64(len(buf)))
		}
		buf = buf[:0]
	}
	saveEvent := func(e anomaly.Event) {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := w.DB.SaveEvent(c, e); err != nil {
			w.fail("event", err)
		}
	}
	tick := time.NewTicker(w.Interval)
	defer tick.Stop()
	prune := time.NewTicker(time.Hour)
	defer prune.Stop()
	w.prune()
	for {
		select {
		case p := <-w.pts:
			buf = append(buf, p)
			if len(buf) >= w.Batch {
				flush()
			}
		case e := <-w.evs:
			flush() // keep points before the event that refers to them
			saveEvent(e)
		case <-tick.C:
			flush()
		case <-prune.C:
			w.prune()
		case <-ctx.Done():
			for {
				select {
				case p := <-w.pts:
					buf = append(buf, p)
					if len(buf) >= w.Batch {
						flush()
					}
				case e := <-w.evs:
					flush()
					saveEvent(e)
				default:
					flush()
					return
				}
			}
		}
	}
}

func (w *Writer) Done() <-chan struct{} { return w.done }

func (w *Writer) prune() {
	if w.RawRetention <= 0 && w.RollupRetention <= 0 {
		return
	}
	now := time.Now()
	before := func(d time.Duration) int64 {
		if d <= 0 {
			return 0
		}
		return now.Add(-d).UnixMilli()
	}
	evBefore := before(max(w.RawRetention, w.RollupRetention))
	if w.RawRetention <= 0 || w.RollupRetention <= 0 {
		evBefore = 0 // something is kept forever; keep its events too
	}
	c, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := w.DB.Prune(c, before(w.RawRetention), before(w.RollupRetention), evBefore); err != nil {
		w.fail("prune", err)
	}
}

// AutoBucket picks a readable bucket size giving at most ~target points.
func AutoBucket(from, to int64, target int) int64 {
	if target <= 0 {
		target = 600
	}
	steps := []int64{1e3, 2e3, 5e3, 10e3, 15e3, 30e3, 60e3, 120e3, 300e3, 600e3, 900e3, 1800e3,
		3600e3, 7200e3, 10800e3, 21600e3, 43200e3, 86400e3}
	want := (to - from) / int64(target)
	for _, s := range steps {
		if s >= want {
			return s
		}
	}
	return steps[len(steps)-1] * ((want + steps[len(steps)-1] - 1) / steps[len(steps)-1])
}

// AlarmStore persists acknowledgements, notes, shelves and the audit log.
type AlarmStore interface {
	SaveAck(ctx context.Context, eventID string, a anomaly.Ack) error
	Acks(ctx context.Context) (map[string]anomaly.Ack, error)
	SaveNote(ctx context.Context, n anomaly.Note) error
	Notes(ctx context.Context, eventID string) ([]anomaly.Note, error)
	NoteCounts(ctx context.Context) (map[string]int, error)
	SaveShelf(ctx context.Context, s anomaly.Shelf) error
	DeleteShelf(ctx context.Context, key string) error
	Shelves(ctx context.Context) ([]anomaly.Shelf, error)
	SaveAudit(ctx context.Context, a anomaly.AuditEntry) error
	Audit(ctx context.Context, from int64, limit int) ([]anomaly.AuditEntry, error)
}
