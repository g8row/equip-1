// Package stream contains the live streaming machinery: mediamtx lifecycle, a
// generic MJPEG fan-out hub, the seamless DV capture hub, the lazy preview
// push, and the WHEP reverse-proxy helpers. It ports api/managers.py and
// api/preview.py.
package stream

import (
	"io"
	"log/slog"
	"sync"
)

const (
	// mjpegChunkSize is the read size for ffmpeg/dvgrab stdout (bytes).
	mjpegChunkSize = 8192
	// clientQueueDepth is the per-subscriber channel buffer (frames) before
	// frames are dropped for a slow client.
	clientQueueDepth = 40
)

// Hub is a generic MJPEG fan-out: a single reader goroutine pulls chunks from an
// io.Reader and broadcasts them (non-blocking) to all subscriber channels. Slow
// subscribers have frames dropped rather than blocking the reader. A nil
// sentinel is broadcast on EOF/stop. Used by the broadcaster, the seamless hub,
// and the recording fanout.
type Hub struct {
	mu          sync.Mutex
	subscribers map[uint64]chan []byte
	nextID      uint64
	name        string
}

// NewHub returns an empty hub. name is used only for logging.
func NewHub(name string) *Hub {
	return &Hub{
		subscribers: make(map[uint64]chan []byte),
		name:        name,
	}
}

// Subscribe registers a new subscriber, returning its id and receive channel.
func (h *Hub) Subscribe() (uint64, <-chan []byte) {
	ch := make(chan []byte, clientQueueDepth)
	h.mu.Lock()
	h.nextID++
	id := h.nextID
	h.subscribers[id] = ch
	total := len(h.subscribers)
	h.mu.Unlock()
	slog.Info(h.name+"-subscriber-add", "cid", id, "total", total)
	return id, ch
}

// Unsubscribe removes a subscriber by id.
func (h *Hub) Unsubscribe(id uint64) {
	h.mu.Lock()
	delete(h.subscribers, id)
	total := len(h.subscribers)
	h.mu.Unlock()
	slog.Info(h.name+"-subscriber-remove", "cid", id, "total", total)
}

// SubscriberCount returns the number of active subscribers.
func (h *Hub) SubscriberCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subscribers)
}

// Broadcast performs a non-blocking send of buf to every subscriber, dropping
// the frame for any subscriber whose buffer is full.
func (h *Hub) Broadcast(buf []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ch := range h.subscribers {
		select {
		case ch <- buf:
		default:
			// slow client — drop frame, never block
		}
	}
}

// CloseAll broadcasts the nil sentinel to all subscribers (signalling EOF) and
// clears the subscriber map.
func (h *Hub) CloseAll() {
	h.mu.Lock()
	subs := make([]chan []byte, 0, len(h.subscribers))
	for _, ch := range h.subscribers {
		subs = append(subs, ch)
	}
	h.subscribers = make(map[uint64]chan []byte)
	h.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- nil:
		default:
		}
	}
}

// sentinelAll broadcasts the nil sentinel without clearing subscribers.
func (h *Hub) sentinelAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ch := range h.subscribers {
		select {
		case ch <- nil:
		default:
		}
	}
}

// ReadLoop reads fixed-size chunks from r and broadcasts each to subscribers
// until EOF/error, then broadcasts the nil sentinel. It runs in the caller's
// goroutine; the caller typically launches it with `go`.
func (h *Hub) ReadLoop(r io.Reader) {
	buf := make([]byte, mjpegChunkSize)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			h.Broadcast(chunk)
		}
		if err != nil {
			if err != io.EOF {
				slog.Info(h.name+"-reader-error", "error", err)
			}
			break
		}
	}
	h.sentinelAll()
	slog.Info(h.name + "-reader-done")
}
