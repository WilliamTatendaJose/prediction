// Package backup exports and imports the hub's configuration and takes
// scheduled online snapshots of a SQLite database, keeping the newest N.
package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/williamtatendajose/prediction/iot-hub/internal/auth"
	"github.com/williamtatendajose/prediction/iot-hub/internal/store"
	"github.com/williamtatendajose/prediction/iot-hub/internal/tsdb"
)

const Version = 1

// Config is a configuration backup: everything needed to rebuild a hub
// apart from its readings.
type Config struct {
	Version   int               `json:"version"`
	Exported  int64             `json:"exported"`
	Sensors   []store.Sensor    `json:"sensors"`
	Dashboard json.RawMessage   `json:"dashboard,omitempty"`
	Devices   []auth.Credential `json:"devices,omitempty"`
}

// Export builds a Config. Device credentials are included only when asked:
// they are hashes of 256-bit tokens (not reversible), but they do let the
// backup's holder restore working access.
func Export(st *store.Store, creds *auth.Store, devices bool) Config {
	c := Config{Version: Version, Exported: time.Now().UnixMilli(), Sensors: st.Definitions(), Dashboard: st.Dashboard()}
	if devices && creds != nil {
		c.Devices = creds.Export()
	}
	return c
}

// Plan is what an import does (or would do, for a dry run).
type Plan struct {
	Created   []string `json:"created"`
	Updated   []string `json:"updated"`
	Deleted   []string `json:"deleted"`
	Dashboard bool     `json:"dashboard"`
	Devices   int      `json:"devices"`
	Revoked   []string `json:"revokedDevices"` // sessions dropped: removed or token changed
	DryRun    bool     `json:"dryRun"`
}

// Import validates the whole backup before changing anything, so a bad
// file never leaves the hub half-restored. replace also deletes sensors (and
// devices, when the backup has them) that are not in the backup.
func Import(c Config, st *store.Store, creds *auth.Store, replace, dryRun bool) (Plan, error) {
	p := Plan{Created: []string{}, Updated: []string{}, Deleted: []string{}, Revoked: []string{}, DryRun: dryRun}
	if c.Version != Version {
		return p, fmt.Errorf("%w: backup version %d, this hub reads %d", store.ErrInvalid, c.Version, Version)
	}
	existing := map[string]bool{}
	for _, d := range st.Definitions() {
		existing[d.ID] = true
	}
	inBackup := map[string]bool{}
	for _, d := range c.Sensors {
		if inBackup[d.ID] {
			return p, fmt.Errorf("%w: sensor %q appears twice", store.ErrInvalid, d.ID)
		}
		inBackup[d.ID] = true
		if err := st.Validate(d); err != nil {
			return p, fmt.Errorf("sensor %q: %w", d.ID, err)
		}
		if existing[d.ID] {
			p.Updated = append(p.Updated, d.ID)
		} else {
			p.Created = append(p.Created, d.ID)
		}
	}
	if replace {
		for id := range existing {
			if !inBackup[id] {
				p.Deleted = append(p.Deleted, id)
			}
		}
		sort.Strings(p.Deleted)
	}
	if n := len(existing) - len(p.Deleted) + len(p.Created); n > st.MaxSensors() {
		return p, fmt.Errorf("%w: the result would have %d sensors, limit %d (-max-sensors)", store.ErrLimit, n, st.MaxSensors())
	}
	if len(c.Dashboard) > 0 {
		if !json.Valid(c.Dashboard) {
			return p, fmt.Errorf("%w: dashboard is not valid JSON", store.ErrInvalid)
		}
		p.Dashboard = true
	}
	if len(c.Devices) > 0 {
		if creds == nil || !creds.Persistent() {
			return p, fmt.Errorf("%w: the backup has device credentials but this hub has no -auth-file to keep them", store.ErrInvalid)
		}
		revoked, err := creds.CheckImport(c.Devices, replace)
		if err != nil {
			return p, err
		}
		p.Devices, p.Revoked = len(c.Devices), append(p.Revoked, revoked...)
	}
	if dryRun {
		return p, nil
	}

	for _, id := range p.Deleted { // first, so their slots count toward the limit
		_ = st.Delete(id)
	}
	for _, d := range c.Sensors {
		if err := st.Upsert(d); err != nil { // only a concurrent auto-registration can hit the limit now
			return p, fmt.Errorf("sensor %q: %w", d.ID, err)
		}
	}
	if p.Dashboard {
		if err := st.SetDashboard(c.Dashboard); err != nil {
			return p, err
		}
	}
	if len(c.Devices) > 0 {
		revoked, err := creds.Import(c.Devices, replace)
		if err != nil {
			return p, err
		}
		p.Revoked = append(p.Revoked[:0], revoked...)
	}
	return p, nil
}

// File names sort by time: iothub-20261002-060000.db / .json
var nameRe = regexp.MustCompile(`^iothub-\d{8}-\d{6}\.(db|json)$`)

// ValidName guards downloads against path traversal.
func ValidName(name string) bool { return nameRe.MatchString(name) }

type File struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"modified"`
}

// Scheduler takes a backup every Every into Dir and keeps the newest Keep
// of each kind (database snapshot and config).
type Scheduler struct {
	Dir   string
	Every time.Duration
	Keep  int
	DB    tsdb.DB // nil or PostgreSQL: config only
	// Config returns the configuration document to store with each backup.
	Config func() Config
	Logf   func(string, ...any)
	Now    func() time.Time // for tests; default time.Now

	mu      sync.Mutex // one backup at a time
	lastErr string
	lastAt  int64
}

// Status for the API.
type Status struct {
	Dir       string `json:"dir"`
	Every     string `json:"every"`
	Keep      int    `json:"keep"`
	Database  bool   `json:"database"` // snapshots include readings
	LastAt    int64  `json:"lastAt,omitempty"`
	LastError string `json:"lastError,omitempty"`
	Files     []File `json:"files"`
}

func (s *Scheduler) snapshotter() tsdb.Snapshotter {
	if sn, ok := s.DB.(tsdb.Snapshotter); ok && sn.CanSnapshot() {
		return sn
	}
	return nil
}

func (s *Scheduler) Status() (Status, error) {
	s.mu.Lock()
	st := Status{Dir: s.Dir, Every: s.Every.String(), Keep: s.Keep, LastAt: s.lastAt, LastError: s.lastErr, Files: []File{}}
	s.mu.Unlock()
	st.Database = s.snapshotter() != nil
	files, err := s.list()
	st.Files = append(st.Files, files...)
	return st, err
}

func (s *Scheduler) list() ([]File, error) {
	ents, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []File
	for _, e := range ents {
		if !ValidName(e.Name()) {
			continue
		}
		if fi, err := e.Info(); err == nil && fi.Mode().IsRegular() {
			out = append(out, File{Name: e.Name(), Size: fi.Size(), ModTime: fi.ModTime().UnixMilli()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name }) // newest first
	return out, nil
}

// Path resolves a backup file name for download.
func (s *Scheduler) Path(name string) (string, bool) {
	if !ValidName(name) {
		return "", false
	}
	p := filepath.Join(s.Dir, name)
	fi, err := os.Stat(p)
	return p, err == nil && fi.Mode().IsRegular()
}

// Once takes one backup now and prunes old ones. It returns the files written.
func (s *Scheduler) Once(ctx context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	files, err := s.once(ctx)
	s.lastAt = time.Now().UnixMilli()
	s.lastErr = ""
	if err != nil {
		s.lastErr = err.Error()
	}
	return files, err
}

func (s *Scheduler) once(ctx context.Context) ([]string, error) {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil { // backups can hold credential hashes
		return nil, err
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	stamp := now().UTC().Format("20060102-150405")
	base := filepath.Join(s.Dir, "iothub-"+stamp)
	var written []string

	// Write to a temporary name and rename, so a crash or a full disk
	// never leaves a truncated file that looks like a good backup.
	if sn := s.snapshotter(); sn != nil {
		tmp := base + ".db.partial"
		_ = os.Remove(tmp) // VACUUM INTO refuses an existing file
		if err := sn.Snapshot(ctx, tmp); err != nil {
			_ = os.Remove(tmp)
			return written, fmt.Errorf("database snapshot: %w", err)
		}
		if err := os.Chmod(tmp, 0o600); err != nil {
			return written, err
		}
		if err := os.Rename(tmp, base+".db"); err != nil {
			return written, err
		}
		written = append(written, filepath.Base(base+".db"))
	}
	if s.Config != nil {
		b, err := json.MarshalIndent(s.Config(), "", "  ")
		if err != nil {
			return written, err
		}
		tmp := base + ".json.partial"
		if err := os.WriteFile(tmp, b, 0o600); err != nil {
			_ = os.Remove(tmp)
			return written, err
		}
		if err := os.Rename(tmp, base+".json"); err != nil {
			return written, err
		}
		written = append(written, filepath.Base(base+".json"))
	}
	return written, s.prune()
}

func (s *Scheduler) prune() error {
	files, err := s.list()
	if err != nil || s.Keep <= 0 {
		return err
	}
	kept := map[string]int{}
	for _, f := range files { // newest first
		ext := filepath.Ext(f.Name)
		if kept[ext]++; kept[ext] > s.Keep {
			if err := os.Remove(filepath.Join(s.Dir, f.Name)); err != nil {
				return err
			}
		}
	}
	return nil
}

// Run takes a backup every Every. After a restart it continues the cycle
// from the newest existing backup rather than starting over, so frequent
// restarts don't postpone backups forever; an overdue one runs after a
// minute.
func (s *Scheduler) Run(ctx context.Context) {
	failed := false
	for {
		wait := time.Minute
		if failed {
			wait = min(s.Every, 15*time.Minute) // e.g. a full disk: retry, but don't spin
		}
		if files, _ := s.list(); len(files) > 0 {
			if t, err := time.Parse("20060102-150405", strings.TrimSuffix(strings.TrimPrefix(files[0].Name, "iothub-"), filepath.Ext(files[0].Name))); err == nil {
				wait = max(wait, time.Until(t.Add(s.Every)))
			}
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		files, err := s.Once(ctx)
		if failed = err != nil; failed {
			s.Logf("backup failed: %v", err)
		} else {
			s.Logf("backup: wrote %s", strings.Join(files, ", "))
		}
	}
}
