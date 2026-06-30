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

// DirectMjpegManager tracks direct (no-RTSP/no-mediamtx) MJPEG capture streams.
// It ports the _ACTIVE_DIRECT_MJPEG bookkeeping and _stream_mjpeg_direct_generate
// spawn logic from main.py. Each stream owns the FireWire bus for its lifetime;
// the endpoint enforces a one-client limit.
type DirectMjpegManager struct {
	mu     sync.Mutex
	active map[uint64]*DirectStream
	nextID uint64
}

// NewDirectMjpegManager returns an empty manager.
func NewDirectMjpegManager() *DirectMjpegManager {
	return &DirectMjpegManager{active: make(map[uint64]*DirectStream)}
}

// Count returns the number of active direct MJPEG streams.
func (m *DirectMjpegManager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.active)
}

// StopAll terminates every active direct MJPEG stream.
func (m *DirectMjpegManager) StopAll() {
	m.mu.Lock()
	active := make([]*DirectStream, 0, len(m.active))
	for _, s := range m.active {
		active = append(active, s)
	}
	m.active = make(map[uint64]*DirectStream)
	m.mu.Unlock()

	if len(active) == 0 {
		return
	}
	slog.Info("mjpeg-direct-stop-all", "count", len(active))
	for _, s := range active {
		slog.Info("mjpeg-direct-stop", "stream_id", s.id)
		s.terminate()
	}
}

// DirectStream is a single direct MJPEG capture, exposing the ffmpeg stdout as
// an io.Reader via Read.
type DirectStream struct {
	id        uint64
	mgr       *DirectMjpegManager
	dvgrab    *proc.Proc
	ffmpeg    *proc.Proc
	stdout    *os.File
	closeOnce sync.Once
}

// Start spawns a direct MJPEG capture for the given capture mode and registers
// it. The caller reads frames via Read and must call Close when done.
func (m *DirectMjpegManager) Start(captureMode string) (*DirectStream, error) {
	m.mu.Lock()
	m.nextID++
	id := m.nextID
	m.mu.Unlock()

	s := &DirectStream{id: id, mgr: m}

	if captureMode == "dvgrab" {
		dvgrabArgs := capture.DvgrabArgs()
		dvgrabCmd := exec.Command(dvgrabArgs[0], dvgrabArgs[1:]...)
		pipeR, pipeW, err := proc.NewStdoutPipe(dvgrabCmd)
		if err != nil {
			return nil, err
		}
		dvErrR, dvErrW, err := proc.NewStderrPipe(dvgrabCmd)
		if err != nil {
			pipeR.Close()
			pipeW.Close()
			return nil, err
		}
		dvgrabProc, err := proc.Start(dvgrabCmd)
		if err != nil {
			pipeR.Close()
			pipeW.Close()
			dvErrR.Close()
			dvErrW.Close()
			return nil, err
		}
		dvErrW.Close()
		proc.DrainStderr("mjpeg-direct-dvgrab", dvErrR)

		ffArgs := capture.DirectMjpegDvgrabFFmpegArgs()
		ffmpegCmd := exec.Command(ffArgs[0], ffArgs[1:]...)
		ffmpegCmd.Stdin = pipeR
		ffOutR, ffOutW, err := proc.NewStdoutPipe(ffmpegCmd)
		if err != nil {
			dvgrabProc.Terminate(3 * time.Second)
			pipeR.Close()
			pipeW.Close()
			return nil, err
		}
		ffErrR, ffErrW, err := proc.NewStderrPipe(ffmpegCmd)
		if err != nil {
			dvgrabProc.Terminate(3 * time.Second)
			pipeR.Close()
			pipeW.Close()
			ffOutR.Close()
			ffOutW.Close()
			return nil, err
		}
		ffmpegProc, err := proc.Start(ffmpegCmd)
		if err != nil {
			dvgrabProc.Terminate(3 * time.Second)
			pipeR.Close()
			pipeW.Close()
			ffOutR.Close()
			ffOutW.Close()
			ffErrR.Close()
			ffErrW.Close()
			return nil, err
		}
		ffOutW.Close()
		ffErrW.Close()
		proc.DrainStderr("mjpeg-direct-ffmpeg", ffErrR)
		pipeR.Close()
		pipeW.Close()

		s.dvgrab = dvgrabProc
		s.ffmpeg = ffmpegProc
		s.stdout = ffOutR
	} else {
		ffArgs := capture.DirectMjpegIec61883FFmpegArgs()
		ffmpegCmd := exec.Command(ffArgs[0], ffArgs[1:]...)
		ffOutR, ffOutW, err := proc.NewStdoutPipe(ffmpegCmd)
		if err != nil {
			return nil, err
		}
		ffErrR, ffErrW, err := proc.NewStderrPipe(ffmpegCmd)
		if err != nil {
			ffOutR.Close()
			ffOutW.Close()
			return nil, err
		}
		ffmpegProc, err := proc.Start(ffmpegCmd)
		if err != nil {
			ffOutR.Close()
			ffOutW.Close()
			ffErrR.Close()
			ffErrW.Close()
			return nil, err
		}
		ffOutW.Close()
		ffErrW.Close()
		proc.DrainStderr("mjpeg-direct-ffmpeg-only", ffErrR)

		s.ffmpeg = ffmpegProc
		s.stdout = ffOutR
	}

	m.mu.Lock()
	m.active[id] = s
	m.mu.Unlock()
	slog.Info("mjpeg-direct-register", "stream_id", id, "capture_mode", captureMode,
		"ffmpeg_pid", s.ffmpeg.Pid())
	return s, nil
}

// Read reads MJPEG bytes from the ffmpeg stdout.
func (s *DirectStream) Read(p []byte) (int, error) { return s.stdout.Read(p) }

// Close unregisters the stream and terminates its processes.
func (s *DirectStream) Close() {
	s.mgr.mu.Lock()
	delete(s.mgr.active, s.id)
	s.mgr.mu.Unlock()
	slog.Info("mjpeg-direct-unregister", "stream_id", s.id)
	s.terminate()
	slog.Info("mjpeg-direct-stop", "stream_id", s.id)
}

func (s *DirectStream) terminate() {
	s.closeOnce.Do(func() {
		if s.ffmpeg != nil {
			s.ffmpeg.Terminate(3 * time.Second)
		}
		if s.dvgrab != nil {
			s.dvgrab.Terminate(3 * time.Second)
		}
		if s.stdout != nil {
			s.stdout.Close()
		}
	})
}
