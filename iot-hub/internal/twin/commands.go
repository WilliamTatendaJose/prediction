package twin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Commands are named actions defined once per tenant ("reboot", "close
// valve") and offered on every device whose id matches. Each is a direct
// method or a cloud-to-device message with a fixed payload plus typed
// parameters the operator fills in. Admins define them; operators may run
// those marked for operators, so the catalog is what bounds what they can
// send. Every run is kept in the device's history.

const (
	MaxCommands = 100
	MaxParams   = 10
	keepRuns    = 50
)

var cmdNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,39}$`)

type Param struct {
	Name     string   `json:"name"`
	Label    string   `json:"label,omitempty"`
	Type     string   `json:"type"` // number | text | bool | choice
	Choices  []string `json:"choices,omitempty"`
	Min      *float64 `json:"min,omitempty"`
	Max      *float64 `json:"max,omitempty"`
	Required bool     `json:"required,omitempty"`
	Default  any      `json:"default,omitempty"`
}

type Command struct {
	Name    string          `json:"name"`
	Label   string          `json:"label,omitempty"`
	Devices []string        `json:"devices,omitempty"` // device id patterns; empty = every device
	Kind    string          `json:"kind"`              // method | message
	Method  string          `json:"method,omitempty"`  // method name; default Name
	Payload json.RawMessage `json:"payload,omitempty"` // fixed part; parameters are added as keys
	Params  []Param         `json:"params,omitempty"`
	Timeout string          `json:"timeout,omitempty"` // method: how long to wait (default 30s)
	TTL     string          `json:"ttl,omitempty"`     // message: how long it may wait to be delivered (default 1h)
	Confirm bool            `json:"confirm,omitempty"` // ask "are you sure?" first
	Role    string          `json:"role,omitempty"`    // who may run it: operator (default) | admin
}

// Run is one execution, kept in the device's history.
type Run struct {
	ID      string          `json:"id"`
	Command string          `json:"command"`
	Label   string          `json:"label,omitempty"`
	Kind    string          `json:"kind"`
	By      string          `json:"by"`
	At      int64           `json:"at"`
	Payload json.RawMessage `json:"payload,omitempty"`
	// ok / failed (the device answered with status ≥ 400) / offline /
	// timeout for methods; queued, then the message's own status.
	Status  string          `json:"status"`
	Code    int             `json:"code,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   string          `json:"error,omitempty"`
	Message string          `json:"messageId,omitempty"`
	TookMs  int64           `json:"tookMs,omitempty"`
	Device  string          `json:"device,omitempty"` // in cross-device lists
}

func (c Command) MethodName() string {
	if c.Method != "" {
		return c.Method
	}
	return c.Name
}

// Applies reports whether the command is offered on device id.
func (c Command) Applies(id string) bool {
	if len(c.Devices) == 0 {
		return true
	}
	for _, p := range c.Devices {
		if ok, _ := path.Match(p, id); ok {
			return true
		}
	}
	return false
}

func (c *Command) validate() error {
	c.Name = strings.TrimSpace(c.Name)
	if !cmdNameRe.MatchString(c.Name) {
		return invalid("command name: 1-40 of a-z, 0-9, _ and -")
	}
	if len(c.Label) > 80 {
		return invalid("label: at most 80 characters")
	}
	for _, p := range c.Devices {
		if _, err := path.Match(p, ""); err != nil || p == "" {
			return invalid("device pattern %q", p)
		}
	}
	switch c.Kind {
	case "method":
		if !methodRe.MatchString(c.MethodName()) {
			return invalid("method name: letters, digits, _ . - (up to 64)")
		}
		if c.Timeout != "" {
			d, err := time.ParseDuration(c.Timeout)
			if err != nil || d < time.Second || d > MaxMethodWait {
				return invalid("timeout: 1s to 5m")
			}
		}
	case "message":
		if c.TTL != "" {
			d, err := time.ParseDuration(c.TTL)
			if err != nil || d <= 0 || d > MaxTTL {
				return invalid("ttl: up to 48h")
			}
		}
	default:
		return invalid("kind: method or message")
	}
	if c.Role == "" {
		c.Role = "operator"
	}
	if c.Role != "operator" && c.Role != "admin" {
		return invalid("role: operator or admin")
	}
	if len(c.Payload) > 0 {
		if !json.Valid(c.Payload) {
			return invalid("payload is not valid JSON")
		}
		if len(c.Payload) > MaxMessageBody {
			return invalid("payload over %d KiB", MaxMessageBody>>10)
		}
	}
	if len(c.Params) > MaxParams {
		return invalid("at most %d parameters", MaxParams)
	}
	if len(c.Params) > 0 && len(c.Payload) > 0 {
		var obj map[string]any
		if json.Unmarshal(c.Payload, &obj) != nil {
			return invalid("with parameters, the payload must be a JSON object")
		}
	}
	seen := map[string]bool{}
	for i := range c.Params {
		p := &c.Params[i]
		if !keyRe.MatchString(p.Name) || seen[p.Name] {
			return invalid("parameter name %q: letters, digits, _ and -, once each", p.Name)
		}
		seen[p.Name] = true
		switch p.Type {
		case "number", "text", "bool":
		case "choice":
			if len(p.Choices) == 0 {
				return invalid("parameter %s: give the choices", p.Name)
			}
		default:
			return invalid("parameter %s: type number, text, bool or choice", p.Name)
		}
		if p.Default != nil {
			if _, err := p.value(p.Default); err != nil {
				return invalid("parameter %s default: %v", p.Name, err)
			}
		}
	}
	return nil
}

// value checks one parameter value (as JSON decoded it).
func (p Param) value(v any) (any, error) {
	switch p.Type {
	case "number":
		f, ok := v.(float64)
		if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, errors.New("want a number")
		}
		if p.Min != nil && f < *p.Min || p.Max != nil && f > *p.Max {
			return nil, fmt.Errorf("out of range %s", rangeText(p.Min, p.Max))
		}
		return f, nil
	case "bool":
		b, ok := v.(bool)
		if !ok {
			return nil, errors.New("want true or false")
		}
		return b, nil
	case "choice":
		s, _ := v.(string)
		for _, c := range p.Choices {
			if s == c {
				return s, nil
			}
		}
		return nil, fmt.Errorf("want one of %s", strings.Join(p.Choices, ", "))
	default:
		s, ok := v.(string)
		if !ok || len(s) > 1000 {
			return nil, errors.New("want text (up to 1000 characters)")
		}
		return s, nil
	}
}

func rangeText(min, max *float64) string {
	f := func(x *float64) string {
		if x == nil {
			return "…"
		}
		return fmt.Sprint(*x)
	}
	return f(min) + " to " + f(max)
}

// payload builds what is sent: the fixed payload plus parameters as keys.
func (c Command) payload(params map[string]any) (json.RawMessage, error) {
	for k := range params {
		found := false
		for _, p := range c.Params {
			found = found || p.Name == k
		}
		if !found {
			return nil, invalid("unknown parameter %s", k)
		}
	}
	if len(c.Params) == 0 {
		return c.Payload, nil
	}
	obj := map[string]any{}
	if len(c.Payload) > 0 {
		if err := json.Unmarshal(c.Payload, &obj); err != nil {
			return nil, err
		}
	}
	for _, p := range c.Params {
		v, given := params[p.Name]
		if !given || v == nil || v == "" {
			if p.Default != nil {
				v, given = p.Default, true
			} else if p.Required {
				return nil, invalid("%s is required", label(p))
			} else {
				continue
			}
		}
		x, err := p.value(v)
		if err != nil {
			return nil, invalid("%s: %v", label(p), err)
		}
		obj[p.Name] = x
	}
	return json.Marshal(obj)
}

func label(p Param) string {
	if p.Label != "" {
		return p.Label
	}
	return p.Name
}

// ---- catalog ------------------------------------------------------------------

func (s *Service) cmdPath() string {
	if s.path == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(s.path), "commands.json")
}

// Commands returns the catalog, by name.
func (s *Service) Commands() []Command {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Command, 0, len(s.cmds))
	for _, c := range s.cmds {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// For lists the commands offered on device id.
func (s *Service) For(id string) []Command {
	var out []Command
	for _, c := range s.Commands() {
		if c.Applies(id) {
			out = append(out, c)
		}
	}
	return out
}

func (s *Service) SetCommand(c Command) (Command, error) {
	if err := c.validate(); err != nil {
		return c, err
	}
	s.mu.Lock()
	if _, ok := s.cmds[c.Name]; !ok && len(s.cmds) >= MaxCommands {
		s.mu.Unlock()
		return c, invalid("at most %d commands", MaxCommands)
	}
	if s.cmds == nil {
		s.cmds = map[string]Command{}
	}
	s.cmds[c.Name] = c
	s.mu.Unlock()
	return c, s.saveCommands()
}

func (s *Service) DeleteCommand(name string) error {
	s.mu.Lock()
	_, ok := s.cmds[name]
	delete(s.cmds, name)
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: command %s", ErrNotFound, name)
	}
	return s.saveCommands()
}

func (s *Service) loadCommands() error {
	p := s.cmdPath()
	if p == "" {
		return nil
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var list []Command
	if err := json.Unmarshal(b, &list); err != nil {
		return fmt.Errorf("parse %s: %w", p, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cmds = map[string]Command{}
	for _, c := range list {
		s.cmds[c.Name] = c
	}
	return nil
}

func (s *Service) saveCommands() error {
	p := s.cmdPath()
	if p == "" {
		return nil
	}
	b, err := json.MarshalIndent(s.Commands(), "", "  ")
	if err != nil {
		return err
	}
	s.cmdSave.Lock() // serialise writers so an older list never lands last
	defer s.cmdSave.Unlock()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// ---- running ------------------------------------------------------------------

// ErrNotOffered: the command doesn't apply to this device.
var ErrNotOffered = errors.New("command is not offered on this device")

// RunCommand executes a command on a device for user "by" and records it.
// A device that is offline or doesn't answer is a recorded outcome, not an
// error; errors are for requests that can't run (unknown, bad parameters).
func (s *Service) RunCommand(ctx context.Context, id, name, by string, params map[string]any) (Run, error) {
	if err := s.check(id); err != nil {
		return Run{}, err
	}
	s.mu.Lock()
	c, ok := s.cmds[name]
	s.mu.Unlock()
	if !ok {
		return Run{}, fmt.Errorf("%w: command %s", ErrNotFound, name)
	}
	if !c.Applies(id) {
		return Run{}, ErrNotOffered
	}
	body, err := c.payload(params)
	if err != nil {
		return Run{}, err
	}
	run := Run{ID: newID(), Command: c.Name, Label: c.Label, Kind: c.Kind, By: by, At: s.now().UnixMilli(), Payload: body}
	start := time.Now()
	if c.Kind == "method" {
		timeout := 30 * time.Second
		if c.Timeout != "" {
			timeout, _ = time.ParseDuration(c.Timeout)
		}
		res, err := s.Invoke(ctx, id, c.MethodName(), body, timeout)
		run.TookMs = time.Since(start).Milliseconds()
		switch {
		case errors.Is(err, ErrOffline):
			run.Status, run.Error = "offline", err.Error()
		case errors.Is(err, ErrTimeout):
			run.Status, run.Error = "timeout", err.Error()
		case err != nil:
			return Run{}, err
		default:
			run.Code, run.Result = res.Status, res.Payload
			run.Status = "ok"
			if res.Status >= 400 {
				run.Status = "failed"
			}
		}
	} else {
		if len(body) == 0 {
			body = json.RawMessage(`{"command":` + strconvQuote(c.Name) + `}`)
			run.Payload = body
		}
		var ttl time.Duration
		if c.TTL != "" {
			ttl, _ = time.ParseDuration(c.TTL)
		}
		m, err := s.Send(id, body, ttl)
		if err != nil {
			return Run{}, err
		}
		run.Status, run.Message = "queued", m.ID
	}
	s.mu.Lock()
	r := s.recLocked(id)
	r.Runs = append(r.Runs, run)
	if len(r.Runs) > keepRuns {
		r.Runs = append([]Run(nil), r.Runs[len(r.Runs)-keepRuns:]...)
	}
	s.mu.Unlock()
	s.markDirty()
	return run, nil
}

func strconvQuote(s string) string { b, _ := json.Marshal(s); return string(b) }

// Runs is a device's command history, newest first. A message's status is
// read from the queue, so it moves from queued to delivered, completed …
func (s *Service) Runs(id string) []Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.devs[id]
	if r == nil {
		return []Run{}
	}
	return s.runsLocked(id, r)
}

func (s *Service) runsLocked(id string, r *rec) []Run {
	out := make([]Run, 0, len(r.Runs))
	for i := len(r.Runs) - 1; i >= 0; i-- {
		run := r.Runs[i]
		run.Device = id
		if run.Message != "" {
			run.Status = "untracked" // dropped from the queue's history
			for _, m := range r.Messages {
				if m.ID == run.Message {
					run.Status = m.Status
				}
			}
		}
		out = append(out, run)
	}
	return out
}

// RecentRuns is the newest n runs across devices ids.
func (s *Service) RecentRuns(ids []string, n int) []Run {
	s.mu.Lock()
	all := []Run{}
	for _, id := range ids {
		if r := s.devs[id]; r != nil {
			all = append(all, s.runsLocked(id, r)...)
		}
	}
	s.mu.Unlock()
	sort.Slice(all, func(i, j int) bool { return all[i].At > all[j].At })
	if len(all) > n {
		all = all[:n]
	}
	return all
}

// ---- expected interval ----------------------------------------------------------

// SetExpected sets how often device id should send data (0 clears).
func (s *Service) SetExpected(id string, d time.Duration) error {
	if err := s.check(id); err != nil {
		return err
	}
	if d != 0 && (d < time.Second || d > 7*24*time.Hour) {
		return invalid("expected interval: 1s to 7 days")
	}
	s.mu.Lock()
	s.recLocked(id).ExpectedSec = int64(d / time.Second)
	s.mu.Unlock()
	s.markDirty()
	return nil
}

// Watch is what the overdue check needs about one device.
type Watch struct {
	ID          string
	LastData    int64 // ms; 0 = never sent
	ExpectedSec int64
}

// Watched lists the devices with an expected interval.
func (s *Service) Watched() []Watch {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Watch
	for id, r := range s.devs {
		if r.ExpectedSec > 0 {
			out = append(out, Watch{ID: id, LastData: r.LastData, ExpectedSec: r.ExpectedSec})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
