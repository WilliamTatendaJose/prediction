// Package stream fans live messages out to Server-Sent Events clients.
// Each message is JSON-encoded once and shared by every subscriber; a client
// that falls behind loses messages instead of slowing ingestion.
package stream

import (
	"sync"
	"sync/atomic"
)

// Msg is one SSE message. Event is the SSE event name ("" = default
// "message"); Sensor lets the stream filter without decoding Data.
type Msg struct {
	Event  string
	Sensor string
	Data   []byte
}

type Hub struct {
	mu      sync.RWMutex
	subs    map[chan *Msg]struct{}
	buf     int
	Dropped atomic.Uint64
	done    chan struct{}
	once    sync.Once
}

func NewHub(buffer int) *Hub {
	return &Hub{subs: map[chan *Msg]struct{}{}, buf: buffer, done: make(chan struct{})}
}

// Close ends every stream (the tenant stopped).
func (h *Hub) Close() { h.once.Do(func() { close(h.done) }) }

// Done is closed by Close.
func (h *Hub) Done() <-chan struct{} { return h.done }

func (h *Hub) Subscribe() chan *Msg {
	ch := make(chan *Msg, h.buf)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *Hub) Unsubscribe(ch chan *Msg) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
}

func (h *Hub) Clients() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs)
}

func (h *Hub) Publish(m *Msg) {
	h.mu.RLock()
	for ch := range h.subs {
		select {
		case ch <- m:
		default:
			h.Dropped.Add(1)
		}
	}
	h.mu.RUnlock()
}
