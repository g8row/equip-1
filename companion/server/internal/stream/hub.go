// Package stream contains the live streaming machinery: mediamtx lifecycle, a
// generic MJPEG fan-out hub, the seamless DV capture hub, the lazy preview
// push, and the WHEP reverse-proxy helpers. It ports api/managers.py and
// api/preview.py.
package stream

import (
	"bytes"
	"io"
	"log/slog"
	"regexp"
	"strconv"
	"sync"
)

const (
	// mjpegChunkSize is the read size for ffmpeg/dvgrab stdout (bytes).
	mjpegChunkSize = 8192
	// clientQueueDepth is the per-subscriber channel buffer (frames) before
	// frames are dropped for a slow client. Broadcasts are whole mpjpeg
	// frames (see frameSplitter), so a small keep-latest depth is enough to
	// absorb jitter without letting a stalled client build up a backlog of
	// stale frames.
	clientQueueDepth = 2
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

// ReadLoop reads from r, splits ffmpeg's "-f mpjpeg" byte stream on frame
// boundaries (see frameSplitter) and broadcasts each complete frame — never
// an arbitrary read-sized chunk — to subscribers, until EOF/error, then
// broadcasts the nil sentinel. It runs in the caller's goroutine; the caller
// typically launches it with `go`.
func (h *Hub) ReadLoop(r io.Reader) {
	buf := make([]byte, mjpegChunkSize)
	var fp frameSplitter
	for {
		n, err := r.Read(buf)
		if n > 0 {
			for _, frame := range fp.push(buf[:n]) {
				h.Broadcast(frame)
			}
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

var (
	crlfcrlf        = []byte("\r\n\r\n")
	contentLengthRe = regexp.MustCompile(`(?i)content-length:\s*(\d+)`)
)

// frameSplitter parses an ffmpeg "-f mpjpeg" byte stream into complete,
// self-contained multipart units (boundary marker + headers + JPEG payload),
// using the in-body "Content-length:" header as the framing signal — mirrors
// web/src/lib/mjpeg.js's createFrameParser, which parses the same framing for
// the same reason: ffmpeg's declared boundary token and the response
// Content-Type both vary, so neither can be trusted.
//
// Unlike the JS parser (which discards the framing and hands bare JPEG bytes
// to <img blob:>), this keeps every byte: some subscribers (a plain
// <img src="..."> on web) decode the forwarded bytes as a native
// multipart/x-mixed-replace stream, so what's broadcast must stay one.
//
// Splitting on frame boundaries instead of forwarding arbitrary read-sized
// chunks means a subscriber whose queue is full only ever drops a whole
// frame — never a half-written header or a truncated JPEG payload, either of
// which would desync that client's parser until it reconnects.
type frameSplitter struct {
	buf       []byte
	need      int // payload bytes still needed to complete the current unit (0 = scanning for a header)
	bodyStart int // offset in buf where the payload begins; valid when need > 0
}

// push appends chunk to the internal buffer and returns every complete unit
// now available, in stream order.
func (fp *frameSplitter) push(chunk []byte) [][]byte {
	if len(chunk) > 0 {
		fp.buf = append(fp.buf, chunk...)
	}
	var units [][]byte
	for {
		if fp.need > 0 {
			total := fp.bodyStart + fp.need
			if len(fp.buf) < total {
				break
			}
			unit := make([]byte, total)
			copy(unit, fp.buf[:total])
			units = append(units, unit)
			fp.consume(total)
			fp.need = 0
			fp.bodyStart = 0
			continue
		}

		hdrEnd := bytes.Index(fp.buf, crlfcrlf)
		if hdrEnd < 0 {
			if len(fp.buf) > 1<<20 {
				// Never found a header — not mpjpeg framing, or the stream
				// is corrupt. Drop the buffer rather than growing it
				// forever; a well-formed header, if one ever arrives,
				// resyncs on its own.
				slog.Warn("mjpeg-frame-splitter-overflow", "buffered", len(fp.buf))
				fp.buf = nil
			}
			break
		}

		bodyStart := hdrEnd + len(crlfcrlf)
		m := contentLengthRe.FindSubmatch(fp.buf[:hdrEnd])
		if m == nil {
			// A boundary-only block with no length yet — keep scanning past
			// it (mirrors the JS parser) instead of matching it forever.
			fp.consume(bodyStart)
			continue
		}
		n, err := strconv.Atoi(string(m[1]))
		if err != nil || n < 0 {
			fp.consume(bodyStart)
			continue
		}
		fp.bodyStart = bodyStart
		fp.need = n
	}
	return units
}

// consume drops the first n bytes of buf, copying the remainder into a fresh
// backing array. Without this, repeatedly reslicing the front of a
// continuously-appended buffer would keep the entire history's backing array
// alive for the life of a long-running stream.
func (fp *frameSplitter) consume(n int) {
	rest := len(fp.buf) - n
	newBuf := make([]byte, rest)
	copy(newBuf, fp.buf[n:])
	fp.buf = newBuf
}
