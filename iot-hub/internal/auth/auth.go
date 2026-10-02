// Package auth holds credentials and permissions. Every client (device,
// service, person) gets its own random token; only its SHA-256 is stored.
// A token belongs to an identity with a role and the sensors it may touch:
//
//	admin    everything, including device and dashboard management
//	service  read, subscribe, ingest and define matching sensors (e.g. the ML bridge)
//	operator read, plus acknowledge/shelve alarms and write notes
//	device   ingest/publish to matching sensors only
//	viewer   read-only (dashboard, API reads, MQTT subscribe)
//
// Sensor patterns are globs: "env-*", "*-ml", "*". Tokens are 256-bit, so a
// fast hash is sufficient (no password stretching needed).
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

type Role string

const (
	Admin    Role = "admin"
	Operator Role = "operator"
	Service  Role = "service"
	Device   Role = "device"
	Viewer   Role = "viewer"
)

func (r Role) Valid() bool {
	return r == Admin || r == Operator || r == Service || r == Device || r == Viewer
}

type Action int

const (
	Read      Action = iota // API reads, dashboard data, SSE
	Subscribe               // MQTT subscribe
	Ingest                  // HTTP data / MQTT publish for a sensor
	Define                  // create/update/delete a sensor definition
	Manage                  // dashboard layout, devices
	Operate                 // acknowledge/shelve alarms, notes
)

// Identity is an authenticated caller.
type Identity struct {
	ID      string   `json:"id"`
	Role    Role     `json:"role"`
	Sensors []string `json:"sensors,omitempty"`
}

// Anonymous is used when authentication is disabled.
var Anonymous = &Identity{ID: "anonymous", Role: Admin}

func (i *Identity) matches(sensor string) bool {
	for _, p := range i.Sensors {
		if ok, _ := path.Match(p, sensor); ok {
			return true
		}
	}
	return false
}

// Can reports whether the identity may perform action (on sensor, for
// Ingest and Define).
func (i *Identity) Can(a Action, sensor string) bool {
	if i == nil {
		return false
	}
	switch i.Role {
	case Admin:
		return true
	case Service:
		switch a {
		case Read, Subscribe:
			return true
		case Ingest, Define:
			return i.matches(sensor)
		}
	case Device:
		return a == Ingest && i.matches(sensor)
	case Operator:
		return a == Read || a == Subscribe || a == Operate
	case Viewer:
		return a == Read || a == Subscribe
	}
	return false
}

type record struct {
	Identity
	Hash    string `json:"hash"` // hex SHA-256 of the token
	Created int64  `json:"created"`
	Note    string `json:"note,omitempty"`
}

// Store is the credential database, persisted to a JSON file (mode 0600).
type Store struct {
	path      string
	adminHash []byte // from -token; never persisted

	mu     sync.RWMutex
	saveMu sync.Mutex // serialises snapshot+write so saves never reorder
	// enabled is sticky: once a credentials file exists or a credential was
	// added, removing the last one must not silently re-open the hub.
	enabled bool
	byHash  map[string]*record
	byID    map[string]*record
}

func New(path, adminToken string) *Store {
	s := &Store{path: path, byHash: map[string]*record{}, byID: map[string]*record{}}
	if adminToken != "" {
		h := sha256.Sum256([]byte(adminToken))
		s.adminHash = h[:]
	}
	return s
}

// Enabled is true once any credential exists; until then the hub is open.
func (s *Store) Enabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.adminHash != nil || s.enabled
}

func hashHex(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// Authenticate resolves a token. With auth disabled every caller is admin.
func (s *Store) Authenticate(token string) (*Identity, bool) {
	if !s.Enabled() {
		return Anonymous, true
	}
	if token == "" {
		return nil, false
	}
	h := sha256.Sum256([]byte(token))
	if s.adminHash != nil && subtle.ConstantTimeCompare(h[:], s.adminHash) == 1 {
		return &Identity{ID: "admin", Role: Admin}, true
	}
	s.mu.RLock()
	r, ok := s.byHash[hex.EncodeToString(h[:])]
	s.mu.RUnlock()
	if !ok {
		return nil, false
	}
	id := r.Identity
	return &id, true
}

var idRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

var (
	ErrExists   = errors.New("already exists")
	ErrNotFound = errors.New("not found")
	ErrInvalid  = errors.New("invalid")
)

// Add creates an identity and returns its token. The token is shown once;
// only its hash is kept.
func (s *Store) Add(id string, role Role, sensors []string, note string) (string, error) {
	if !idRe.MatchString(id) || id == "admin" || id == "anonymous" {
		return "", fmt.Errorf("%w: id must match %s and not be reserved", ErrInvalid, idRe)
	}
	if !role.Valid() {
		return "", fmt.Errorf("%w: role must be admin, operator, service, device or viewer", ErrInvalid)
	}
	for _, p := range sensors {
		if _, err := path.Match(p, ""); err != nil {
			return "", fmt.Errorf("%w: bad sensor pattern %q", ErrInvalid, p)
		}
	}
	if (role == Device || role == Service) && len(sensors) == 0 {
		return "", fmt.Errorf("%w: %s needs at least one sensor pattern", ErrInvalid, role)
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := "iot_" + base64.RawURLEncoding.EncodeToString(b)
	r := &record{Identity: Identity{ID: id, Role: role, Sensors: sensors}, Hash: hashHex(token),
		Created: time.Now().UnixMilli(), Note: note}

	s.mu.Lock()
	if _, ok := s.byID[id]; ok {
		s.mu.Unlock()
		return "", ErrExists
	}
	s.byID[id] = r
	s.byHash[r.Hash] = r
	wasEnabled := s.enabled
	s.enabled = true
	s.mu.Unlock()
	if err := s.save(); err != nil {
		s.mu.Lock()
		delete(s.byID, id)
		delete(s.byHash, r.Hash)
		s.enabled = wasEnabled
		s.mu.Unlock()
		return "", err
	}
	return token, nil
}

func (s *Store) Remove(id string) error {
	s.mu.Lock()
	r, ok := s.byID[id]
	if ok {
		delete(s.byID, id)
		delete(s.byHash, r.Hash)
	}
	s.mu.Unlock()
	if !ok {
		return ErrNotFound
	}
	return s.save()
}

// DeviceInfo is the listing shape: never includes the hash.
type DeviceInfo struct {
	Identity
	Created int64  `json:"created"`
	Note    string `json:"note,omitempty"`
}

func (s *Store) List() []DeviceInfo {
	s.mu.RLock()
	out := make([]DeviceInfo, 0, len(s.byID))
	for _, r := range s.byID {
		out = append(out, DeviceInfo{Identity: r.Identity, Created: r.Created, Note: r.Note})
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *Store) Load() error {
	if s.path == "" {
		return nil
	}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var recs []*record
	if err := json.Unmarshal(b, &recs); err != nil {
		return fmt.Errorf("parse %s: %w", s.path, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enabled = true // the file exists, even if every credential was revoked
	for _, r := range recs {
		s.byID[r.ID] = r
		s.byHash[r.Hash] = r
	}
	return nil
}

func (s *Store) save() error {
	if s.path == "" {
		return nil
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	s.mu.RLock()
	recs := make([]*record, 0, len(s.byID))
	for _, r := range s.byID {
		recs = append(recs, r)
	}
	s.mu.RUnlock()
	sort.Slice(recs, func(i, j int) bool { return recs[i].ID < recs[j].ID })
	b, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
