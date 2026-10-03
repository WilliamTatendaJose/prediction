// Package alarm adds operator handling to anomaly episodes, loosely after
// ISA-18.2: acknowledge (with a verdict that doubles as a training label),
// a notes journal, time-limited shelving of nuisance alarms, and an audit
// log of who changed what.
//
// State is cached in memory (acks and note counts are small and bounded by
// episode retention) and written through to the database when one is
// configured, so it survives restarts.
package alarm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/williamtatendajose/prediction/iot-hub/internal/anomaly"
	"github.com/williamtatendajose/prediction/iot-hub/internal/tsdb"
)

var (
	ErrInvalid  = errors.New("invalid")
	ErrNotFound = errors.New("not found")
)

// MaxShelve bounds shelving: a forgotten shelf must not silence an alarm
// indefinitely.
const MaxShelve = 7 * 24 * time.Hour

type Manager struct {
	db  tsdb.AlarmStore // nil = memory only
	now func() time.Time

	mu      sync.RWMutex
	acks    map[string]anomaly.Ack
	notes   map[string][]anomaly.Note // memory-only mode keeps notes here
	counts  map[string]int
	shelves map[string]anomaly.Shelf
	audit   []anomaly.AuditEntry // ring, newest last (memory mode)
	// OnChange is told about acks and shelf changes (live UI updates).
	OnChange func(kind string)
}

func New(db tsdb.AlarmStore) *Manager {
	return &Manager{db: db, now: time.Now, acks: map[string]anomaly.Ack{}, notes: map[string][]anomaly.Note{},
		counts: map[string]int{}, shelves: map[string]anomaly.Shelf{}}
}

// Load reads persisted state (call once at startup).
func (m *Manager) Load(ctx context.Context) error {
	if m.db == nil {
		return nil
	}
	acks, err := m.db.Acks(ctx)
	if err != nil {
		return err
	}
	counts, err := m.db.NoteCounts(ctx)
	if err != nil {
		return err
	}
	shelves, err := m.db.Shelves(ctx)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.acks, m.counts = acks, counts
	for _, s := range shelves {
		m.shelves[s.Key] = s
	}
	return nil
}

func (m *Manager) changed(kind string) {
	if m.OnChange != nil {
		m.OnChange(kind)
	}
}

var verdicts = map[string]bool{"": true, anomaly.VerdictConfirmed: true, anomaly.VerdictFalseAlarm: true, anomaly.VerdictExpected: true}

// Acknowledge records who acknowledged an episode and their verdict.
// Re-acknowledging updates the verdict (e.g. once the cause is known).
func (m *Manager) Acknowledge(ctx context.Context, e anomaly.Event, actor, verdict, note string) (anomaly.Ack, error) {
	if !verdicts[verdict] {
		return anomaly.Ack{}, fmt.Errorf("%w: verdict must be confirmed, false_alarm or expected", ErrInvalid)
	}
	if len(note) > 1000 {
		return anomaly.Ack{}, fmt.Errorf("%w: note longer than 1000 characters", ErrInvalid)
	}
	a := anomaly.Ack{By: actor, At: m.now().UnixMilli(), Verdict: verdict, Note: strings.TrimSpace(note)}
	if m.db != nil {
		if err := m.db.SaveAck(ctx, e.ID, a); err != nil {
			return a, err
		}
	}
	m.mu.Lock()
	m.acks[e.ID] = a
	m.mu.Unlock()
	detail := verdict
	if a.Note != "" {
		detail += ": " + a.Note
	}
	m.Audit(ctx, actor, "alarm.ack", e.ID, detail)
	m.changed("ack")
	return a, nil
}

func (m *Manager) AddNote(ctx context.Context, eventID, actor, text string) (anomaly.Note, error) {
	text = strings.TrimSpace(text)
	if text == "" || len(text) > 2000 {
		return anomaly.Note{}, fmt.Errorf("%w: note must be 1-2000 characters", ErrInvalid)
	}
	n := anomaly.Note{EventID: eventID, TS: m.now().UnixMilli(), By: actor, Text: text}
	if m.db != nil {
		if err := m.db.SaveNote(ctx, n); err != nil {
			return n, err
		}
	}
	m.mu.Lock()
	if m.db == nil {
		m.notes[eventID] = append(m.notes[eventID], n)
	}
	m.counts[eventID]++
	m.mu.Unlock()
	m.Audit(ctx, actor, "alarm.note", eventID, text)
	m.changed("note")
	return n, nil
}

func (m *Manager) Notes(ctx context.Context, eventID string) ([]anomaly.Note, error) {
	if m.db != nil {
		return m.db.Notes(ctx, eventID)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]anomaly.Note{}, m.notes[eventID]...), nil
}

// shelfKey uses ':' because ids cannot contain it (and '/' would put "//"
// into URLs, which routers rewrite).
func shelfKey(sensor, field, kind string) string { return sensor + ":" + field + ":" + kind }

// Shelve suppresses matching episodes for d (max MaxShelve). A reason is
// required: a shelf nobody can explain is how real alarms get missed.
func (m *Manager) Shelve(ctx context.Context, sensor, field, kind string, d time.Duration, actor, reason string) (anomaly.Shelf, error) {
	reason = strings.TrimSpace(reason)
	switch {
	case sensor == "":
		return anomaly.Shelf{}, fmt.Errorf("%w: sensor is required", ErrInvalid)
	case d <= 0 || d > MaxShelve:
		return anomaly.Shelf{}, fmt.Errorf("%w: duration must be between 1m and %s", ErrInvalid, MaxShelve)
	case reason == "":
		return anomaly.Shelf{}, fmt.Errorf("%w: a reason is required", ErrInvalid)
	case kind != "" && kind != "range" && kind != "spike" && kind != "stale":
		return anomaly.Shelf{}, fmt.Errorf("%w: kind must be range, spike or stale", ErrInvalid)
	}
	now := m.now()
	s := anomaly.Shelf{Key: shelfKey(sensor, field, kind), Sensor: sensor, Field: field, Kind: kind,
		Until: now.Add(d).UnixMilli(), By: actor, Reason: reason, Created: now.UnixMilli()}
	if m.db != nil {
		if err := m.db.SaveShelf(ctx, s); err != nil {
			return s, err
		}
	}
	m.mu.Lock()
	m.shelves[s.Key] = s
	m.mu.Unlock()
	m.Audit(ctx, actor, "alarm.shelve", s.Key, fmt.Sprintf("for %s: %s", d.Round(time.Minute), reason))
	m.changed("shelf")
	return s, nil
}

func (m *Manager) Unshelve(ctx context.Context, key, actor string) error {
	m.mu.Lock()
	_, ok := m.shelves[key]
	delete(m.shelves, key)
	m.mu.Unlock()
	if !ok {
		return ErrNotFound
	}
	if m.db != nil {
		if err := m.db.DeleteShelf(ctx, key); err != nil {
			return err
		}
	}
	m.Audit(ctx, actor, "alarm.unshelve", key, "")
	m.changed("shelf")
	return nil
}

// Shelves returns current (unexpired) shelves.
func (m *Manager) Shelves() []anomaly.Shelf {
	now := m.now().UnixMilli()
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []anomaly.Shelf{}
	for _, s := range m.shelves {
		if s.Until > now {
			out = append(out, s)
		}
	}
	return out
}

func (m *Manager) shelvedLocked(e anomaly.Event, now int64) bool {
	for _, k := range []string{shelfKey(e.Sensor, e.Field, e.Kind), shelfKey(e.Sensor, e.Field, ""),
		shelfKey(e.Sensor, "", e.Kind), shelfKey(e.Sensor, "", "")} {
		if s, ok := m.shelves[k]; ok && s.Until > now {
			return true
		}
	}
	return false
}

// Decorate fills in ack, shelved and note count on events.
func (m *Manager) Decorate(evs []anomaly.Event) []anomaly.Event {
	now := m.now().UnixMilli()
	m.mu.RLock()
	defer m.mu.RUnlock()
	for i := range evs {
		if a, ok := m.acks[evs[i].ID]; ok {
			a := a
			evs[i].Ack = &a
		}
		evs[i].Shelved = m.shelvedLocked(evs[i], now)
		evs[i].Notes = m.counts[evs[i].ID]
	}
	return evs
}

// Audit appends to the audit log. Failures to persist are not fatal to the
// action itself but are kept in memory.
func (m *Manager) Audit(ctx context.Context, actor, action, target, detail string) {
	if len(detail) > 500 {
		detail = detail[:500]
	}
	a := anomaly.AuditEntry{TS: m.now().UnixMilli(), Actor: actor, Action: action, Target: target, Detail: detail}
	if m.db != nil {
		if err := m.db.SaveAudit(ctx, a); err == nil {
			return
		}
	}
	m.mu.Lock()
	m.audit = append(m.audit, a)
	if len(m.audit) > 2000 {
		m.audit = m.audit[len(m.audit)-2000:]
	}
	m.mu.Unlock()
}

func (m *Manager) AuditLog(ctx context.Context, from int64, limit int) ([]anomaly.AuditEntry, error) {
	if m.db != nil {
		return m.db.Audit(ctx, from, limit)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []anomaly.AuditEntry{}
	for i := len(m.audit) - 1; i >= 0 && len(out) < limit; i-- {
		if m.audit[i].TS >= from {
			out = append(out, m.audit[i])
		}
	}
	return out, nil
}
