// Package escalate re-notifies about alarms nobody has acknowledged: after
// each level's delay its targets are told (a supervisor after 15 min, the
// plant manager after 60), optionally repeating until someone acts.
// Acknowledging, shelving or the alarm clearing stops it.
package escalate

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/williamtatendajose/prediction/iot-hub/internal/anomaly"
	"github.com/williamtatendajose/prediction/iot-hub/internal/notify"
)

type Level struct {
	After   time.Duration
	Targets []*notify.Target
}

type Policy struct {
	Levels []Level       // ascending After
	Repeat time.Duration // 0 = no repeat after the last level
	Sender *notify.Notifier
}

// Engine is evaluated periodically against the open episodes.
type Engine struct {
	// Active returns open episodes with their ack/shelve state.
	Active func() []anomaly.Event

	mu     sync.Mutex
	policy Policy
	state  map[string]*progress
	now    func() time.Time
	// Sent counts escalation messages (for status and tests).
	Sent []Record
}

type progress struct {
	level int // levels already sent
	last  time.Time
}

// Record is one escalation that went out.
type Record struct {
	Event string `json:"event"`
	Level int    `json:"level"`
	At    int64  `json:"at"`
}

func New(active func() []anomaly.Event) *Engine {
	return &Engine{Active: active, state: map[string]*progress{}, now: time.Now}
}

func (e *Engine) SetPolicy(p Policy) {
	sort.Slice(p.Levels, func(i, j int) bool { return p.Levels[i].After < p.Levels[j].After })
	e.mu.Lock()
	e.policy = p
	e.mu.Unlock()
}

// Tick evaluates every open episode once.
func (e *Engine) Tick(ctx context.Context) {
	e.mu.Lock()
	p := e.policy
	now := e.now()
	type send struct {
		ev    anomaly.Event
		level int
	}
	var out []send
	open := map[string]bool{}
	if len(p.Levels) > 0 && p.Sender != nil {
		for _, ev := range e.Active() {
			if ev.End != 0 || ev.Ack != nil || ev.Shelved {
				continue
			}
			open[ev.ID] = true
			st := e.state[ev.ID]
			if st == nil {
				st = &progress{}
				e.state[ev.ID] = st
			}
			age := now.Sub(time.UnixMilli(ev.Start))
			// Jump to the highest level that is due: after a restart or a
			// policy change, people get one message, not a burst.
			due := st.level
			for due < len(p.Levels) && age >= p.Levels[due].After {
				due++
			}
			switch {
			case due > st.level:
				st.level, st.last = due, now
				out = append(out, send{ev, due})
			case p.Repeat > 0 && st.level == len(p.Levels) && now.Sub(st.last) >= p.Repeat:
				st.last = now
				out = append(out, send{ev, st.level})
			}
		}
	}
	for id := range e.state {
		if !open[id] {
			delete(e.state, id) // acknowledged, shelved or cleared
		}
	}
	for _, s := range out {
		e.Sent = append(e.Sent, Record{s.ev.ID, s.level, now.UnixMilli()})
	}
	if len(e.Sent) > 200 {
		e.Sent = append([]Record(nil), e.Sent[len(e.Sent)-200:]...)
	}
	e.mu.Unlock()

	for _, s := range out {
		lv := p.Levels[s.level-1]
		p.Sender.SendTo(ctx, lv.Targets, p.Sender.Escalation(s.ev, s.level, now.Sub(time.UnixMilli(s.ev.Start))))
	}
}

// Recent returns the latest escalations sent.
func (e *Engine) Recent() []Record {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Record{}, e.Sent...)
}

// Run ticks every interval until ctx ends.
func (e *Engine) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.Tick(ctx)
		}
	}
}
