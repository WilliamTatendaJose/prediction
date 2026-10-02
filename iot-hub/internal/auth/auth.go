// Package auth holds credentials and permissions.
//
// Every identity belongs to a tenant and has a role and the sensors it may
// touch:
//
//	superadmin  the platform operator: every tenant, tenant management
//	admin       everything inside its tenant
//	service     read, subscribe, ingest and define matching sensors (e.g. the ML bridge)
//	operator    read, plus acknowledge/shelve alarms and write notes
//	device      ingest/publish to matching sensors only
//	viewer      read-only (dashboard, API reads, MQTT subscribe)
//
// An identity authenticates with either or both of:
//   - a random token, of which only the SHA-256 is stored;
//   - a primary/secondary key pair (as in Azure IoT Hub), from which the
//     device or a backend signs short-lived SAS tokens. Two keys allow
//     rotation without downtime: move devices to the secondary, regenerate
//     the primary. Keys must be stored to verify signatures; with a master
//     key they are encrypted at rest (AES-256-GCM).
//
// Sensor patterns are globs: "env-*", "*-ml", "*". Tokens and keys are
// 256-bit, so a fast hash is sufficient (no password stretching needed).
//
// A Store is a view of one shared credential database: New returns the
// "default" tenant's view; Tenant(id) another tenant's; Platform() all of
// them (for the gateway, the broker and the superadmin).
package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Role string

const (
	Superadmin Role = "superadmin"
	Admin      Role = "admin"
	Operator   Role = "operator"
	Service    Role = "service"
	Device     Role = "device"
	Viewer     Role = "viewer"
)

// Valid reports whether r can be given to a tenant identity.
func (r Role) Valid() bool {
	return r == Admin || r == Operator || r == Service || r == Device || r == Viewer
}

type Action int

const (
	Read      Action = iota // API reads, dashboard data, SSE
	Subscribe               // MQTT subscribe
	Ingest                  // HTTP data / MQTT publish for a sensor
	Define                  // create/update/delete a sensor definition
	Manage                  // dashboard layout, devices, settings, jobs
	Operate                 // acknowledge/shelve alarms, notes
	Platform                // tenants, quotas: superadmin only
)

// DefaultTenant is the only tenant in single-tenant mode.
const DefaultTenant = "default"

// Identity is an authenticated caller.
type Identity struct {
	Tenant  string   `json:"tenant,omitempty"` // empty for superadmins
	ID      string   `json:"id"`
	Role    Role     `json:"role"`
	Sensors []string `json:"sensors,omitempty"`
}

// Anonymous is used when authentication is disabled (single-tenant only).
var Anonymous = &Identity{Tenant: DefaultTenant, ID: "anonymous", Role: Admin}

func (i *Identity) matches(sensor string) bool {
	for _, p := range i.Sensors {
		if ok, _ := path.Match(p, sensor); ok {
			return true
		}
	}
	return false
}

// Can reports whether the identity may perform action (on sensor, for
// Ingest and Define). It does not check the tenant: callers resolve the
// tenant first and only ever hand an identity to its own tenant.
func (i *Identity) Can(a Action, sensor string) bool {
	if i == nil {
		return false
	}
	switch i.Role {
	case Superadmin:
		return true
	case Admin:
		return a != Platform
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

// InTenant reports whether the identity may act in tenant t.
func (i *Identity) InTenant(t string) bool {
	return i != nil && (i.Role == Superadmin || i.Tenant == t)
}

// Keys is a stored key pair. Values are base64 (plain) or "v1:" + base64
// of nonce||ciphertext when a master key encrypts them at rest.
type Keys struct {
	Primary   string `json:"primary"`
	Secondary string `json:"secondary"`
}

type record struct {
	Identity
	Hash     string `json:"hash,omitempty"` // hex SHA-256 of the token
	Keys     *Keys  `json:"keys,omitempty"`
	Disabled bool   `json:"disabled,omitempty"`
	Expires  int64  `json:"expires,omitempty"` // token expiry, unix ms; 0 = never
	Created  int64  `json:"created"`
	Note     string `json:"note,omitempty"`
}

func (r *record) key() string { return r.Tenant + "\x00" + r.ID }

type core struct {
	path      string
	superHash []byte // from -token; never persisted
	seal      cipher.AEAD

	mu     sync.RWMutex
	saveMu sync.Mutex // serialises snapshot+write so saves never reorder
	// enabled is sticky: once a credentials file exists or a credential was
	// added, removing the last one must not silently re-open the hub.
	enabled bool
	byHash  map[string]*record
	byKey   map[string]*record // tenant\x00id
	now     func() time.Time
}

// Store is a view of the credential database for one tenant, or for all
// of them (Platform).
type Store struct {
	c      *core
	tenant string // "" = platform view
}

// New opens the credential database (not yet loaded) and returns the
// default tenant's view. superToken is the superadmin token (-token).
func New(path, superToken string) *Store {
	c := &core{path: path, byHash: map[string]*record{}, byKey: map[string]*record{}, now: time.Now}
	if superToken != "" {
		h := sha256.Sum256([]byte(superToken))
		c.superHash = h[:]
	}
	return &Store{c: c, tenant: DefaultTenant}
}

// SetMasterKey encrypts stored keys at rest from now on (call before Load).
// The AES key is SHA-256 of the master secret, so use a long random value.
func (s *Store) SetMasterKey(secret string) error {
	if secret == "" {
		return nil
	}
	k := sha256.Sum256([]byte(secret))
	b, err := aes.NewCipher(k[:])
	if err != nil {
		return err
	}
	g, err := cipher.NewGCM(b)
	if err != nil {
		return err
	}
	s.c.seal = g
	return nil
}

// Tenant returns the view of tenant id (sharing the same database).
func (s *Store) Tenant(id string) *Store { return &Store{c: s.c, tenant: id} }

// Platform returns the view across all tenants.
func (s *Store) Platform() *Store { return &Store{c: s.c} }

// TenantID is the view's tenant ("" for the platform view).
func (s *Store) TenantID() string { return s.tenant }

// Enabled is true once any credential exists; until then the hub is open.
func (s *Store) Enabled() bool {
	s.c.mu.RLock()
	defer s.c.mu.RUnlock()
	return s.c.superHash != nil || s.c.enabled
}

func hashHex(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// SASPrefix starts a shared access signature, as in Azure IoT Hub.
const SASPrefix = "SharedAccessSignature "

// MaxSASLifetime caps how far ahead a SAS token may expire.
const MaxSASLifetime = 366 * 24 * time.Hour

// Authenticate resolves a token or SAS token. With auth disabled every
// caller is an anonymous admin of the default tenant. A tenant view only
// accepts its own identities (and superadmins).
func (s *Store) Authenticate(token string) (*Identity, bool) {
	if !s.Enabled() {
		return Anonymous, true
	}
	if token == "" {
		return nil, false
	}
	if strings.HasPrefix(token, SASPrefix) {
		return s.authSAS(token)
	}
	h := sha256.Sum256([]byte(token))
	if s.c.superHash != nil && subtle.ConstantTimeCompare(h[:], s.c.superHash) == 1 {
		return &Identity{ID: "admin", Role: Superadmin}, true
	}
	s.c.mu.RLock()
	r, ok := s.c.byHash[hex.EncodeToString(h[:])]
	var id Identity
	if ok {
		id = r.Identity
		ok = !r.Disabled && (r.Expires == 0 || s.c.now().UnixMilli() < r.Expires)
	}
	s.c.mu.RUnlock()
	if !ok || !s.visible(id.Tenant) {
		return nil, false
	}
	return &id, true
}

func (s *Store) visible(tenant string) bool { return s.tenant == "" || s.tenant == tenant }

// ResourceURI is what a SAS token for a device is scoped to.
func ResourceURI(tenant, id string) string { return tenant + "/devices/" + id }

// SignSAS builds a SAS token for resource, valid until expiry, signed with
// a base64 key: HMAC-SHA256(key, urlencode(resource) + "\n" + expiry),
// the same construction as Azure IoT Hub, so its client libraries and
// samples can generate tokens for this hub.
func SignSAS(resource, key string, expiry time.Time) (string, error) {
	k, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		return "", fmt.Errorf("%w: key is not base64", ErrInvalid)
	}
	sr := url.QueryEscape(resource)
	se := strconv.FormatInt(expiry.Unix(), 10)
	return SASPrefix + "sr=" + sr + "&sig=" + url.QueryEscape(sasSig(k, sr, se)) + "&se=" + se, nil
}

func sasSig(key []byte, sr, se string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(sr + "\n" + se))
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

func (s *Store) authSAS(token string) (*Identity, bool) {
	q, err := url.ParseQuery(strings.TrimPrefix(token, SASPrefix))
	if err != nil || q.Get("sr") == "" || q.Get("sig") == "" || q.Get("se") == "" {
		return nil, false
	}
	se, err := strconv.ParseInt(q.Get("se"), 10, 64)
	now := s.c.now()
	if err != nil || se <= now.Unix() || se > now.Add(MaxSASLifetime).Unix() {
		return nil, false
	}
	tenant, rest, ok := strings.Cut(q.Get("sr"), "/devices/")
	if !ok || !s.visible(tenant) {
		return nil, false
	}
	s.c.mu.RLock()
	r := s.c.byKey[tenant+"\x00"+rest]
	var keys Keys
	var id Identity
	if r != nil && r.Keys != nil && !r.Disabled {
		keys, id = *r.Keys, r.Identity
	}
	s.c.mu.RUnlock()
	if keys.Primary == "" {
		return nil, false
	}
	sr := url.QueryEscape(q.Get("sr"))
	got := []byte(q.Get("sig"))
	match := false
	for _, k := range []string{keys.Primary, keys.Secondary} {
		raw, err := s.c.open(k)
		if err != nil {
			continue
		}
		if subtle.ConstantTimeCompare(got, []byte(sasSig(raw, sr, q.Get("se")))) == 1 {
			match = true
		}
	}
	if !match {
		return nil, false
	}
	return &id, true
}

var idRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

var (
	ErrExists   = errors.New("already exists")
	ErrNotFound = errors.New("not found")
	ErrInvalid  = errors.New("invalid")
)

func newSecret() ([]byte, error) {
	b := make([]byte, 32)
	_, err := rand.Read(b)
	return b, err
}

func newToken() (string, error) {
	b, err := newSecret()
	return "iot_" + base64.RawURLEncoding.EncodeToString(b), err
}

// seal encrypts raw key bytes for storage (or encodes them, without a
// master key).
func (c *core) sealKey(raw []byte) (string, error) {
	if c.seal == nil {
		return base64.StdEncoding.EncodeToString(raw), nil
	}
	nonce := make([]byte, c.seal.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return "v1:" + base64.StdEncoding.EncodeToString(c.seal.Seal(nonce, nonce, raw, nil)), nil
}

func (c *core) open(stored string) ([]byte, error) {
	enc, ok := strings.CutPrefix(stored, "v1:")
	if !ok {
		return base64.StdEncoding.DecodeString(stored)
	}
	if c.seal == nil {
		return nil, errors.New("keys are encrypted: set the master key")
	}
	b, err := base64.StdEncoding.DecodeString(enc)
	if err != nil || len(b) < c.seal.NonceSize() {
		return nil, errors.New("corrupt key")
	}
	return c.seal.Open(nil, b[:c.seal.NonceSize()], b[c.seal.NonceSize():], nil)
}

// Spec describes a new identity.
type Spec struct {
	ID      string   `json:"id"`
	Role    Role     `json:"role"`
	Sensors []string `json:"sensors,omitempty"`
	Note    string   `json:"note,omitempty"`
	// Auth: "token" (default), "keys" (SAS) or "both".
	Auth    string `json:"auth,omitempty"`
	Expires int64  `json:"expires,omitempty"` // token expiry, unix ms
}

// Secrets are shown once, when created or rotated.
type Secrets struct {
	Token        string `json:"token,omitempty"`
	PrimaryKey   string `json:"primaryKey,omitempty"`
	SecondaryKey string `json:"secondaryKey,omitempty"`
}

func validate(id string, role Role, sensors []string) error {
	if !idRe.MatchString(id) || id == "admin" || id == "anonymous" {
		return fmt.Errorf("%w: id must match %s and not be reserved", ErrInvalid, idRe)
	}
	if !role.Valid() {
		return fmt.Errorf("%w: role must be admin, operator, service, device or viewer", ErrInvalid)
	}
	for _, p := range sensors {
		if _, err := path.Match(p, ""); err != nil {
			return fmt.Errorf("%w: bad sensor pattern %q", ErrInvalid, p)
		}
	}
	if (role == Device || role == Service) && len(sensors) == 0 {
		return fmt.Errorf("%w: %s needs at least one sensor pattern", ErrInvalid, role)
	}
	return nil
}

// Add creates an identity with a token and returns the token. The token is
// shown once; only its hash is kept.
func (s *Store) Add(id string, role Role, sensors []string, note string) (string, error) {
	sec, err := s.Create(Spec{ID: id, Role: role, Sensors: sensors, Note: note})
	return sec.Token, err
}

// Create adds an identity to this tenant.
func (s *Store) Create(sp Spec) (Secrets, error) {
	var sec Secrets
	if s.tenant == "" {
		return sec, fmt.Errorf("%w: choose a tenant", ErrInvalid)
	}
	if err := validate(sp.ID, sp.Role, sp.Sensors); err != nil {
		return sec, err
	}
	if sp.Expires != 0 && sp.Expires <= s.c.now().UnixMilli() {
		return sec, fmt.Errorf("%w: expiry is in the past", ErrInvalid)
	}
	r := &record{Identity: Identity{Tenant: s.tenant, ID: sp.ID, Role: sp.Role, Sensors: sp.Sensors},
		Created: s.c.now().UnixMilli(), Note: sp.Note, Expires: sp.Expires}
	switch sp.Auth {
	case "", "token", "both", "keys":
	default:
		return sec, fmt.Errorf("%w: auth must be token, keys or both", ErrInvalid)
	}
	if sp.Auth != "keys" {
		tok, err := newToken()
		if err != nil {
			return sec, err
		}
		sec.Token, r.Hash = tok, hashHex(tok)
	}
	if sp.Auth == "keys" || sp.Auth == "both" {
		k, err := s.c.newKeys(&sec)
		if err != nil {
			return sec, err
		}
		r.Keys = k
	}

	c := s.c
	c.mu.Lock()
	if _, ok := c.byKey[r.key()]; ok {
		c.mu.Unlock()
		return Secrets{}, ErrExists
	}
	c.put(r)
	wasEnabled := c.enabled
	c.enabled = true
	c.mu.Unlock()
	if err := c.save(); err != nil {
		c.mu.Lock()
		c.drop(r)
		c.enabled = wasEnabled
		c.mu.Unlock()
		return Secrets{}, err
	}
	return sec, nil
}

func (c *core) newKeys(sec *Secrets) (*Keys, error) {
	var k Keys
	for _, dst := range []struct {
		stored, shown *string
	}{{&k.Primary, &sec.PrimaryKey}, {&k.Secondary, &sec.SecondaryKey}} {
		raw, err := newSecret()
		if err != nil {
			return nil, err
		}
		if *dst.stored, err = c.sealKey(raw); err != nil {
			return nil, err
		}
		*dst.shown = base64.StdEncoding.EncodeToString(raw)
	}
	return &k, nil
}

// put/drop index a record; callers hold c.mu.
func (c *core) put(r *record) {
	c.byKey[r.key()] = r
	if r.Hash != "" {
		c.byHash[r.Hash] = r
	}
}

func (c *core) drop(r *record) {
	delete(c.byKey, r.key())
	if r.Hash != "" && c.byHash[r.Hash] == r {
		delete(c.byHash, r.Hash)
	}
}

func (s *Store) Remove(id string) error {
	c := s.c
	c.mu.Lock()
	r, ok := c.byKey[s.tenant+"\x00"+id]
	if ok {
		c.drop(r)
	}
	c.mu.Unlock()
	if !ok {
		return ErrNotFound
	}
	return c.save()
}

// RemoveTenant deletes every identity of a tenant and returns their ids.
func (s *Store) RemoveTenant(tenant string) ([]string, error) {
	c := s.c
	var ids []string
	c.mu.Lock()
	for _, r := range c.byKey {
		if r.Tenant == tenant {
			c.drop(r)
			ids = append(ids, r.ID)
		}
	}
	c.mu.Unlock()
	sort.Strings(ids)
	return ids, c.save()
}

// Update changes an identity's sensors, note, disabled state or expiry.
type Update struct {
	Sensors  *[]string `json:"sensors,omitempty"`
	Note     *string   `json:"note,omitempty"`
	Disabled *bool     `json:"disabled,omitempty"`
	Expires  *int64    `json:"expires,omitempty"`
}

func (s *Store) Update(id string, u Update) error {
	c := s.c
	c.mu.Lock()
	r, ok := c.byKey[s.tenant+"\x00"+id]
	if !ok {
		c.mu.Unlock()
		return ErrNotFound
	}
	next := *r
	if u.Sensors != nil {
		next.Sensors = *u.Sensors
	}
	if u.Note != nil {
		next.Note = *u.Note
	}
	if u.Disabled != nil {
		next.Disabled = *u.Disabled
	}
	if u.Expires != nil {
		next.Expires = *u.Expires
	}
	if err := validate(next.ID, next.Role, next.Sensors); err != nil {
		c.mu.Unlock()
		return err
	}
	*r = next
	c.mu.Unlock()
	return c.save()
}

// Rotate replaces one secret: "token", "primary" or "secondary". The old
// value stops working at once; the other key keeps working.
func (s *Store) Rotate(id, which string) (Secrets, error) {
	var sec Secrets
	c := s.c
	c.mu.Lock()
	defer func() {
		if sec != (Secrets{}) {
			_ = c.save()
		}
	}()
	defer c.mu.Unlock()
	r, ok := c.byKey[s.tenant+"\x00"+id]
	if !ok {
		return sec, ErrNotFound
	}
	switch which {
	case "token":
		tok, err := newToken()
		if err != nil {
			return sec, err
		}
		if r.Hash != "" {
			delete(c.byHash, r.Hash)
		}
		r.Hash = hashHex(tok)
		c.byHash[r.Hash] = r
		sec.Token = tok
	case "primary", "secondary":
		if r.Keys == nil { // first keys for a token-only identity: show both
			k, err := c.newKeys(&sec)
			if err != nil {
				return Secrets{}, err
			}
			r.Keys = k
			return sec, nil
		}
		raw, err := newSecret()
		if err != nil {
			return sec, err
		}
		stored, err := c.sealKey(raw)
		if err != nil {
			return sec, err
		}
		shown := base64.StdEncoding.EncodeToString(raw)
		if which == "primary" {
			r.Keys.Primary, sec.PrimaryKey = stored, shown
		} else {
			r.Keys.Secondary, sec.SecondaryKey = stored, shown
		}
	default:
		return sec, fmt.Errorf("%w: rotate token, primary or secondary", ErrInvalid)
	}
	return sec, nil
}

// KeysOf returns an identity's keys in plain form (for showing a
// connection string to an administrator).
func (s *Store) KeysOf(id string) (Secrets, error) {
	c := s.c
	c.mu.RLock()
	r, ok := c.byKey[s.tenant+"\x00"+id]
	var k Keys
	if ok && r.Keys != nil {
		k = *r.Keys
	}
	c.mu.RUnlock()
	if !ok {
		return Secrets{}, ErrNotFound
	}
	if k.Primary == "" {
		return Secrets{}, fmt.Errorf("%w: %s has no keys", ErrInvalid, id)
	}
	p, err := c.open(k.Primary)
	if err != nil {
		return Secrets{}, err
	}
	q, err := c.open(k.Secondary)
	if err != nil {
		return Secrets{}, err
	}
	return Secrets{PrimaryKey: base64.StdEncoding.EncodeToString(p), SecondaryKey: base64.StdEncoding.EncodeToString(q)}, nil
}

// Credential is a stored identity as exported in a config backup: hashes
// and (encrypted, with a master key) keys, never tokens, so a restore keeps
// existing tokens working.
type Credential struct {
	Identity
	Hash     string `json:"hash,omitempty"`
	Keys     *Keys  `json:"keys,omitempty"`
	Disabled bool   `json:"disabled,omitempty"`
	Expires  int64  `json:"expires,omitempty"`
	Created  int64  `json:"created"`
	Note     string `json:"note,omitempty"`
}

// Persistent reports whether credentials are saved to a file.
func (s *Store) Persistent() bool { return s.c.path != "" }

func (s *Store) records() []*record {
	s.c.mu.RLock()
	out := make([]*record, 0, len(s.c.byKey))
	for _, r := range s.c.byKey {
		if s.visible(r.Tenant) {
			cp := *r
			out = append(out, &cp)
		}
	}
	s.c.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].key() < out[j].key() })
	return out
}

func (s *Store) Export() []Credential {
	recs := s.records()
	out := make([]Credential, len(recs))
	for i, r := range recs {
		out[i] = Credential(*r)
	}
	return out
}

var hashRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// CheckImport validates credentials for this tenant and reports which
// existing identities an import would revoke: removed (replace) or given a
// different token or keys.
func (s *Store) CheckImport(creds []Credential, replace bool) (revoked []string, err error) {
	if s.tenant == "" {
		return nil, fmt.Errorf("%w: choose a tenant", ErrInvalid)
	}
	seen, hashes := map[string]bool{}, map[string]bool{}
	for _, c := range creds {
		if seen[c.ID] {
			return nil, fmt.Errorf("%w: credential id %q appears twice", ErrInvalid, c.ID)
		}
		seen[c.ID] = true
		if err := validate(c.ID, c.Role, c.Sensors); err != nil {
			return nil, fmt.Errorf("credential %q: %w", c.ID, err)
		}
		if c.Hash == "" && c.Keys == nil {
			return nil, fmt.Errorf("%w: credential %q has neither token hash nor keys", ErrInvalid, c.ID)
		}
		if c.Hash != "" {
			if !hashRe.MatchString(c.Hash) {
				return nil, fmt.Errorf("%w: credential %q: bad hash", ErrInvalid, c.ID)
			}
			if hashes[c.Hash] {
				return nil, fmt.Errorf("%w: credential %q shares a token with another", ErrInvalid, c.ID)
			}
			hashes[c.Hash] = true
		}
		if c.Keys != nil {
			for _, k := range []string{c.Keys.Primary, c.Keys.Secondary} {
				if _, err := s.c.open(k); err != nil {
					return nil, fmt.Errorf("%w: credential %q: keys: %v", ErrInvalid, c.ID, err)
				}
			}
		}
	}
	s.c.mu.RLock()
	defer s.c.mu.RUnlock()
	byID := map[string]Credential{}
	for _, c := range creds {
		byID[c.ID] = c
	}
	for _, c := range creds {
		if r, ok := s.c.byHash[c.Hash]; ok && c.Hash != "" && r.key() != s.tenant+"\x00"+c.ID {
			if replace && r.Tenant == s.tenant {
				continue // that identity is being replaced too
			}
			return nil, fmt.Errorf("%w: credential %q has the token of existing %q", ErrInvalid, c.ID, r.ID)
		}
	}
	for _, r := range s.c.byKey {
		if r.Tenant != s.tenant {
			continue
		}
		c, ok := byID[r.ID]
		if ok && (c.Hash != r.Hash || !sameKeys(c.Keys, r.Keys)) || !ok && replace {
			revoked = append(revoked, r.ID)
		}
	}
	sort.Strings(revoked)
	return revoked, nil
}

func sameKeys(a, b *Keys) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// Import adds or updates credentials in this tenant (replace: and removes
// the tenant's others). It returns the identities whose sessions must be
// dropped.
func (s *Store) Import(creds []Credential, replace bool) ([]string, error) {
	revoked, err := s.CheckImport(creds, replace)
	if err != nil {
		return nil, err
	}
	c := s.c
	c.mu.Lock()
	if replace {
		for _, r := range c.byKey {
			if r.Tenant == s.tenant {
				c.drop(r)
			}
		}
	}
	for _, cr := range creds {
		if old, ok := c.byKey[s.tenant+"\x00"+cr.ID]; ok {
			c.drop(old)
		}
		r := record(cr)
		r.Tenant = s.tenant
		c.put(&r)
	}
	if len(creds) > 0 {
		c.enabled = true
	}
	c.mu.Unlock()
	return revoked, c.save()
}

// DeviceInfo is the listing shape: never includes hashes or keys.
type DeviceInfo struct {
	Identity
	Created  int64  `json:"created"`
	Note     string `json:"note,omitempty"`
	Disabled bool   `json:"disabled,omitempty"`
	Expires  int64  `json:"expires,omitempty"`
	Token    bool   `json:"token"` // has a token
	Keys     bool   `json:"keys"`  // has SAS keys
}

func (s *Store) List() []DeviceInfo {
	recs := s.records()
	out := make([]DeviceInfo, len(recs))
	for i, r := range recs {
		out[i] = DeviceInfo{Identity: r.Identity, Created: r.Created, Note: r.Note, Disabled: r.Disabled,
			Expires: r.Expires, Token: r.Hash != "", Keys: r.Keys != nil}
	}
	return out
}

// Count is the number of identities in this view.
func (s *Store) Count() int {
	s.c.mu.RLock()
	defer s.c.mu.RUnlock()
	n := 0
	for _, r := range s.c.byKey {
		if s.visible(r.Tenant) {
			n++
		}
	}
	return n
}

func (s *Store) Load() error {
	c := s.c
	if c.path == "" {
		return nil
	}
	b, err := os.ReadFile(c.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var recs []*record
	if err := json.Unmarshal(b, &recs); err != nil {
		return fmt.Errorf("parse %s: %w", c.path, err)
	}
	migrate := false
	c.mu.Lock()
	c.enabled = true // the file exists, even if every credential was revoked
	for _, r := range recs {
		if r.Tenant == "" { // written before tenants existed
			r.Tenant = DefaultTenant
		}
		if r.Keys != nil && c.seal != nil && !strings.HasPrefix(r.Keys.Primary, "v1:") {
			// A master key was added: encrypt existing plain keys.
			for _, k := range []*string{&r.Keys.Primary, &r.Keys.Secondary} {
				raw, err := c.open(*k)
				if err != nil {
					c.mu.Unlock()
					return fmt.Errorf("%s: %s: %w", c.path, r.ID, err)
				}
				if *k, err = c.sealKey(raw); err != nil {
					c.mu.Unlock()
					return err
				}
			}
			migrate = true
		}
		c.put(r)
	}
	c.mu.Unlock()
	if migrate {
		return c.save()
	}
	return nil
}

func (c *core) save() error {
	if c.path == "" {
		return nil
	}
	c.saveMu.Lock()
	defer c.saveMu.Unlock()
	c.mu.RLock()
	recs := make([]*record, 0, len(c.byKey))
	for _, r := range c.byKey {
		cp := *r
		recs = append(recs, &cp)
	}
	c.mu.RUnlock()
	sort.Slice(recs, func(i, j int) bool { return recs[i].key() < recs[j].key() })
	b, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}
