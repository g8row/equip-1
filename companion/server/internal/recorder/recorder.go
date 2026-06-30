// Package recorder manages the recording pipeline, porting api/recorder.py.
//
// dvgrab mode delegates to the always-on seamless hub (toggling file writing).
// ffmpeg-only mode spawns its own ffmpeg writing lossless DV to disk, plus an
// optional RTSP output (when a WebRTC-compatible encoder exists) or an MJPEG
// fanout (when it does not).
package recorder

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"equip1/companion/server/internal/capture"
	"equip1/companion/server/internal/config"
	"equip1/companion/server/internal/encoders"
	"equip1/companion/server/internal/proc"
	"equip1/companion/server/internal/stream"
)

// RecorderState manages recording lifecycle and state.
type RecorderState struct {
	captureDir     string
	captureMode    *config.CaptureMode
	mediamtx       *stream.MediamtxManager
	mediamtxBinary string
	seamless       *stream.SeamlessDvHub
	preview        *stream.PreviewPush
	rtspURL        string
	// stopAllDirectMjpeg releases any direct MJPEG streams owning the bus.
	stopAllDirectMjpeg func()

	mu          sync.Mutex
	mode        string // "idle" | "recording"
	startTime   time.Time
	dvgrab      *proc.Proc
	mux         *proc.Proc
	muxStdout   *os.File
	currentFile string

	// mjpegHub fans out the recording mux's mpjpeg output (ffmpeg-only, no-RTSP
	// path). Subscribers are never wired in main.py; kept for parity.
	mjpegHub *stream.Hub
}

// Deps bundles the recorder's dependencies.
type Deps struct {
	CaptureDir         string
	CaptureMode        *config.CaptureMode
	Mediamtx           *stream.MediamtxManager
	MediamtxBinary     string
	Seamless           *stream.SeamlessDvHub
	Preview            *stream.PreviewPush
	RTSPURL            string
	StopAllDirectMjpeg func()
}

// New constructs a RecorderState in the idle state.
func New(d Deps) *RecorderState {
	return &RecorderState{
		captureDir:         d.CaptureDir,
		captureMode:        d.CaptureMode,
		mediamtx:           d.Mediamtx,
		mediamtxBinary:     d.MediamtxBinary,
		seamless:           d.Seamless,
		preview:            d.Preview,
		rtspURL:            d.RTSPURL,
		stopAllDirectMjpeg: d.StopAllDirectMjpeg,
		mode:               "idle",
		mjpegHub:           stream.NewHub("record-mjpeg-fanout"),
	}
}

// Toggle starts recording if idle, otherwise stops it.
func (r *RecorderState) Toggle() error {
	if r.IsRecording() {
		r.Stop()
		return nil
	}
	return r.Start()
}

// Start begins recording. Returns an error (mapped to HTTP 503) when
// requirements are unmet or ffmpeg/dvgrab exits immediately.
func (r *RecorderState) Start() error {
	r.RefreshProcessState()

	r.mu.Lock()
	if r.mode == "recording" {
		current := r.currentFile
		r.mu.Unlock()
		slog.Info("record-start-ignored", "mode", "recording", "current_file", current)
		return nil
	}
	r.mu.Unlock()

	captureMode := r.captureMode.Get()
	slog.Info("record-start", "capture_mode", captureMode)

	// ---- Requirements check ----
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return fmt.Errorf("ffmpeg is not installed")
	}
	if captureMode == "dvgrab" {
		fwNodes, _ := filepath.Glob("/dev/fw[0-9]*")
		if len(fwNodes) == 0 {
			return fmt.Errorf("Camera not found — no /dev/fw* device present")
		}
		if _, err := exec.LookPath("dvgrab"); err != nil {
			return fmt.Errorf("dvgrab is not installed")
		}
	}

	selectedEncoder := encoders.SafeSelectedRTSPEncoder()

	// dvgrab mode uses the always-on seamless hub for stream + recording tap.
	if captureMode == "dvgrab" {
		timestamp := time.Now().Format("20060102_150405")
		outputPath := filepath.Join(r.captureDir, fmt.Sprintf("capture_%s.dv", timestamp))
		slog.Info("record-start", "seamless-hub", true, "output", outputPath, "encoder", selectedEncoder)

		if err := r.seamless.StartRecording(outputPath); err != nil {
			return err
		}
		r.mu.Lock()
		r.mode = "recording"
		r.startTime = time.Now()
		r.currentFile = outputPath
		r.mu.Unlock()
		slog.Info("record-start", "complete", true, "file", outputPath)
		return nil
	}

	// ---- ffmpeg-only mode ----
	if !r.mediamtx.IsRunning() {
		if !r.mediamtx.Start() {
			return fmt.Errorf(
				"mediamtx is not running and could not be started. "+
					"Install from https://github.com/bluenviron/mediamtx/releases "+
					"and ensure '%s' is in PATH.", r.mediamtxBinary)
		}
	}

	// Stop any live preview / direct MJPEG owning the FireWire bus.
	r.preview.Stop()
	if r.stopAllDirectMjpeg != nil {
		r.stopAllDirectMjpeg()
	}
	time.Sleep(300 * time.Millisecond) // give the bus a moment to release

	timestamp := time.Now().Format("20060102_150405")
	outputPath := filepath.Join(r.captureDir, fmt.Sprintf("capture_%s.dv", timestamp))
	slog.Info("record-start-requested", "output", outputPath)

	// Add RTSP output only when a WebRTC-compatible encoder is available.
	enableRTSPOutput := selectedEncoder != "" && encoders.IsWebRTCCompatible(selectedEncoder)
	var rtspOutputArgs []string
	var mjpegLiveArgs []string
	if enableRTSPOutput {
		args, err := encoders.BuildRTSPVideoOutputArgs(r.rtspURL)
		if err != nil {
			return err
		}
		rtspOutputArgs = args
	} else {
		mjpegLiveArgs = capture.RecorderMjpegLiveOutputArgs()
	}
	slog.Info("record-start-stream-path", "encoder", selectedEncoder, "rtsp_enabled", enableRTSPOutput)

	if err := r.spawnMux(captureMode, outputPath, rtspOutputArgs, mjpegLiveArgs, enableRTSPOutput); err != nil {
		return err
	}

	r.mu.Lock()
	r.mode = "recording"
	r.startTime = time.Now()
	r.currentFile = outputPath
	mux := r.mux
	dvgrab := r.dvgrab
	muxStdout := r.muxStdout
	r.mu.Unlock()

	if !enableRTSPOutput && muxStdout != nil {
		r.startMjpegFanout(muxStdout)
	}

	time.Sleep(200 * time.Millisecond)
	if mux != nil && mux.Exited() {
		rc := mux.ExitCode()
		r.Stop()
		return fmt.Errorf("ffmpeg mux exited immediately (rc=%d)", rc)
	}
	if captureMode == "dvgrab" && dvgrab != nil && dvgrab.Exited() {
		rc := dvgrab.ExitCode()
		r.Stop()
		return fmt.Errorf("dvgrab exited immediately (rc=%d)", rc)
	}

	slog.Info("record-start", "complete", true, "file", outputPath)
	return nil
}

// spawnMux spawns the recording ffmpeg (and dvgrab in the dvgrab branch, which
// is unreachable in practice since dvgrab mode uses the seamless hub — kept for
// structural parity with recorder.py).
func (r *RecorderState) spawnMux(captureMode, outputPath string, rtspOutputArgs, mjpegLiveArgs []string, enableRTSPOutput bool) error {
	if captureMode == "dvgrab" {
		dvgrabArgs := capture.DvgrabArgs()
		dvgrabCmd := exec.Command(dvgrabArgs[0], dvgrabArgs[1:]...)
		pipeR, pipeW, err := proc.NewStdoutPipe(dvgrabCmd)
		if err != nil {
			return err
		}
		dvErrR, dvErrW, err := proc.NewStderrPipe(dvgrabCmd)
		if err != nil {
			pipeR.Close()
			pipeW.Close()
			return err
		}
		dvgrabProc, err := proc.Start(dvgrabCmd)
		if err != nil {
			pipeR.Close()
			pipeW.Close()
			dvErrR.Close()
			dvErrW.Close()
			return err
		}
		dvErrW.Close()
		proc.DrainStderr("record-dvgrab", dvErrR)
		slog.Info("record-start", "dvgrab-pid", dvgrabProc.Pid())

		muxArgs := capture.RecorderDvgrabMuxArgs(outputPath, rtspOutputArgs, mjpegLiveArgs)
		muxCmd := exec.Command(muxArgs[0], muxArgs[1:]...)
		muxCmd.Stdin = pipeR
		var muxStdoutR *os.File
		if !enableRTSPOutput {
			or, ow, perr := proc.NewStdoutPipe(muxCmd)
			if perr != nil {
				dvgrabProc.Terminate(3 * time.Second)
				pipeR.Close()
				pipeW.Close()
				return perr
			}
			muxStdoutR = or
			defer ow.Close()
		}
		muxErrR, muxErrW, err := proc.NewStderrPipe(muxCmd)
		if err != nil {
			dvgrabProc.Terminate(3 * time.Second)
			pipeR.Close()
			pipeW.Close()
			if muxStdoutR != nil {
				muxStdoutR.Close()
			}
			return err
		}
		muxProc, err := proc.Start(muxCmd)
		if err != nil {
			dvgrabProc.Terminate(3 * time.Second)
			pipeR.Close()
			pipeW.Close()
			muxErrR.Close()
			muxErrW.Close()
			if muxStdoutR != nil {
				muxStdoutR.Close()
			}
			return err
		}
		muxErrW.Close()
		proc.DrainStderr("record-ffmpeg", muxErrR)
		pipeR.Close()
		pipeW.Close()

		r.mu.Lock()
		r.dvgrab = dvgrabProc
		r.mux = muxProc
		r.muxStdout = muxStdoutR
		r.mu.Unlock()
		slog.Info("record-start", "mux-pid", muxProc.Pid(), "rtsp", r.rtspURL)
		return nil
	}

	// ffmpeg-only / iec61883
	muxArgs := capture.RecorderIec61883MuxArgs(outputPath, rtspOutputArgs, mjpegLiveArgs)
	muxCmd := exec.Command(muxArgs[0], muxArgs[1:]...)
	var muxStdoutR *os.File
	if !enableRTSPOutput {
		or, ow, perr := proc.NewStdoutPipe(muxCmd)
		if perr != nil {
			return perr
		}
		muxStdoutR = or
		defer ow.Close()
	}
	muxErrR, muxErrW, err := proc.NewStderrPipe(muxCmd)
	if err != nil {
		if muxStdoutR != nil {
			muxStdoutR.Close()
		}
		return err
	}
	muxProc, err := proc.Start(muxCmd)
	if err != nil {
		muxErrR.Close()
		muxErrW.Close()
		if muxStdoutR != nil {
			muxStdoutR.Close()
		}
		return err
	}
	muxErrW.Close()
	proc.DrainStderr("record-ffmpeg-direct", muxErrR)

	r.mu.Lock()
	r.mux = muxProc
	r.muxStdout = muxStdoutR
	r.mu.Unlock()
	slog.Info("record-start", "ffmpeg-direct-pid", muxProc.Pid(), "rtsp", r.rtspURL)
	return nil
}

// startMjpegFanout reads the recording mux stdout and broadcasts chunks to the
// recording hub. Mirrors _start_recording_mjpeg_fanout.
func (r *RecorderState) startMjpegFanout(stdout *os.File) {
	slog.Info("record-mjpeg-fanout-start")
	go func() {
		r.mjpegHub.ReadLoop(stdout)
		stdout.Close()
		r.mjpegHub.CloseAll()
		slog.Info("record-mjpeg-fanout-stop")
	}()
}

// StopMjpegFanout sentinels and clears the recording fanout subscribers.
// Mirrors _stop_recording_mjpeg_fanout.
func (r *RecorderState) StopMjpegFanout() {
	r.mjpegHub.CloseAll()
}

// Stop ends recording.
func (r *RecorderState) Stop() {
	r.mu.Lock()
	mode := r.mode
	current := r.currentFile
	r.mu.Unlock()
	slog.Info("record-stop-requested", "mode", mode, "file", current)

	captureMode := r.captureMode.Get()

	if captureMode == "dvgrab" {
		r.seamless.StopRecording()
		r.mu.Lock()
		r.mode = "idle"
		r.startTime = time.Time{}
		r.mu.Unlock()
		slog.Info("record-stop-complete")
		return
	}

	r.mu.Lock()
	r.mode = "idle"
	r.startTime = time.Time{}
	mux := r.mux
	dvgrab := r.dvgrab
	r.mux = nil
	r.dvgrab = nil
	r.muxStdout = nil
	r.mu.Unlock()

	r.StopMjpegFanout()

	if mux != nil {
		mux.Terminate(3 * time.Second)
	}
	if dvgrab != nil {
		dvgrab.Terminate(3 * time.Second)
	}
	slog.Info("record-stop-complete")
}

// RefreshProcessState detects dead capture processes and resets state. Mirrors
// refresh_process_state.
func (r *RecorderState) RefreshProcessState() {
	r.mu.Lock()
	if r.mode != "recording" {
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()

	captureMode := r.captureMode.Get()
	if captureMode == "dvgrab" {
		if !r.seamless.IsRunning() {
			slog.Error("record-process-died", "detail", "seamless-hub not-running")
			r.mu.Lock()
			r.mode = "idle"
			r.startTime = time.Time{}
			r.mu.Unlock()
		}
		return
	}

	r.mu.Lock()
	dvgrab := r.dvgrab
	mux := r.mux
	r.mu.Unlock()

	dead := false
	if dvgrab != nil && dvgrab.Exited() {
		dead = true
	}
	if mux != nil && mux.Exited() {
		dead = true
	}
	if dead {
		slog.Error("record-process-died")
		r.Stop()
	}
}

// IsRecording reports whether a recording is in progress.
func (r *RecorderState) IsRecording() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.mode == "recording"
}

// Mode returns the current recorder mode ("idle" | "recording").
func (r *RecorderState) Mode() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.mode
}

// CurrentFile returns the current recording file path (empty if idle).
func (r *RecorderState) CurrentFile() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.currentFile
}

// ElapsedSeconds returns the recording duration in whole seconds.
func (r *RecorderState) ElapsedSeconds() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.mode != "recording" || r.startTime.IsZero() {
		return 0
	}
	return int(time.Since(r.startTime).Seconds())
}

// DvgrabPid returns the recording dvgrab pid, or 0 if none.
func (r *RecorderState) DvgrabPid() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dvgrab == nil {
		return 0
	}
	return r.dvgrab.Pid()
}

// MuxPid returns the recording mux pid, or 0 if none.
func (r *RecorderState) MuxPid() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.mux == nil {
		return 0
	}
	return r.mux.Pid()
}
