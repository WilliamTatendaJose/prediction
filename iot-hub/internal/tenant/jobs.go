package tenant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/williamtatendajose/prediction/iot-hub/internal/analytics"
	"github.com/williamtatendajose/prediction/iot-hub/internal/jobs"
	"github.com/williamtatendajose/prediction/iot-hub/internal/store"
)

// MaxJobsDefault applies when the quota sets none.
const MaxJobsDefault = 50

// jobsMgr stores a tenant's stream jobs and keeps the engine in step.
type jobsMgr struct {
	mu      sync.Mutex
	path    string
	specs   []jobs.Spec
	engine  *jobs.Engine
	maxJobs func() int
	store   *store.Store
	an      *analytics.Service
}

func (m *jobsMgr) load() error {
	if m.path == "" {
		return nil
	}
	b, err := os.ReadFile(m.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &m.specs); err != nil {
		return fmt.Errorf("parse %s: %w", m.path, err)
	}
	m.engine.Set(m.specs)
	return nil
}

func (m *jobsMgr) save(specs []jobs.Spec) error {
	if m.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(specs, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.path), 0o700); err != nil {
		return err
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.path)
}

// List is GET /api/jobs: specs with live status.
func (m *jobsMgr) List() any { return m.engine.Status() }

// Put creates or replaces a job (PUT /api/jobs/{id}).
func (m *jobsMgr) Put(id string, raw []byte) error {
	var sp jobs.Spec
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&sp); err != nil {
		return fmt.Errorf("%w: %v", store.ErrInvalid, err)
	}
	sp.ID = id
	if _, err := jobs.Compile(sp); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	next := make([]jobs.Spec, 0, len(m.specs)+1)
	found := false
	for _, s := range m.specs {
		if s.ID == id {
			s, found = sp, true
		}
		next = append(next, s)
	}
	if !found {
		if n := m.maxJobs(); n > 0 && len(m.specs) >= n {
			return fmt.Errorf("%w: max %d jobs for this tenant", store.ErrLimit, n)
		}
		next = append(next, sp)
	}
	// One step only: an output sensor may not be any enabled job's input,
	// so jobs can't chain into loops.
	if err := checkChains(next); err != nil {
		return err
	}
	sort.Slice(next, func(i, j int) bool { return next[i].ID < next[j].ID })
	if err := m.save(next); err != nil {
		return err
	}
	m.specs = next
	m.engine.Set(next)
	return nil
}

func checkChains(specs []jobs.Spec) error {
	type io struct {
		id, in, out string
	}
	var all []io
	for _, s := range specs {
		if !s.Enabled {
			continue
		}
		p, err := jobs.Compile(s)
		if err != nil {
			continue // listed with its error; never runs
		}
		out := ""
		if p.Out.Kind == "sensor" {
			out = p.Out.Sensor
		}
		all = append(all, io{s.ID, p.Sensors, out})
	}
	for _, a := range all {
		if a.out == "" {
			continue
		}
		sample := strings.ReplaceAll(a.out, "{sensor}", "x")
		for _, b := range all {
			if ok, _ := path.Match(b.in, sample); ok {
				return fmt.Errorf("%w: job %s writes %s, which job %s reads: jobs can't feed each other", store.ErrInvalid, a.id, a.out, b.id)
			}
		}
	}
	return nil
}

func (m *jobsMgr) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var next []jobs.Spec
	for _, s := range m.specs {
		if s.ID != id {
			next = append(next, s)
		}
	}
	if len(next) == len(m.specs) {
		return store.ErrNotFound
	}
	if err := m.save(next); err != nil {
		return err
	}
	m.specs = next
	m.engine.Set(next)
	return nil
}

// TestResult is a dry run of a job over stored history.
type TestResult struct {
	Readings  int         `json:"readings"`
	Sensors   []string    `json:"sensors"`
	Rows      []jobs.Row  `json:"rows"`
	Alerts    []string    `json:"alerts,omitempty"`
	Truncated bool        `json:"truncated,omitempty"`
	Status    jobs.Status `json:"status"`
}

const maxTestRows = 500

// Test replays history through a job without writing anything
// (POST /api/jobs/test), like Stream Analytics' "test query".
func (m *jobsMgr) Test(ctx context.Context, raw []byte, from, to int64) (any, error) {
	var sp jobs.Spec
	if err := json.Unmarshal(raw, &sp); err != nil {
		return nil, fmt.Errorf("%w: %v", store.ErrInvalid, err)
	}
	if sp.ID == "" {
		sp.ID = "test"
	}
	sp.Enabled = true
	plan, err := jobs.Compile(sp)
	if err != nil {
		return nil, err
	}
	res := &TestResult{Rows: []jobs.Row{}, Sensors: []string{}}
	var mu sync.Mutex
	add := func(r jobs.Row) {
		mu.Lock()
		defer mu.Unlock()
		if len(res.Rows) >= maxTestRows {
			res.Truncated = true
			return
		}
		res.Rows = append(res.Rows, r)
	}
	eng := jobs.New(jobs.Outputs{
		Sensor: func(sensor string, ts int64, v map[string]any) error {
			for f, x := range v {
				add(jobs.Row{Job: sp.ID, Group: sensor, End: ts, Field: f, Value: x.(float64)})
			}
			return nil
		},
		Raise: func(key, sensor, field string, ts int64, v float64, msg string) {
			mu.Lock()
			res.Alerts = append(res.Alerts, time.UnixMilli(ts).UTC().Format(time.RFC3339)+" raised: "+msg)
			mu.Unlock()
		},
		Clear: func(key string, ts int64) {
			mu.Lock()
			res.Alerts = append(res.Alerts, time.UnixMilli(ts).UTC().Format(time.RFC3339)+" cleared")
			mu.Unlock()
		},
		Webhook: func(_ string, r jobs.Row) { add(r) },
	})
	eng.Set([]jobs.Spec{sp})
	type pt struct {
		sensor string
		ts     int64
		v      float64
	}
	var pts []pt
	for _, sv := range m.store.List() {
		if ok, _ := path.Match(plan.Sensors, sv.ID); !ok {
			continue
		}
		ts, vs, err := m.an.Raw(ctx, sv.ID, plan.Field, from, to, 200000)
		if err != nil {
			return nil, err
		}
		res.Sensors = append(res.Sensors, sv.ID)
		for i := range ts {
			if ts[i] >= from {
				pts = append(pts, pt{sv.ID, ts[i], vs[i]})
			}
		}
	}
	sort.SliceStable(pts, func(i, j int) bool { return pts[i].ts < pts[j].ts })
	for _, p := range pts {
		eng.Observe(store.Reading{Sensor: p.sensor, TS: p.ts, Values: map[string]any{plan.Field: p.v}})
	}
	eng.Flush()
	res.Readings = len(pts)
	if st := eng.Status(); len(st) == 1 {
		res.Status = st[0]
	}
	sort.Strings(res.Sensors)
	return res, nil
}
