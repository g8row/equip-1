package stream

import (
	"log/slog"
	"net"
	"os/exec"
	"sync"
	"time"

	"equip1/companion/server/internal/proc"
)

// MediamtxManager manages the mediamtx subprocess lifecycle. Ports
// managers.MediamtxManager including the 5s restart cooldown and the `which`
// availability check.
type MediamtxManager struct {
	binary string
	config string // optional path to mediamtx.yml

	mu               sync.Mutex
	process          *proc.Proc
	lastStartAttempt time.Time
	restartCooldown  time.Duration
}

// NewMediamtxManager returns a manager for the given mediamtx binary name and
// an optional config path. mediamtx only reads a config from its CWD by default
// (never /etc), so without an explicit path it silently falls back to an empty
// config that rejects RTSP publishing (WHEP 400) — passing the path is what
// makes WHEP work. Empty config keeps mediamtx's built-in defaults.
func NewMediamtxManager(binary, config string) *MediamtxManager {
	return &MediamtxManager{
		binary:          binary,
		config:          config,
		restartCooldown: 5 * time.Second,
	}
}

// Start launches mediamtx if not already running. It is idempotent and applies
// a restart cooldown. Returns true if running (or already running).
func (m *MediamtxManager) Start() bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.process != nil && !m.process.Exited() {
		slog.Info("mediamtx-already-running", "pid", m.process.Pid())
		return true
	}
	if mediamtxPortOpen() {
		slog.Info("mediamtx-already-running-external")
		return true
	}

	now := time.Now()
	if now.Sub(m.lastStartAttempt) < m.restartCooldown {
		remaining := m.restartCooldown - now.Sub(m.lastStartAttempt)
		slog.Info("mediamtx-start-cooldown", "remaining", remaining.Seconds())
		return false
	}
	m.lastStartAttempt = now

	// `which` check via exec.LookPath.
	if _, err := exec.LookPath(m.binary); err != nil {
		slog.Warn("mediamtx-not-found",
			"binary", m.binary,
			"hint", "Install from https://github.com/bluenviron/mediamtx/releases")
		return false
	}

	var cmd *exec.Cmd
	if m.config != "" {
		cmd = exec.Command(m.binary, m.config)
	} else {
		cmd = exec.Command(m.binary)
	}
	// stdout → DEVNULL (Stdout left nil discards output).
	er, ew, err := proc.NewStderrPipe(cmd)
	if err != nil {
		slog.Error("mediamtx-start-failed", "error", err)
		return false
	}
	p, err := proc.Start(cmd)
	if err != nil {
		er.Close()
		ew.Close()
		slog.Error("mediamtx-start-failed", "error", err)
		return false
	}
	ew.Close()
	proc.DrainStderr("mediamtx", er)
	m.process = p
	slog.Info("mediamtx-start", "pid", p.Pid())
	// Give mediamtx a moment to bind its ports before clients connect.
	time.Sleep(500 * time.Millisecond)
	return true
}

// Stop terminates mediamtx.
func (m *MediamtxManager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.process != nil {
		m.process.Terminate(3 * time.Second)
	}
	m.process = nil
	slog.Info("mediamtx-stopped")
}

// IsRunning reports whether mediamtx is currently running (either as our
// subprocess or as an external process already bound to the RTSP port).
func (m *MediamtxManager) IsRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.process != nil && !m.process.Exited() {
		return true
	}
	return mediamtxPortOpen()
}

// mediamtxPortOpen returns true if something is already listening on :8554.
func mediamtxPortOpen() bool {
	c, err := net.DialTimeout("tcp", "127.0.0.1:8554", 300*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}
