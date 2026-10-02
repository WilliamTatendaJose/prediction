// Package stream fans live readings out to Server-Sent Events clients.
// Each message is JSON-encoded once and shared by every subscriber; a client
// that falls behind loses messages instead of slowing ingestion.
package stream

import (
	"sync"
	"sync/atomic"
)

type Hub struct {
	mu      sync.RWMutex
	subs    map[chan []byte]struct{}
	buf     int
	Dropped atomic.Uint64
}

func NewHub(buffer int) *Hub {
	return &Hub{subs: map[chan []byte]struct{}{}, buf: buffer}
}

func (h *Hub) Subscribe() chan []byte {
	ch := make(chan []byte, h.buf)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *Hub) Unsubscribe(ch chan []byte) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
}

func (h *Hub) Clients() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs)
}

func (h *Hub) Publish(msg []byte) {
	h.mu.RLock()
	for ch := range h.subs {
		select {
		case ch <- msg:
		default:
			h.Dropped.Add(1)
		}
	}
	h.mu.RUnlock()
}
