package twin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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
	Message string `json:"messageId,omitempty"`
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
	Interrupted bool           `json:"interrupted,omitempty"`
	Counts      map[string]int `json:"counts"`
	Results     []BatchResult  `json:"results"`
}

// batchLocked copies b with messages' current statuses (s.mu held).
func (s *Service) batchLocked(b *Batch) Batch {
	c := *b
	c.Results = append([]BatchResult(nil), b.Results...)
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
		if c.Results[i].Status != "pending" {
			c.Counts[c.Results[i].Status]++
		}
	}
	return c
}

// RunBatch starts command name on devices for user "by". skip lists devices
// the caller asked for but may not command, with the reason; they are
// reported, not run. Parameters are checked before anything is sent.
func (s *Service) RunBatch(name, by string, devices []string, skip map[string]string, params map[string]any) (Batch, error) {
	c, err := s.command(name)
	if err != nil {
		return Batch{}, err
	}
	if _, err := c.payload(params); err != nil {
		return Batch{}, err
	}
	if len(devices)+len(skip) > MaxBatchDevices {
		return Batch{}, invalid("at most %d devices per batch", MaxBatchDevices)
	}
	b := &Batch{ID: newID(), Command: c.Name, Label: c.Label, Kind: c.Kind, By: by, At: s.now().UnixMilli(), Params: params,
		Counts: map[string]int{}}
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
	for _, r := range b.Results {
		if r.Status == "skipped" {
			b.Done++
		}
	}
	s.mu.Lock()
	s.batches = append(s.batches, b)
	if len(s.batches) > keepBatches {
		s.batches = s.batches[len(s.batches)-keepBatches:]
	}
	out := s.batchLocked(b)
	s.mu.Unlock()
	s.markDirty()

	go func() {
		sem := make(chan struct{}, batchWorkers)
		var wg sync.WaitGroup
		for _, i := range run {
			wg.Add(1)
			sem <- struct{}{}
			go func(i int) {
				defer func() { <-sem; wg.Done() }()
				dev := b.Results[i].Device // only this goroutine writes entry i
				r, err := s.runCommand(context.Background(), dev, c, by, params, b.ID)
				res := BatchResult{Device: dev, Status: r.Status, Code: r.Code, Error: r.Error, Run: r.ID, Message: r.Message}
				if err != nil {
					res = BatchResult{Device: dev, Status: "error", Error: err.Error()}
				}
				s.mu.Lock()
				b.Results[i] = res
				b.Done++
				s.mu.Unlock()
				s.markDirty()
			}(i)
		}
		wg.Wait()
		s.mu.Lock()
		b.Finished = s.now().UnixMilli()
		s.mu.Unlock()
		s.markDirty()
	}()
	return out, nil
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
				x.Done++
			}
		}
		x.Finished, x.Interrupted = now, true
	}
	if len(list) > keepBatches {
		list = list[len(list)-keepBatches:]
	}
	s.mu.Lock()
	s.batches = list
	s.mu.Unlock()
	return nil
}
