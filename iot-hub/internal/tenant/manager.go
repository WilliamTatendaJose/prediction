package tenant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/williamtatendajose/prediction/iot-hub/internal/auth"
	"github.com/williamtatendajose/prediction/iot-hub/internal/tsdb"
)

var (
	ErrNotFound  = errors.New("tenant not found")
	ErrExists    = errors.New("tenant already exists")
	ErrSuspended = errors.New("tenant is suspended")
)

// Platform holds every tenant. In multi-tenant mode each tenant's files
// live under Dir/{id}/ and its database is Dir/{id}/{dbfile} (SQLite) or
// schema t_{id} (PostgreSQL).
type Platform struct {
	Dir     string          // tenants root, e.g. data/tenants
	DBURL   string          // "" none; sqlite path (per-tenant file name) or postgres://
	Base    Options         // platform defaults; per tenant: paths, creds, quota, limiter
	Default Quota           // quota for fields a tenant leaves at 0
	Creds   *auth.Store     // root credential store
	OnStop  func(id string) // called when a tenant stops (drop its sessions)

	mu      sync.RWMutex
	infos   map[string]Info
	running map[string]*Runtime
	ctx     context.Context
}

func (p *Platform) registryPath() string { return filepath.Join(p.Dir, "tenants.json") }

// Load reads the registry and starts every active tenant.
func (p *Platform) Load(ctx context.Context) error {
	p.ctx = ctx
	p.infos, p.running = map[string]Info{}, map[string]*Runtime{}
	b, err := os.ReadFile(p.registryPath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(b) > 0 {
		var list []Info
		if err := json.Unmarshal(b, &list); err != nil {
			return fmt.Errorf("parse %s: %w", p.registryPath(), err)
		}
		for _, in := range list {
			p.infos[in.ID] = in
		}
	}
	for _, in := range p.infos {
		if in.Status == Active {
			if err := p.start(in); err != nil {
				return fmt.Errorf("tenant %s: %w", in.ID, err)
			}
		}
	}
	return nil
}

func (p *Platform) save() error {
	list := make([]Info, 0, len(p.infos))
	for _, in := range p.infos {
		list = append(list, in)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		return err
	}
	tmp := p.registryPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p.registryPath())
}

func (p *Platform) dir(id string) string { return filepath.Join(p.Dir, id) }

func schema(id string) string { return "t_" + strings.ReplaceAll(id, "-", "_") }

func (p *Platform) dbURL(id string) (url, sch string) {
	switch {
	case p.DBURL == "":
		return "", ""
	case strings.Contains(p.DBURL, "://"):
		return p.DBURL, schema(id)
	}
	name := filepath.Base(strings.TrimPrefix(p.DBURL, "sqlite:"))
	if name == "." || name == "/" || name == "" {
		name = "readings.db"
	}
	return filepath.Join(p.dir(id), name), ""
}

// start opens a tenant's runtime; callers hold p.mu (or are loading).
func (p *Platform) start(in Info) error {
	q, raw, rollup := p.effective(in)
	o := p.Base
	o.ConfigPath = filepath.Join(p.dir(in.ID), "config.json")
	o.SettingsPath = filepath.Join(p.dir(in.ID), "settings.json")
	o.JobsPath = filepath.Join(p.dir(in.ID), "jobs.json")
	o.TwinsPath = filepath.Join(p.dir(in.ID), "twins.json")
	tid := in.ID
	o.MaxJobs = func() int {
		if q, _, _ := p.effective(p.info(tid)); q.MaxJobs > 0 {
			return q.MaxJobs
		}
		return MaxJobsDefault
	}
	o.Seed = Settings{Shifts: p.Base.Seed.Shifts, TimeZone: p.Base.Seed.TimeZone} // never the operator's targets
	o.DBURL, o.PGSchema = p.dbURL(in.ID)
	o.MaxSensors = q.MaxSensors
	o.RawRetention, o.RollupRetention = raw, rollup
	if o.BackupDir != "" {
		o.BackupDir = filepath.Join(o.BackupDir, in.ID)
	}
	o.Creds = p.Creds.Tenant(in.ID)
	o.PublicRead = false // every tenant request is authenticated
	id := in.ID
	o.SelfService = func() bool { return p.info(id).DeviceSelfService }
	o.MaxDevices = func() int { q, _, _ := p.effective(p.info(id)); return q.MaxDevices }
	o.Limiter = NewLimiter(q.MessagesPerSecond, q.MessagesPerDay)
	if err := os.MkdirAll(p.dir(in.ID), 0o700); err != nil {
		return err
	}
	rt, err := Open(p.ctx, in.ID, o)
	if err != nil {
		return err
	}
	p.running[in.ID] = rt
	return nil
}

// effective resolves a tenant's quota: its own values, else the platform
// default, else the flags (Base); -1 = unlimited. Rates and counts of 0
// mean unlimited to the enforcing code.
func (p *Platform) effective(in Info) (q Quota, raw, rollup time.Duration) {
	q = in.Quota.Or(p.Default)
	switch {
	case q.MaxSensors < 0:
		q.MaxSensors = 1 << 30
	case q.MaxSensors == 0:
		q.MaxSensors = p.Base.MaxSensors
	}
	q.MaxDevices = max(q.MaxDevices, 0)
	q.MessagesPerSecond = max(q.MessagesPerSecond, 0)
	q.MessagesPerDay = max(q.MessagesPerDay, 0)
	q.MaxJobs = max(q.MaxJobs, 0)
	raw, rollup = p.Base.RawRetention, p.Base.RollupRetention
	if q.RawRetentionDays != 0 {
		raw = days(q.RawRetentionDays) // -1: forever
	}
	if q.RollupRetentionDays != 0 {
		rollup = days(q.RollupRetentionDays)
	}
	return q, raw, rollup
}

func (p *Platform) info(id string) Info {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.infos[id]
}

// Runtime returns an active tenant's runtime.
func (p *Platform) Runtime(id string) (*Runtime, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	in, ok := p.infos[id]
	if !ok {
		return nil, ErrNotFound
	}
	if in.Status != Active {
		return nil, ErrSuspended
	}
	return p.running[id], nil
}

// Info returns a tenant's registry entry.
func (p *Platform) Info(id string) (Info, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	in, ok := p.infos[id]
	return in, ok
}

// Usage is what a tenant consumes, for the superadmin.
type Usage struct {
	Info
	Effective     Quota `json:"effectiveQuota"`
	Sensors       int   `json:"sensors"`
	Devices       int   `json:"devices"`
	MessagesToday int64 `json:"messagesToday"`
	Rejected      int64 `json:"messagesRejected"`
}

func (p *Platform) usage(in Info) Usage {
	q, _, _ := p.effective(in)
	u := Usage{Info: in, Effective: q, Devices: p.Creds.Tenant(in.ID).Count()}
	if rt := p.running[in.ID]; rt != nil {
		u.Sensors = rt.Store.Count()
		if rt.Limiter != nil {
			u.MessagesToday, u.Rejected = rt.Limiter.Today(), rt.Limiter.Rejected.Load()
		}
	}
	return u
}

func (p *Platform) List() []Usage {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]Usage, 0, len(p.infos))
	for _, in := range p.infos {
		out = append(out, p.usage(in))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (p *Platform) Get(id string) (Usage, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	in, ok := p.infos[id]
	if !ok {
		return Usage{}, ErrNotFound
	}
	return p.usage(in), nil
}

// Create registers and starts a tenant.
func (p *Platform) Create(in Info) (Info, error) {
	if err := ValidID(in.ID); err != nil {
		return in, err
	}
	if err := in.Quota.validate(); err != nil {
		return in, err
	}
	in.Status, in.Created = Active, time.Now().UnixMilli()
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.infos[in.ID]; ok {
		return in, ErrExists
	}
	// A directory left from a deleted tenant must not leak into a new one.
	if _, err := os.Stat(p.dir(in.ID)); err == nil {
		return in, fmt.Errorf("%w: data directory %s exists; remove it first", ErrExists, p.dir(in.ID))
	}
	if url, sch := p.dbURL(in.ID); sch != "" {
		ctx, cancel := context.WithTimeout(p.ctx, 10*time.Second)
		exists, err := tsdb.SchemaExists(ctx, url, sch)
		cancel()
		if err != nil {
			return in, err
		}
		if exists {
			return in, fmt.Errorf("%w: database schema %s exists (left from an earlier tenant?); drop it first", ErrExists, sch)
		}
	}
	p.infos[in.ID] = in
	if err := p.start(in); err != nil {
		delete(p.infos, in.ID)
		return in, err
	}
	if err := p.save(); err != nil {
		return in, err
	}
	return in, nil
}

func (q Quota) validate() error {
	if q.MessagesPerSecond < 0 && q.MessagesPerSecond != -1 || q.RawRetentionDays < -1 || q.RollupRetentionDays < -1 || q.MaxSensors < -1 || q.MaxDevices < -1 || q.MessagesPerDay < -1 || q.MaxJobs < -1 {
		return errors.New("quota values must be positive, 0 (platform default) or -1 (unlimited)")
	}
	return nil
}

// Patch is a partial update of a tenant.
type Patch struct {
	Name              *string `json:"name,omitempty"`
	Status            *string `json:"status,omitempty"`
	Quota             *Quota  `json:"quota,omitempty"`
	DeviceSelfService *bool   `json:"deviceSelfService,omitempty"`
	Note              *string `json:"note,omitempty"`
}

// Update applies a patch: quotas take effect at once; suspending stops the
// runtime (data kept) and drops sessions; resuming starts it again.
func (p *Platform) Update(id string, pt Patch) (Info, error) {
	p.mu.Lock()
	in, ok := p.infos[id]
	if !ok {
		p.mu.Unlock()
		return in, ErrNotFound
	}
	if pt.Status != nil && *pt.Status != Active && *pt.Status != Suspended {
		p.mu.Unlock()
		return in, errors.New("status must be active or suspended")
	}
	if pt.Quota != nil {
		if err := pt.Quota.validate(); err != nil {
			p.mu.Unlock()
			return in, err
		}
		in.Quota = *pt.Quota
	}
	if pt.Name != nil {
		in.Name = *pt.Name
	}
	if pt.Note != nil {
		in.Note = *pt.Note
	}
	if pt.DeviceSelfService != nil {
		in.DeviceSelfService = *pt.DeviceSelfService
	}
	var stop *Runtime
	if pt.Status != nil && *pt.Status != in.Status {
		in.Status = *pt.Status
		if in.Status == Suspended {
			stop = p.running[id]
			delete(p.running, id)
		} else if err := p.start(in); err != nil {
			p.mu.Unlock()
			return in, err
		}
	}
	p.infos[id] = in
	if rt := p.running[id]; rt != nil {
		q, raw, rollup := p.effective(in)
		rt.Store.SetMaxSensors(q.MaxSensors)
		if rt.Limiter != nil {
			rt.Limiter.Set(q.MessagesPerSecond, q.MessagesPerDay)
		}
		rt.SetRetention(raw, rollup)
	}
	err := p.save()
	p.mu.Unlock()
	if stop != nil {
		if p.OnStop != nil {
			p.OnStop(id)
		}
		stop.Close()
	}
	return in, err
}

// Delete stops a tenant and removes its registry entry, credentials and
// data (files, or its PostgreSQL schema). Irreversible.
func (p *Platform) Delete(ctx context.Context, id string) error {
	p.mu.Lock()
	if _, ok := p.infos[id]; !ok {
		p.mu.Unlock()
		return ErrNotFound
	}
	rt := p.running[id]
	delete(p.running, id)
	delete(p.infos, id)
	err := p.save()
	p.mu.Unlock()
	if p.OnStop != nil {
		p.OnStop(id)
	}
	if rt != nil {
		rt.Close()
	}
	if _, e := p.Creds.RemoveTenant(id); e != nil && err == nil {
		err = e
	}
	if url, sch := p.dbURL(id); sch != "" {
		if e := tsdb.DropSchema(ctx, url, sch); e != nil && err == nil {
			err = e
		}
	}
	if e := os.RemoveAll(p.dir(id)); e != nil && err == nil {
		err = e
	}
	if p.Base.BackupDir != "" {
		_ = os.RemoveAll(filepath.Join(p.Base.BackupDir, id))
	}
	return err
}

// Close stops every tenant.
func (p *Platform) Close() {
	p.mu.Lock()
	rts := p.running
	p.running = map[string]*Runtime{}
	p.mu.Unlock()
	var wg sync.WaitGroup
	for _, rt := range rts {
		wg.Add(1)
		go func() { defer wg.Done(); rt.Close() }()
	}
	wg.Wait()
}
