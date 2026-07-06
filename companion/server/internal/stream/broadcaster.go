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

	mu sync.Mutex
	// gen is bumped every time Start (re)spawns ffmpeg. It lets the
	// reader-done goroutine from a superseded spawn recognize it no longer
	// owns the "running" state, mirroring the same guard in SeamlessDvHub.
	gen     uint64
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
//
// The lock is held across the entire body, spawn included, so two concurrent
// callers can't both observe "not running" and each spawn their own ffmpeg.
// running is only set true after a successful spawn — an early "running=true"
// before the spawn attempt would itself create exactly that window if the
// spawn is slow or fails.
func (b *MjpegBroadcaster) Start() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.running {
		return
	}

	b.stopLocked(b.gen)
	b.gen++
	gen := b.gen

	args := capture.BroadcasterFFmpegArgs(b.rtspURL)
	cmd := exec.Command(args[0], args[1:]...)
	r, w, err := proc.NewStdoutPipe(cmd)
	if err != nil {
		slog.Error("mjpeg-broadcaster-ffmpeg-failed", "error", err)
		return
	}
	// stderr → DEVNULL (left nil).
	p, err := proc.Start(cmd)
	if err != nil {
		r.Close()
		w.Close()
		slog.Error("mjpeg-broadcaster-ffmpeg-failed", "error", err)
		return
	}
	w.Close() // parent must release its write end so reads see EOF on exit

	b.ffmpeg = p
	b.stdoutR = r
	b.running = true

	slog.Info("mjpeg-broadcaster-start", "gen", gen, "ffmpeg_pid", p.Pid(), "rtsp", b.rtspURL)

	go func() {
		b.hub.ReadLoop(r)
		r.Close()
		b.stopIfGen(gen)
	}()
}

// Stop terminates the broadcaster ffmpeg and notifies all clients.
func (b *MjpegBroadcaster) Stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.gen++
	b.stopLocked(b.gen)
}

// stopLocked tears down the current ffmpeg process, if any. Caller must hold
// b.mu. Safe to call when nothing is running.
func (b *MjpegBroadcaster) stopLocked(gen uint64) {
	if !b.running && b.ffmpeg == nil {
		return
	}
	b.running = false
	ffmpeg := b.ffmpeg
	b.ffmpeg = nil
	b.stdoutR = nil

	if ffmpeg != nil {
		ffmpeg.Terminate(3 * time.Second)
	}
	b.hub.CloseAll()
	slog.Info("mjpeg-broadcaster-stopped", "gen", gen)
}

// stopIfGen clears running only if gen is still the current generation —
// otherwise a newer Start() has already superseded this spawn and this
// reader-done goroutine must not clobber its state.
func (b *MjpegBroadcaster) stopIfGen(gen uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.gen != gen {
		return
	}
	b.stopLocked(gen)
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
