// Package recorder manages the recording pipeline, porting api/recorder.py.
//
// dvgrab mode delegates to the always-on seamless hub (toggling file writing).
// ffmpeg-only mode spawns its own ffmpeg writing lossless DV to disk, plus an
// optional RTSP output (when a WebRTC-compatible encoder exists) or an MJPEG
// fanout (when it does not).
package recorder

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"equip1/companion/server/internal/capture"
	"equip1/companion/server/internal/config"
	"equip1/companion/server/internal/encoders"
	"equip1/companion/server/internal/proc"
	"equip1/companion/server/internal/stream"
)

const (
	// minFreeBytesToStart blocks starting a new recording when free space is
	// already this low — DV capture runs ~3.5MB/s; this is a few minutes of
	// headroom, not a hard safety margin.
	minFreeBytesToStart = 200 * 1024 * 1024
	// minFreeBytesCritical auto-stops an in-progress recording before the
	// disk fills completely and capture processes start failing mid-write.
	// 150MB (~40s of DV headroom) instead of 50MB: the watchdog only samples
	// every storageWatchInterval, so the margin has to absorb a full
	// interval's worth of writes plus however long Stop()'s Terminate calls
	// take, not just the instant the check fires.
	minFreeBytesCritical = 150 * 1024 * 1024
	// storageWatchInterval is how often the background watchdog checks free
	// space while a recording is active. 3s (down from 10s) so a fast-filling
	// disk is caught with enough of minFreeBytesCritical's margin left to
	// stop cleanly before capture processes start failing mid-write.
	storageWatchInterval = 3 * time.Second
)

// Stop reasons, exposed via /api/status so the app can show *why* recording
// isn't running instead of the user guessing.
const (
	ReasonUser        = "user"         // explicit stop via the API
	ReasonDiskFull    = "disk_full"    // storageWatchLoop auto-stop
	ReasonProcessDied = "process_died" // dvgrab/ffmpeg exited unexpectedly
	ReasonWriteError  = "write_error"  // record-file write failed mid-capture
	ReasonShutdown    = "shutdown"     // companion-api graceful shutdown
)

// ErrAlreadyStarting is returned by Start when another Start call is still in
// flight (mode already flipped to "starting" but not yet "recording" or back
// to "idle"). Distinguished from other Start errors so callers can map it to
// HTTP 409 instead of 503.
var ErrAlreadyStarting = errors.New("a recording is already starting")

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

	mu             sync.Mutex
	mode           string // "idle" | "starting" | "recording"
	startTime      time.Time
	dvgrab         *proc.Proc
	mux            *proc.Proc
	muxStdout      *os.File
	currentFile    string // the ".part" path actually being written, while recording
	finalFile      string // currentFile's name after a clean stop renames it
	lastStopReason string
	lastStopAt     time.Time

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
	r := &RecorderState{
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
	go r.storageWatchLoop()
	return r
}

// freeBytes reports free space on the filesystem containing dir.
func freeBytes(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}

// storageWatchLoop runs for the life of the process, auto-stopping an
// in-progress recording before the disk fills completely — a full disk mid-
// write leaves a truncated/corrupt capture file and can wedge ffmpeg/dvgrab.
func (r *RecorderState) storageWatchLoop() {
	ticker := time.NewTicker(storageWatchInterval)
	defer ticker.Stop()
	for range ticker.C {
		if !r.IsRecording() {
			continue
		}
		free, err := freeBytes(r.captureDir)
		if err != nil {
			continue
		}
		if free < minFreeBytesCritical {
			slog.Error("record-storage-critical-auto-stop",
				"free_bytes", free, "threshold_bytes", minFreeBytesCritical)
			r.stopWithReason(ReasonDiskFull)
		}
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

// Start begins recording. Returns an error (mapped to HTTP 503, or
// ErrAlreadyStarting mapped to 409) when requirements are unmet, a start is
// already in flight, or ffmpeg/dvgrab exits immediately.
func (r *RecorderState) Start() (err error) {
	r.RefreshProcessState()

	r.mu.Lock()
	switch r.mode {
	case "recording":
		current := r.currentFile
		r.mu.Unlock()
		slog.Info("record-start-ignored", "mode", "recording", "current_file", current)
		return nil
	case "starting":
		r.mu.Unlock()
		return ErrAlreadyStarting
	}
	// Claim "starting" in the same critical section as the mode check above —
	// otherwise two concurrent Start() calls can both observe "idle" and both
	// spawn a competing dvgrab/ffmpeg pair. Any early return below must go
	// through the deferred reset so a rejected/failed start doesn't strand
	// the recorder in "starting" forever.
	r.mode = "starting"
	r.mu.Unlock()
	defer func() {
		if err != nil {
			r.mu.Lock()
			if r.mode == "starting" { // a completed spawn already moved past this
				r.mode = "idle"
			}
			r.mu.Unlock()
		}
	}()

	captureMode := r.captureMode.Get()
	slog.Info("record-start", "capture_mode", captureMode)

	// ---- Requirements check ----
	if _, lookErr := exec.LookPath("ffmpeg"); lookErr != nil {
		return fmt.Errorf("ffmpeg is not installed")
	}
	if captureMode == "dvgrab" {
		fwNodes, _ := filepath.Glob("/dev/fw[0-9]*")
		if len(fwNodes) == 0 {
			return fmt.Errorf("Camera not found — no /dev/fw* device present")
		}
		if _, lookErr := exec.LookPath("dvgrab"); lookErr != nil {
			return fmt.Errorf("dvgrab is not installed")
		}
	}

	if free, ferr := freeBytes(r.captureDir); ferr == nil && free < minFreeBytesToStart {
		return fmt.Errorf("Not enough free storage to start recording (%.0f MB free, need %.0f MB)",
			float64(free)/1024/1024, float64(minFreeBytesToStart)/1024/1024)
	}

	selectedEncoder := encoders.SafeSelectedRTSPEncoder()

	// Every capture writes to a ".part" file first; Stop only renames it away
	// on a clean, user-initiated stop (see stopWithReason). Anything else —
	// crash, disk-full auto-stop, write error, shutdown — leaves the ".part"
	// name in place so files.List can flag it as a recovered/incomplete
	// capture instead of presenting it as a normal, complete recording.
	timestamp := time.Now().Format("20060102_150405")
	finalPath := filepath.Join(r.captureDir, fmt.Sprintf("capture_%s.dv", timestamp))
	outputPath := finalPath + ".part"

	// dvgrab mode uses the always-on seamless hub for stream + recording tap.
	if captureMode == "dvgrab" {
		slog.Info("record-start", "seamless-hub", true, "output", outputPath, "encoder", selectedEncoder)

		if err = r.seamless.StartRecording(outputPath); err != nil {
			return err
		}
		r.mu.Lock()
		r.mode = "recording"
		r.startTime = time.Now()
		r.currentFile = outputPath
		r.finalFile = finalPath
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

	slog.Info("record-start-requested", "output", outputPath)

	// Add RTSP output only when a WebRTC-compatible encoder is available.
	enableRTSPOutput := selectedEncoder != "" && encoders.IsWebRTCCompatible(selectedEncoder)
	var rtspOutputArgs []string
	var mjpegLiveArgs []string
	if enableRTSPOutput {
		args, argErr := encoders.BuildRTSPVideoOutputArgs(r.rtspURL)
		if argErr != nil {
			return argErr
		}
		rtspOutputArgs = args
	} else {
		mjpegLiveArgs = capture.RecorderMjpegLiveOutputArgs()
	}
	slog.Info("record-start-stream-path", "encoder", selectedEncoder, "rtsp_enabled", enableRTSPOutput)

	if err = r.spawnMux(captureMode, outputPath, rtspOutputArgs, mjpegLiveArgs, enableRTSPOutput); err != nil {
		return err
	}

	r.mu.Lock()
	r.mode = "recording"
	r.startTime = time.Now()
	r.currentFile = outputPath
	r.finalFile = finalPath
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
		r.stopWithReason(ReasonProcessDied)
		return fmt.Errorf("ffmpeg mux exited immediately (rc=%d)", rc)
	}
	if captureMode == "dvgrab" && dvgrab != nil && dvgrab.Exited() {
		rc := dvgrab.ExitCode()
		r.stopWithReason(ReasonProcessDied)
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

// Stop ends recording as a user-initiated action.
func (r *RecorderState) Stop() {
	r.stopWithReason(ReasonUser)
}

// StopWithReason ends recording, tagging the transition with reason
// (exported for cmd/companion-api's shutdown path; internal auto-stops use
// the private stopWithReason directly).
func (r *RecorderState) StopWithReason(reason string) {
	r.stopWithReason(reason)
}

// RecordFailed is invoked by the seamless hub (dvgrab mode) when a
// record-file write fails mid-capture. Unlike a normal stop the capture
// pipeline itself (dvgrab/ffmpeg feeding the stream) is untouched — the hub
// has already closed just the record-file tap — so this only needs to bring
// the recorder's own state machine back to idle with the right reason.
func (r *RecorderState) RecordFailed(reason string) {
	r.stopWithReason(reason)
}

// stopWithReason ends recording and records why. Only a clean, user-
// initiated stop finalizes the ".part" file to its real name (see Start's
// comment on outputPath) — every other reason leaves the ".part" name in
// place so files.List's recovered/incomplete check can flag it.
func (r *RecorderState) stopWithReason(reason string) {
	r.mu.Lock()
	mode := r.mode
	current := r.currentFile
	final := r.finalFile
	r.mu.Unlock()
	slog.Info("record-stop-requested", "mode", mode, "file", current, "reason", reason)

	captureMode := r.captureMode.Get()

	if captureMode == "dvgrab" {
		r.seamless.StopRecording()
	} else {
		r.mu.Lock()
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
	}

	if reason == ReasonUser && current != "" && final != "" {
		if err := os.Rename(current, final); err != nil {
			slog.Error("record-finalize-rename-failed", "from", current, "to", final, "error", err)
		} else {
			slog.Info("record-finalize", "file", final)
		}
	}

	r.mu.Lock()
	r.mode = "idle"
	r.startTime = time.Time{}
	r.currentFile = ""
	r.finalFile = ""
	r.lastStopReason = reason
	r.lastStopAt = time.Now()
	r.mu.Unlock()
	slog.Info("record-stop-complete", "reason", reason)
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
			r.stopWithReason(ReasonProcessDied)
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
		r.stopWithReason(ReasonProcessDied)
	}
}

// IsRecording reports whether a recording is in progress.
func (r *RecorderState) IsRecording() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.mode == "recording"
}

// Mode returns the current recorder mode ("idle" | "starting" | "recording").
func (r *RecorderState) Mode() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.mode
}

// CurrentFile returns the current recording file path (empty if idle). While
// recording this is the in-progress ".part" path (see Start); it is cleared
// on every stop, clean or not.
func (r *RecorderState) CurrentFile() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.currentFile
}

// LastStopReason returns why capture last stopped
// (Reason* constant; "" if it has never stopped this process's lifetime).
func (r *RecorderState) LastStopReason() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastStopReason
}

// LastStopAt returns when capture last stopped (zero if never).
func (r *RecorderState) LastStopAt() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastStopAt
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
