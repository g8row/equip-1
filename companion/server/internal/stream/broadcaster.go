package stream

import (
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"

	"equip1/companion/server/internal/capture"
	"equip1/companion/server/internal/proc"
)

// MjpegBroadcaster runs a single ffmpeg that reads the mediamtx RTSP stream and
// re-encodes MJPEG, fanning the output out to N HTTP clients via a Hub. Slow
// clients drop frames; they never block others. Ports managers.MjpegBroadcaster.
type MjpegBroadcaster struct {
	rtspURL string
	hub     *Hub

	mu      sync.Mutex
	ffmpeg  *proc.Proc
	stdoutR *os.File
	running bool
}

// NewMjpegBroadcaster returns a broadcaster reading the given RTSP URL.
func NewMjpegBroadcaster(rtspURL string) *MjpegBroadcaster {
	return &MjpegBroadcaster{
		rtspURL: rtspURL,
		hub:     NewHub("mjpeg"),
	}
}

// Start launches the broadcaster ffmpeg (idempotent).
func (b *MjpegBroadcaster) Start() {
	b.mu.Lock()
	if b.running {
		b.mu.Unlock()
		return
	}
	b.running = true
	b.mu.Unlock()

	args := capture.BroadcasterFFmpegArgs(b.rtspURL)
	cmd := exec.Command(args[0], args[1:]...)
	r, w, err := proc.NewStdoutPipe(cmd)
	if err != nil {
		slog.Error("mjpeg-broadcaster-ffmpeg-failed", "error", err)
		b.mu.Lock()
		b.running = false
		b.mu.Unlock()
		return
	}
	// stderr → DEVNULL (left nil).
	p, err := proc.Start(cmd)
	if err != nil {
		r.Close()
		w.Close()
		slog.Error("mjpeg-broadcaster-ffmpeg-failed", "error", err)
		b.mu.Lock()
		b.running = false
		b.mu.Unlock()
		return
	}
	w.Close() // parent must release its write end so reads see EOF on exit

	b.mu.Lock()
	b.ffmpeg = p
	b.stdoutR = r
	b.mu.Unlock()

	slog.Info("mjpeg-broadcaster-start", "ffmpeg_pid", p.Pid(), "rtsp", b.rtspURL)

	go func() {
		b.hub.ReadLoop(r)
		r.Close()
		b.mu.Lock()
		b.running = false
		b.mu.Unlock()
	}()
}

// Stop terminates the broadcaster ffmpeg and notifies all clients.
func (b *MjpegBroadcaster) Stop() {
	b.mu.Lock()
	b.running = false
	ffmpeg := b.ffmpeg
	b.ffmpeg = nil
	b.mu.Unlock()

	if ffmpeg != nil {
		ffmpeg.Terminate(3 * time.Second)
	}
	b.hub.CloseAll()
	slog.Info("mjpeg-broadcaster-stopped")
}

// Subscribe registers a client.
func (b *MjpegBroadcaster) Subscribe() (uint64, <-chan []byte) { return b.hub.Subscribe() }

// Unsubscribe removes a client.
func (b *MjpegBroadcaster) Unsubscribe(id uint64) { b.hub.Unsubscribe(id) }

// SubscriberCount returns the number of active clients.
func (b *MjpegBroadcaster) SubscriberCount() int { return b.hub.SubscriberCount() }

// IsRunning reports whether the broadcaster is running.
func (b *MjpegBroadcaster) IsRunning() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.running
}
