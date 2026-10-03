package auth

import (
	"encoding/hex"
	"strings"
	"time"
)

// Signing in with an email and password mints a session token, which is what
// the browser's cookie holds: there is no long-lived secret to store there.
//
// Sessions live in memory, so a hub restart signs people out and they sign in
// again. They are not credentials: each use re-reads the account, so
// disabling, deleting or expiring someone ends their sessions at once.
const (
	sessionPrefix = "ses_"
	SessionTTL    = 12 * time.Hour
)

type session struct {
	tenant, id string
	expires    int64
}

// StartSession issues a session token for an identity that has just proved
// who it is.
func (s *Store) StartSession(id *Identity, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		ttl = SessionTTL
	}
	raw, err := newSecret()
	if err != nil {
		return "", err
	}
	tok := sessionPrefix + hex.EncodeToString(raw)
	c := s.c
	c.mu.Lock()
	if c.sessions == nil {
		c.sessions = map[string]*session{}
	}
	c.sessions[hashHex(tok)] = &session{tenant: id.Tenant, id: id.ID, expires: c.now().Add(ttl).UnixMilli()}
	c.sweepSessionsLocked()
	c.mu.Unlock()
	return tok, nil
}

// EndSession drops one session (sign out). Unknown tokens are ignored.
func (s *Store) EndSession(token string) {
	if !strings.HasPrefix(token, sessionPrefix) {
		return
	}
	c := s.c
	c.mu.Lock()
	delete(c.sessions, hashHex(token))
	c.mu.Unlock()
}

// authSession resolves a session token to its current identity.
func (s *Store) authSession(token string) (*Identity, bool) {
	c := s.c
	now := c.now().UnixMilli()
	c.mu.RLock()
	defer c.mu.RUnlock()
	ses, ok := c.sessions[hashHex(token)]
	if !ok || ses.expires <= now {
		return nil, false
	}
	// Re-read the account: a session must not outlive the person's access.
	r, ok := c.byKey[ses.tenant+"\x00"+ses.id]
	if !ok || r.Disabled || (r.Expires != 0 && now >= r.Expires) || r.Pass == nil {
		return nil, false
	}
	if !s.visible(r.Tenant) {
		return nil, false
	}
	id := r.Identity
	return &id, true
}

// sweepSessionsLocked drops expired sessions; callers hold c.mu.
func (c *core) sweepSessionsLocked() {
	now := c.now().UnixMilli()
	for k, ses := range c.sessions {
		if ses.expires <= now {
			delete(c.sessions, k)
		}
	}
}

// EndSessionsFor drops every session of an identity (disabled, deleted, or
// its password changed). An empty id drops the whole tenant's sessions.
func (s *Store) EndSessionsFor(tenant, id string) {
	c := s.c
	c.mu.Lock()
	c.endSessionsLocked(tenant, id)
	c.mu.Unlock()
}

// endSessionsLocked is EndSessionsFor for callers already holding c.mu.
func (c *core) endSessionsLocked(tenant, id string) {
	for k, ses := range c.sessions {
		if ses.tenant == tenant && (id == "" || ses.id == id) {
			delete(c.sessions, k)
		}
	}
}

// IsSession reports whether a token is a session token rather than a
// credential, so callers can end it on sign-out.
func IsSession(token string) bool { return strings.HasPrefix(token, sessionPrefix) }
