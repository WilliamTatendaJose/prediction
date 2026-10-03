package twin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// A batch sends one command to many devices. It runs in the background: a
// direct method may wait up to five minutes for each device, so the caller
// gets the batch at once and polls it. Each device's run is also kept in
// that device's history, tagged with the batch id.

const (
	MaxBatchDevices = 1000
	batchWorkers    = 16 // devices in flight at once
	keepBatches     = 20
)

type BatchResult struct {
	Device string `json:"device"`
	Status string `json:"status"` // pending, then a Run status; or skipped
	Code   int    `json:"code,omitempty"`
	Error  string `json:"error,omitempty"`
	Run    string `json:"run,omitempty"`
	// A message's id: its status is read from the queue, so it moves on
	// from queued to delivered, completed …
	Message  string `json:"messageId,omitempty"`
	Attempts int    `json:"attempts,omitempty"` // tries so far
}

type Batch struct {
	ID       string         `json:"id"`
	Command  string         `json:"command"`
	Label    string         `json:"label,omitempty"`
	Kind     string         `json:"kind"`
	By       string         `json:"by"`
	At       int64          `json:"at"`
	Params   map[string]any `json:"params,omitempty"`
	Total    int            `json:"total"`
	Done     int            `json:"done"`
	Finished int64          `json:"finished,omitempty"`
	// Interrupted: the hub stopped while it ran; devices that had not
	// answered are marked interrupted.
	Interrupted bool `json:"interrupted,omitempty"`
	// Resends: RetryOf is the batch this one resent to the failed devices
	// of; Retries are the batches that resent this one.
	RetryOf string   `json:"retryOf,omitempty"`
	Retries []string `json:"retries,omitempty"`
	// Automatic retries (methods only): Retry is the policy, Round the
	// attempt round now or last run, NextRetry when the next round starts.
	Retry     *RetryPolicy   `json:"retry,omitempty"`
	Round     int            `json:"round,omitempty"`
	NextRetry int64          `json:"nextRetry,omitempty"`
	Cancelled bool           `json:"cancelled,omitempty"`
	running   bool           // a round is in flight
	Counts    map[string]int `json:"counts"`
	Results   []BatchResult  `json:"results"`
}

// batchLocked copies b with messages' current statuses (s.mu held).
func (s *Service) batchLocked(b *Batch) Batch {
	c := *b
	c.Results = append([]BatchResult(nil), b.Results...)
	c.Retries = append([]string(nil), b.Retries...)
	c.Counts = map[string]int{}
	for i, r := range c.Results {
		if r.Message != "" {
			c.Results[i].Status = "untracked" // dropped from the queue's history
			if d := s.devs[r.Device]; d != nil {
				for _, m := range d.Messages {
					if m.ID == r.Message {
						c.Results[i].Status = m.Status
					}
				}
			}
		}
		if st := c.Results[i].Status; st != "pending" {
			c.Counts[st]++
		}
	}
	c.Done = 0
	for _, r := range c.Results {
		if r.Status != "pending" && r.Status != "retrying" {
			c.Done++
		}
	}
	return c
}

// RunBatch starts command name on devices for user "by". skip lists devices
// the caller asked for but may not command, with the reason; they are
// reported, not run. Parameters are checked before anything is sent.
// retry: nil uses the command's default policy; Attempts 0 turns retries off.
func (s *Service) RunBatch(name, by string, devices []string, skip map[string]string, params map[string]any, retry *RetryPolicy) (Batch, error) {
	return s.runBatch(name, by, devices, skip, params, "", retry)
}

// RetryPolicy: automatic retries of a batch's devices that failed in a way
// a later attempt may fix (AutoRetry).
//
// The wait before retry n (n = 1, 2, …) is Every × Backoff^(n-1), capped at
// MaxEvery: every 1m with backoff 2 waits 1m, 2m, 4m, 8m …
type RetryPolicy struct {
	Attempts int     `json:"attempts"`           // extra attempts after the first: 1-10 (0 = none)
	Every    string  `json:"every"`              // first wait: 10s to 24h
	Backoff  float64 `json:"backoff,omitempty"`  // multiplier per retry: 1-10; 0 or 1 = fixed interval
	MaxEvery string  `json:"maxEvery,omitempty"` // longest single wait (default 24h)
}

const maxWait = 24 * time.Hour

func (p *RetryPolicy) validate() error {
	if p == nil || p.Attempts == 0 {
		return nil
	}
	if p.Attempts < 0 || p.Attempts > 10 {
		return invalid("retry attempts: 1 to 10 (0 for none)")
	}
	d, err := time.ParseDuration(p.Every)
	if err != nil || d < 10*time.Second || d > maxWait {
		return invalid("retry every: 10s to 24h")
	}
	if p.Backoff != 0 && (p.Backoff < 1 || p.Backoff > 10) {
		return invalid("retry backoff: a multiplier from 1 (fixed) to 10")
	}
	if p.MaxEvery != "" {
		m, err := time.ParseDuration(p.MaxEvery)
		if err != nil || m < d || m > maxWait {
			return invalid("retry maxEvery: from the first wait up to 24h")
		}
	}
	return nil
}

// Wait is how long to wait before retry n (1-based).
func (p *RetryPolicy) Wait(n int) time.Duration {
	d, _ := time.ParseDuration(p.Every)
	limit := maxWait
	if p.MaxEvery != "" {
		limit, _ = time.ParseDuration(p.MaxEvery)
	}
	w := float64(d)
	if p.Backoff > 1 {
		w *= math.Pow(p.Backoff, float64(n-1)) // +Inf for huge n: capped below
	}
	if w > float64(limit) || math.IsInf(w, 0) {
		return limit
	}
	return time.Duration(w)
}

// AutoRetry: failures a later attempt may fix by itself. A device that
// refused the command is not retried automatically (Resend is still there).
func AutoRetry(status string) bool {
	switch status {
	case "offline", "timeout", "error", "interrupted":
		return true
	}
	return false
}

// Retryable: a device whose result is a definite failure. Successes,
// work still in flight, skips (the command can't run there) and messages
// whose fate is unknown are not resent.
func Retryable(status string) bool {
	switch status {
	case "offline", "timeout", "failed", "error", "interrupted", "cancelled", "rejected", "deadlettered", "expired":
		return true
	}
	return false
}

// Failed returns batch id (current statuses) and its devices to resend.
func (s *Service) Failed(id string) (Batch, []string, error) {
	b, err := s.GetBatch(id)
	if err != nil {
		return b, nil, err
	}
	var out []string
	for _, r := range b.Results {
		if Retryable(r.Status) {
			out = append(out, r.Device)
		}
	}
	return b, out, nil
}

// Resend starts a new batch of the same command and parameters on devices
// (the caller has picked the failed ones it may command), linked both ways.
func (s *Service) Resend(of Batch, by string, devices []string, skip map[string]string) (Batch, error) {
	return s.runBatch(of.Command, by, devices, skip, of.Params, of.ID, of.Retry)
}

func (s *Service) runBatch(name, by string, devices []string, skip map[string]string, params map[string]any, retryOf string, retry *RetryPolicy) (Batch, error) {
	c, err := s.command(name)
	if err != nil {
		return Batch{}, err
	}
	if _, err := c.payload(params); err != nil {
		return Batch{}, err
	}
	if retry == nil {
		retry = c.Retry
	}
	if err := retry.validate(); err != nil {
		return Batch{}, err
	}
	if retry != nil && retry.Attempts == 0 {
		retry = nil
	}
	if retry != nil && c.Kind != "method" {
		return Batch{}, invalid("automatic retries are for direct methods; messages are redelivered by the queue")
	}
	if len(devices)+len(skip) > MaxBatchDevices {
		return Batch{}, invalid("at most %d devices per batch", MaxBatchDevices)
	}
	b := &Batch{ID: newID(), Command: c.Name, Label: c.Label, Kind: c.Kind, By: by, At: s.now().UnixMilli(), Params: params,
		Counts: map[string]int{}, RetryOf: retryOf, Retry: retry, Round: 1, running: true}
	var run []int // indexes into Results to send
	seen := map[string]bool{}
	for _, d := range devices {
		if seen[d] {
			continue
		}
		seen[d] = true
		switch {
		case s.Exists != nil && !s.Exists(d):
			b.Results = append(b.Results, BatchResult{Device: d, Status: "skipped", Error: "no such device"})
		case !c.Applies(d):
			b.Results = append(b.Results, BatchResult{Device: d, Status: "skipped", Error: "command not offered on this device"})
		default:
			run = append(run, len(b.Results))
			b.Results = append(b.Results, BatchResult{Device: d, Status: "pending"})
		}
	}
	for d, why := range skip {
		if !seen[d] {
			b.Results = append(b.Results, BatchResult{Device: d, Status: "skipped", Error: why})
		}
	}
	if len(run) == 0 {
		return Batch{}, invalid("none of the chosen devices can run %s", name)
	}
	b.Total = len(b.Results)
	s.mu.Lock()
	for _, o := range s.batches {
		if o.ID == retryOf {
			o.Retries = append(o.Retries, b.ID)
		}
	}
	s.batches = append(s.batches, b)
	if len(s.batches) > keepBatches {
		s.batches = s.batches[len(s.batches)-keepBatches:]
	}
	out := s.batchLocked(b)
	s.mu.Unlock()
	s.markDirty()

	go s.round(b, c, run)
	return out, nil
}

// round sends b's command to the devices at idxs, then decides what comes
// next: another round later, or done.
func (s *Service) round(b *Batch, c Command, idxs []int) {
	sem := make(chan struct{}, batchWorkers)
	var wg sync.WaitGroup
	for _, i := range idxs {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer func() { <-sem; wg.Done() }()
			s.mu.Lock()
			prev := b.Results[i]
			s.mu.Unlock()
			r, err := s.runCommand(context.Background(), prev.Device, c, b.By, b.Params, b.ID)
			res := BatchResult{Device: prev.Device, Status: r.Status, Code: r.Code, Error: r.Error, Run: r.ID, Message: r.Message}
			if err != nil {
				res = BatchResult{Device: prev.Device, Status: "error", Error: err.Error()}
			}
			res.Attempts = prev.Attempts + 1
			s.mu.Lock()
			b.Results[i] = res
			s.mu.Unlock()
			s.markDirty()
		}(i)
	}
	wg.Wait()
	s.mu.Lock()
	b.running = false
	s.afterRoundLocked(b, s.now().UnixMilli())
	s.mu.Unlock()
	s.markDirty()
}

// afterRoundLocked: devices that may still succeed wait for the next round;
// otherwise the batch is finished.
func (s *Service) afterRoundLocked(b *Batch, now int64) {
	again := false
	for i, r := range b.Results {
		if b.Retry == nil || b.Cancelled {
			break
		}
		if r.Status == "retrying" || AutoRetry(r.Status) && r.Attempts <= b.Retry.Attempts {
			if r.Status != "retrying" {
				b.Results[i].Error = fmt.Sprintf("attempt %d: %s", r.Attempts, strings.TrimSpace(r.Status+" "+r.Error))
				b.Results[i].Status = "retrying"
			}
			again = true
		}
	}
	if again {
		if b.NextRetry == 0 {
			// The round that just ran was retry Round-1; the next is Round.
			b.NextRetry = now + b.Retry.Wait(b.Round).Milliseconds()
		}
		return
	}
	for i, r := range b.Results {
		if r.Status == "retrying" { // cancelled while waiting
			b.Results[i].Status = "cancelled"
		}
	}
	b.NextRetry, b.Finished = 0, now
}

// tickBatches starts the rounds that are due (from Tick).
func (s *Service) tickBatches(now int64) {
	type due struct {
		b    *Batch
		idxs []int
	}
	var start []due
	s.mu.Lock()
	for _, b := range s.batches {
		if b.Finished != 0 || b.running || b.NextRetry == 0 || b.NextRetry > now {
			continue
		}
		var idxs []int
		for i, r := range b.Results {
			if r.Status == "retrying" {
				b.Results[i].Status = "pending"
				idxs = append(idxs, i)
			}
		}
		b.NextRetry = 0
		if len(idxs) == 0 {
			s.afterRoundLocked(b, now)
			continue
		}
		b.running = true
		b.Round++
		start = append(start, due{b, idxs})
	}
	s.mu.Unlock()
	for _, d := range start {
		c, err := s.command(d.b.Command)
		if err != nil { // the command was deleted meanwhile
			s.mu.Lock()
			for _, i := range d.idxs {
				d.b.Results[i].Status, d.b.Results[i].Error = "error", "command "+d.b.Command+" no longer exists"
			}
			d.b.running = false
			d.b.NextRetry, d.b.Finished = 0, now
			s.mu.Unlock()
			continue
		}
		go s.round(d.b, c, d.idxs)
	}
	if len(start) > 0 {
		s.markDirty()
	}
}

// CancelBatch stops a batch's pending retries. Calls already in flight
// finish; nothing new is sent.
func (s *Service) CancelBatch(id string) (Batch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range s.batches {
		if b.ID != id {
			continue
		}
		if b.Finished != 0 {
			return s.batchLocked(b), invalid("the batch has already finished")
		}
		b.Cancelled = true
		if !b.running {
			s.afterRoundLocked(b, s.now().UnixMilli())
		}
		s.markDirty()
		return s.batchLocked(b), nil
	}
	return Batch{}, fmt.Errorf("%w: batch %s", ErrNotFound, id)
}

// GetBatch returns a batch's current state.
func (s *Service) GetBatch(id string) (Batch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range s.batches {
		if b.ID == id {
			return s.batchLocked(b), nil
		}
	}
	return Batch{}, fmt.Errorf("%w: batch %s (only the last %d are kept)", ErrNotFound, id, keepBatches)
}

// Batches lists recent batches, newest first, without per-device results.
func (s *Service) Batches() []Batch {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Batch, 0, len(s.batches))
	for i := len(s.batches) - 1; i >= 0; i-- {
		c := s.batchLocked(s.batches[i])
		c.Results = nil
		out = append(out, c)
	}
	return out
}

func (s *Service) batchPath() string { return filepath.Join(filepath.Dir(s.path), "batches.json") }

// loadBatches reads the saved batches. One the hub stopped in the middle of
// is finished now: devices still waiting are marked interrupted (their
// command may or may not have reached them; their history says what was
// recorded).
func (s *Service) loadBatches() error {
	if s.path == "" {
		return nil
	}
	b, err := os.ReadFile(s.batchPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var list []*Batch
	if err := json.Unmarshal(b, &list); err != nil {
		return fmt.Errorf("parse %s: %w", s.batchPath(), err)
	}
	now := s.now().UnixMilli()
	for _, x := range list {
		if x.Finished != 0 {
			continue
		}
		for i, r := range x.Results {
			if r.Status == "pending" {
				x.Results[i].Status, x.Results[i].Error = "interrupted", "the hub stopped before this device answered"
				x.Results[i].Attempts++
				x.Interrupted = true
			}
		}
		// With retries left, it carries on (the next round waits its
		// interval); otherwise it is finished now.
		s.afterRoundLocked(x, now)
	}
	if len(list) > keepBatches {
		list = list[len(list)-keepBatches:]
	}
	s.mu.Lock()
	s.batches = list
	s.mu.Unlock()
	return nil
}
