package stream

import (
	"log/slog"
	"os/exec"
	"sync"
	"time"

	"equip1/companion/server/internal/capture"
	"equip1/companion/server/internal/config"
	"equip1/companion/server/internal/encoders"
	"equip1/companion/server/internal/proc"
)

// previewRetryCooldown is the wait after a failure before retrying, to avoid
// hammering the FireWire bus with new processes.
const previewRetryCooldown = 3 * time.Second

// PreviewPush is the lazy dvgrab|iec61883 → ffmpeg → RTSP live preview. It is
// started lazily (first MJPEG/WebRTC request) and stopped when recording starts.
// Only one FireWire source process is ever running at a time. Ports
// preview.PreviewPush.
type PreviewPush struct {
	captureMode *config.CaptureMode
	mediamtx    *MediamtxManager
	rtspURL     string

	// isRecording is injected to break the recorder↔preview import cycle; it
	// reports whether recording currently owns the bus.
	isRecording func() bool

	mu          sync.Mutex
	dvgrab      *proc.Proc
	ffmpeg      *proc.Proc
	lastFailure time.Time
}

// NewPreviewPush constructs a preview push. SetRecordingCheck must be called to
// wire the recording-state predicate before use (mirrors main.py setting
// preview._recorder after construction).
func NewPreviewPush(captureMode *config.CaptureMode, mediamtx *MediamtxManager, rtspURL string) *PreviewPush {
	return &PreviewPush{
		captureMode: captureMode,
		mediamtx:    mediamtx,
		rtspURL:     rtspURL,
		isRecording: func() bool { return false },
	}
}

// SetRecordingCheck injects the recording-state predicate.
func (p *PreviewPush) SetRecordingCheck(fn func() bool) {
	p.mu.Lock()
	p.isRecording = fn
	p.mu.Unlock()
}

// EnsureRunning starts the preview push if not already running and not recording.
func (p *PreviewPush) EnsureRunning() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.isRecording() {
		return // recording owns the bus + mediamtx
	}
	if p.aliveLocked() {
		return
	}

	// Cooldown: don't hammer the FireWire bus after a recent failure.
	if elapsed := time.Since(p.lastFailure); elapsed < previewRetryCooldown {
		slog.Info("preview-push-cooldown", "remaining", (previewRetryCooldown - elapsed).Seconds())
		return
	}

	captureMode := p.captureMode.Get()
	slog.Info("preview-push-start", "capture_mode", captureMode)

	if !p.mediamtx.IsRunning() {
		p.mediamtx.Start()
	}

	rtspArgs, err := encoders.BuildRTSPVideoOutputArgs(p.rtspURL)
	if err != nil {
		slog.Error("preview-push-encoder-unavailable", "error", err)
		p.lastFailure = time.Now()
		return
	}

	if captureMode == "dvgrab" {
		p.startDvgrabLocked(rtspArgs)
	} else {
		p.startIec61883Locked(rtspArgs)
	}
}

// startDvgrabLocked spawns dvgrab piped directly into ffmpeg → RTSP. Caller holds p.mu.
func (p *PreviewPush) startDvgrabLocked(rtspArgs []string) {
	dvgrabArgs := capture.DvgrabArgs()
	dvgrabCmd := exec.Command(dvgrabArgs[0], dvgrabArgs[1:]...)

	// dvgrab stdout → ffmpeg stdin, connected directly via one os.Pipe.
	pipeR, pipeW, err := proc.NewStdoutPipe(dvgrabCmd)
	if err != nil {
		slog.Error("preview-push-dvgrab-pipe-failed", "error", err)
		return
	}
	dvErrR, dvErrW, err := proc.NewStderrPipe(dvgrabCmd)
	if err != nil {
		pipeR.Close()
		pipeW.Close()
		return
	}
	dvgrabProc, err := proc.Start(dvgrabCmd)
	if err != nil {
		// dvgrab not found, etc.
		pipeR.Close()
		pipeW.Close()
		dvErrR.Close()
		dvErrW.Close()
		slog.Error("preview-push-dvgrab-not-found", "error", err)
		return
	}
	dvErrW.Close()
	proc.DrainStderr("preview-dvgrab", dvErrR)

	ffArgs := capture.PreviewDvgrabFFmpegArgs(rtspArgs)
	ffmpegCmd := exec.Command(ffArgs[0], ffArgs[1:]...)
	ffmpegCmd.Stdin = pipeR // read directly from dvgrab's stdout pipe
	ffErrR, ffErrW, err := proc.NewStderrPipe(ffmpegCmd)
	if err != nil {
		dvgrabProc.Terminate(3 * time.Second)
		pipeR.Close()
		pipeW.Close()
		return
	}
	ffmpegProc, err := proc.Start(ffmpegCmd)
	if err != nil {
		slog.Error("preview-push-ffmpeg-failed", "error", err)
		dvgrabProc.Terminate(3 * time.Second)
		pipeR.Close()
		pipeW.Close()
		ffErrR.Close()
		ffErrW.Close()
		return
	}
	ffErrW.Close()
	proc.DrainStderr("preview-ffmpeg", ffErrR)

	// Parent closes both pipe ends — they belong to the children now.
	pipeW.Close()
	pipeR.Close()

	p.dvgrab = dvgrabProc
	p.ffmpeg = ffmpegProc

	// Sanity-check: if a process dies within 500ms, the camera is absent.
	time.Sleep(500 * time.Millisecond)
	if p.ffmpeg.Exited() || p.dvgrab.Exited() {
		slog.Error("preview-push-early-exit",
			"dvgrab_rc", p.dvgrab.ExitCode(), "ffmpeg_rc", p.ffmpeg.ExitCode())
		p.ffmpeg.Terminate(3 * time.Second)
		p.dvgrab.Terminate(3 * time.Second)
		p.ffmpeg = nil
		p.dvgrab = nil
		p.lastFailure = time.Now()
		return
	}
	slog.Info("preview-push-running", "dvgrab_pid", p.dvgrab.Pid(), "ffmpeg_pid", p.ffmpeg.Pid())
}

// startIec61883Locked spawns the direct iec61883 → ffmpeg → RTSP push. Caller holds p.mu.
func (p *PreviewPush) startIec61883Locked(rtspArgs []string) {
	ffArgs := capture.PreviewIec61883FFmpegArgs(rtspArgs)
	ffmpegCmd := exec.Command(ffArgs[0], ffArgs[1:]...)
	ffErrR, ffErrW, err := proc.NewStderrPipe(ffmpegCmd)
	if err != nil {
		return
	}
	ffmpegProc, err := proc.Start(ffmpegCmd)
	if err != nil {
		slog.Error("preview-push-ffmpeg-only-failed", "error", err)
		ffErrR.Close()
		ffErrW.Close()
		return
	}
	ffErrW.Close()
	proc.DrainStderr("preview-ffmpeg-direct", ffErrR)
	p.ffmpeg = ffmpegProc

	// Sanity-check for iec61883 start.
	time.Sleep(500 * time.Millisecond)
	if p.ffmpeg.Exited() {
		slog.Error("preview-push-early-exit", "ffmpeg_rc", p.ffmpeg.ExitCode(), "note", "iec61883 unavailable?")
		p.ffmpeg = nil
		p.lastFailure = time.Now()
		return
	}
	slog.Info("preview-push-running", "ffmpeg_pid", p.ffmpeg.Pid())
}

// Stop terminates the preview processes.
func (p *PreviewPush) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ffmpeg != nil {
		p.ffmpeg.Terminate(3 * time.Second)
	}
	if p.dvgrab != nil {
		p.dvgrab.Terminate(3 * time.Second)
	}
	p.ffmpeg = nil
	p.dvgrab = nil
	slog.Info("preview-push-stopped")
}

// IsAlive reports whether the preview ffmpeg is running.
func (p *PreviewPush) IsAlive() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.aliveLocked()
}

func (p *PreviewPush) aliveLocked() bool {
	return p.ffmpeg != nil && !p.ffmpeg.Exited()
}
