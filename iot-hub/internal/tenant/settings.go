package tenant

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

	"github.com/williamtatendajose/prediction/iot-hub/internal/escalate"
	"github.com/williamtatendajose/prediction/iot-hub/internal/notify"
	"github.com/williamtatendajose/prediction/iot-hub/internal/store"
)

// Settings is what a tenant admin configures: where alerts, escalations
// and shift reports go, and the plant's shifts and time zone. Targets are
// named once (their URLs hold secrets) and referenced by id elsewhere.
type Settings struct {
	Targets       []TargetSpec `json:"targets"`
	Notify        NotifyCfg    `json:"notify"`
	Escalation    EscalateCfg  `json:"escalation"`
	Reports       ReportsCfg   `json:"reports"`
	Shifts        []string     `json:"shifts"`
	TimeZone      string       `json:"timeZone,omitempty"`
	WebhookSecret string       `json:"webhookSecret,omitempty"` // signs generic webhooks
}

type TargetSpec struct {
	ID   string `json:"id"`
	Spec string `json:"spec"` // kind=URL, with secrets
}

type NotifyCfg struct {
	Targets     []string `json:"targets"`
	Kinds       []string `json:"kinds,omitempty"` // empty = all
	Resolved    bool     `json:"resolved"`
	CooldownMin int      `json:"cooldownMin,omitempty"` // default 10
	PerMinute   int      `json:"perMinute,omitempty"`   // default 20
}

type EscalateCfg struct {
	Levels    []EscLevel `json:"levels"`
	RepeatMin int        `json:"repeatMin,omitempty"` // 0 = no repeat
}

type EscLevel struct {
	AfterMin int      `json:"afterMin"`
	Targets  []string `json:"targets"`
}

type ReportsCfg struct {
	Targets []string `json:"targets"`
}

var targetIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

func invalid(f string, a ...any) error {
	return fmt.Errorf("%w: %s", store.ErrInvalid, fmt.Sprintf(f, a...))
}

// compile validates settings and parses their targets and time zone.
func (s Settings) compile() (map[string]*notify.Target, *time.Location, error) {
	ts := map[string]*notify.Target{}
	for _, t := range s.Targets {
		if !targetIDRe.MatchString(t.ID) {
			return nil, nil, invalid("target id %q: use a-z, 0-9 and '-', up to 32", t.ID)
		}
		if ts[t.ID] != nil {
			return nil, nil, invalid("target %q twice", t.ID)
		}
		p, err := notify.ParseTarget(t.Spec)
		if err != nil {
			return nil, nil, invalid("target %s: %v", t.ID, err)
		}
		ts[t.ID] = p
	}
	refs := func(where string, ids []string) error {
		for _, id := range ids {
			if ts[id] == nil {
				return invalid("%s refers to unknown target %q", where, id)
			}
		}
		return nil
	}
	if err := refs("notify", s.Notify.Targets); err != nil {
		return nil, nil, err
	}
	if err := refs("reports", s.Reports.Targets); err != nil {
		return nil, nil, err
	}
	if len(s.Escalation.Levels) > 5 {
		return nil, nil, invalid("at most 5 escalation levels")
	}
	prev := 0
	for i, l := range s.Escalation.Levels {
		if l.AfterMin <= prev || l.AfterMin > 7*24*60 {
			return nil, nil, invalid("escalation level %d: afterMin must increase and be at most 7 days", i+1)
		}
		prev = l.AfterMin
		if len(l.Targets) == 0 {
			return nil, nil, invalid("escalation level %d has no targets", i+1)
		}
		if err := refs(fmt.Sprintf("escalation level %d", i+1), l.Targets); err != nil {
			return nil, nil, err
		}
	}
	if r := s.Escalation.RepeatMin; r != 0 && (r < 5 || r > 24*60) {
		return nil, nil, invalid("escalation repeatMin must be 0 or 5..1440")
	}
	for _, k := range s.Notify.Kinds {
		if k != "range" && k != "spike" && k != "stale" && k != "rule" {
			return nil, nil, invalid("notify kind %q: range, spike, stale or rule", k)
		}
	}
	if s.Notify.CooldownMin < 0 || s.Notify.PerMinute < 0 {
		return nil, nil, invalid("cooldownMin and perMinute can't be negative")
	}
	for _, sh := range s.Shifts {
		if _, err := time.Parse("15:04", sh); err != nil {
			return nil, nil, invalid("shift start %q: want HH:MM", sh)
		}
	}
	loc := time.Local
	if s.TimeZone != "" {
		l, err := time.LoadLocation(s.TimeZone)
		if err != nil {
			return nil, nil, invalid("time zone %q: %v", s.TimeZone, err)
		}
		loc = l
	}
	return ts, loc, nil
}

func pick(ts map[string]*notify.Target, ids []string) []*notify.Target {
	out := make([]*notify.Target, 0, len(ids))
	for _, id := range ids {
		out = append(out, ts[id])
	}
	return out
}

// settingsMgr owns a tenant's settings file and applies changes live.
type settingsMgr struct {
	mu      sync.Mutex
	path    string
	cur     Settings
	targets map[string]*notify.Target
	apply   func(Settings, map[string]*notify.Target, *time.Location)
}

func (m *settingsMgr) load(seed Settings) (Settings, error) {
	if m.path != "" {
		b, err := os.ReadFile(m.path)
		switch {
		case err == nil:
			var s Settings
			if err := json.Unmarshal(b, &s); err != nil {
				return s, fmt.Errorf("parse %s: %w", m.path, err)
			}
			return s, nil
		case !errors.Is(err, os.ErrNotExist):
			return seed, err
		}
	}
	return seed, nil
}

func (m *settingsMgr) save(s Settings) error {
	if m.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.path), 0o700); err != nil {
		return err
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil { // target URLs hold secrets
		return err
	}
	return os.Rename(tmp, m.path)
}

// set validates, saves and applies; callers hold m.mu.
func (m *settingsMgr) set(s Settings) error {
	ts, loc, err := s.compile()
	if err != nil {
		return err
	}
	if err := m.save(s); err != nil {
		return err
	}
	m.cur, m.targets = s, ts
	m.apply(s, ts, loc)
	return nil
}

// TargetView is a target as shown to admins: never its secrets.
type TargetView struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Display   string `json:"display"`
	Sent      uint64 `json:"sent"`
	Failed    uint64 `json:"failed"`
	LastError string `json:"lastError,omitempty"`
}

// View is GET /api/settings.
func (m *settingsMgr) View() any {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.cur
	nz := func(x []string) []string {
		if x == nil {
			return []string{}
		}
		return x
	}
	s.Notify.Targets, s.Notify.Kinds, s.Reports.Targets, s.Shifts = nz(s.Notify.Targets), nz(s.Notify.Kinds), nz(s.Reports.Targets), nz(s.Shifts)
	if s.Escalation.Levels == nil {
		s.Escalation.Levels = []EscLevel{}
	}
	tv := make([]TargetView, 0, len(s.Targets))
	for _, t := range s.Targets {
		p := m.targets[t.ID]
		if p == nil {
			continue
		}
		tv = append(tv, TargetView{ID: t.ID, Kind: p.Kind, Display: p.Redacted(), Sent: p.Sent.Load(), Failed: p.Failed.Load(), LastError: p.LastError()})
	}
	return map[string]any{
		"targets": tv, "notify": s.Notify, "escalation": s.Escalation, "reports": s.Reports,
		"shifts": s.Shifts, "timeZone": s.TimeZone, "webhookSecretSet": s.WebhookSecret != "",
	}
}

// Update replaces everything except the targets (PUT /api/settings).
// "webhookSecret" is write-only: omitted keeps the current one.
func (m *settingsMgr) Update(raw []byte) error {
	var in struct {
		Notify        *NotifyCfg   `json:"notify"`
		Escalation    *EscalateCfg `json:"escalation"`
		Reports       *ReportsCfg  `json:"reports"`
		Shifts        *[]string    `json:"shifts"`
		TimeZone      *string      `json:"timeZone"`
		WebhookSecret *string      `json:"webhookSecret"`
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return invalid("%v", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.cur
	if in.Notify != nil {
		s.Notify = *in.Notify
	}
	if in.Escalation != nil {
		s.Escalation = *in.Escalation
	}
	if in.Reports != nil {
		s.Reports = *in.Reports
	}
	if in.Shifts != nil {
		s.Shifts = *in.Shifts
	}
	if in.TimeZone != nil {
		s.TimeZone = *in.TimeZone
	}
	if in.WebhookSecret != nil {
		s.WebhookSecret = *in.WebhookSecret
	}
	return m.set(s)
}

// AddTarget adds or replaces a target.
func (m *settingsMgr) AddTarget(id, spec string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.cur
	s.Targets = append([]TargetSpec(nil), s.Targets...)
	replaced := false
	for i := range s.Targets {
		if s.Targets[i].ID == id {
			s.Targets[i].Spec, replaced = spec, true
		}
	}
	if !replaced {
		if len(s.Targets) >= 20 {
			return invalid("at most 20 targets")
		}
		s.Targets = append(s.Targets, TargetSpec{ID: id, Spec: spec})
	}
	sort.Slice(s.Targets, func(i, j int) bool { return s.Targets[i].ID < s.Targets[j].ID })
	return m.set(s)
}

// RemoveTarget deletes a target that nothing refers to.
func (m *settingsMgr) RemoveTarget(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.cur
	var keep []TargetSpec
	for _, t := range s.Targets {
		if t.ID != id {
			keep = append(keep, t)
		}
	}
	if len(keep) == len(s.Targets) {
		return store.ErrNotFound
	}
	s.Targets = keep
	return m.set(s) // fails (400) if notify/escalation/reports still use it
}

// TestTarget sends a test message to one target.
func (m *settingsMgr) TestTarget(ctx context.Context, id string) error {
	m.mu.Lock()
	t := m.targets[id]
	secret := m.cur.WebhookSecret
	m.mu.Unlock()
	if t == nil {
		return store.ErrNotFound
	}
	return notify.New(notify.Config{Targets: []*notify.Target{t}, Secret: secret}).Test(ctx, t)
}

// escalationPolicy builds the engine policy from settings.
func escalationPolicy(s Settings, ts map[string]*notify.Target, sender *notify.Notifier) escalate.Policy {
	p := escalate.Policy{Repeat: time.Duration(s.Escalation.RepeatMin) * time.Minute, Sender: sender}
	for _, l := range s.Escalation.Levels {
		p.Levels = append(p.Levels, escalate.Level{After: time.Duration(l.AfterMin) * time.Minute, Targets: pick(ts, l.Targets)})
	}
	return p
}
