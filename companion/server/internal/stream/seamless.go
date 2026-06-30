package stream

import (
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"

	"equip1/companion/server/internal/capture"
	"equip1/companion/server/internal/encoders"
	"equip1/companion/server/internal/proc"
)

// SeamlessDvHub is the single-owner DV capture hub used in dvgrab mode. It ports
// managers.SeamlessDvHub.
//
// Pipeline:
//
//	dvgrab --format raw -  ->  ffmpeg (DV -> RTSP + MJPEG pipe)
//
// A single pump goroutine reads dvgrab stdout and writes it to ffmpeg stdin AND
// (when recording) to the open record file. A reader goroutine reads ffmpeg's
// mpjpeg stdout into a Hub for MJPEG subscribers. Recording toggles only the
// file handle on/off — capture ownership (the single FireWire owner) never
// restarts.
type SeamlessDvHub struct {
	mediamtx *MediamtxManager
	rtspURL  string
	hub      *Hub

	mu           sync.Mutex
	running      bool
	dvgrab       *proc.Proc
	ffmpeg       *proc.Proc
	dvgrabStdout *os.File
	ffmpegStdin  *os.File
	ffmpegStdout *os.File
	recordFile   *os.File
	recordPath   string
}

// NewSeamlessDvHub returns a seamless hub publishing to rtspURL via mediamtx.
func NewSeamlessDvHub(mediamtx *MediamtxManager, rtspURL string) *SeamlessDvHub {
	return &SeamlessDvHub{
		mediamtx: mediamtx,
		rtspURL:  rtspURL,
		hub:      NewHub("seamless"),
	}
}

// EnsureRunning starts the capture pipeline if it is not already alive. It is
// idempotent. Mirrors ensure_running (which can raise when no encoder is
// available — here that surfaces as an error).
func (s *SeamlessDvHub) EnsureRunning() error {
	s.mu.Lock()
	if s.running && s.dvgrab != nil && s.ffmpeg != nil &&
		!s.dvgrab.Exited() && !s.ffmpeg.Exited() {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	s.Stop()

	if !s.mediamtx.IsRunning() {
		s.mediamtx.Start()
	}

	rtspArgs, err := encoders.BuildRTSPVideoOutputArgs(s.rtspURL)
	if err != nil {
		return err
	}

	// --- dvgrab: stdout (read by pump) + stderr (drained) ---
	dvgrabArgs := capture.DvgrabArgs()
	dvgrabCmd := exec.Command(dvgrabArgs[0], dvgrabArgs[1:]...)
	dvOutR, dvOutW, err := proc.NewStdoutPipe(dvgrabCmd)
	if err != nil {
		return err
	}
	dvErrR, dvErrW, err := proc.NewStderrPipe(dvgrabCmd)
	if err != nil {
		dvOutR.Close()
		dvOutW.Close()
		return err
	}
	dvgrabProc, err := proc.Start(dvgrabCmd)
	if err != nil {
		dvOutR.Close()
		dvOutW.Close()
		dvErrR.Close()
		dvErrW.Close()
		return err
	}
	dvOutW.Close()
	dvErrW.Close()
	proc.DrainStderr("seamless-dvgrab", dvErrR)

	// --- ffmpeg: stdin (written by pump) + stdout (read by reader) + stderr ---
	ffArgs := capture.SeamlessFFmpegArgs(rtspArgs)
	ffmpegCmd := exec.Command(ffArgs[0], ffArgs[1:]...)
	ffInR, ffInW, err := proc.NewStdinPipe(ffmpegCmd)
	if err != nil {
		dvgrabProc.Terminate(3 * time.Second)
		dvOutR.Close()
		return err
	}
	ffOutR, ffOutW, err := proc.NewStdoutPipe(ffmpegCmd)
	if err != nil {
		dvgrabProc.Terminate(3 * time.Second)
		dvOutR.Close()
		ffInR.Close()
		ffInW.Close()
		return err
	}
	ffErrR, ffErrW, err := proc.NewStderrPipe(ffmpegCmd)
	if err != nil {
		dvgrabProc.Terminate(3 * time.Second)
		dvOutR.Close()
		ffInR.Close()
		ffInW.Close()
		ffOutR.Close()
		ffOutW.Close()
		return err
	}
	ffmpegProc, err := proc.Start(ffmpegCmd)
	if err != nil {
		dvgrabProc.Terminate(3 * time.Second)
		dvOutR.Close()
		ffInR.Close()
		ffInW.Close()
		ffOutR.Close()
		ffOutW.Close()
		ffErrR.Close()
		ffErrW.Close()
		return err
	}
	ffInR.Close()  // child holds its own copy of the read end
	ffOutW.Close() // parent releases the write end so reads see EOF on exit
	ffErrW.Close()
	proc.DrainStderr("seamless-ffmpeg", ffErrR)

	s.mu.Lock()
	s.dvgrab = dvgrabProc
	s.ffmpeg = ffmpegProc
	s.dvgrabStdout = dvOutR
	s.ffmpegStdin = ffInW
	s.ffmpegStdout = ffOutR
	s.running = true
	s.mu.Unlock()

	go s.pumpLoop(dvOutR, ffInW)
	go func() {
		s.hub.ReadLoop(ffOutR)
		slog.Info("seamless-reader-stop")
	}()
	slog.Info("seamless-hub-start", "dvgrab_pid", dvgrabProc.Pid(), "ffmpeg_pid", ffmpegProc.Pid())
	return nil
}

// IsRunning reports whether both capture processes are alive.
func (s *SeamlessDvHub) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running && s.dvgrab != nil && s.ffmpeg != nil &&
		!s.dvgrab.Exited() && !s.ffmpeg.Exited()
}

// StartRecording opens the output file so the pump begins tee-ing capture to
// disk, without restarting capture. Idempotent if already recording.
func (s *SeamlessDvHub) StartRecording(outputPath string) error {
	if err := s.EnsureRunning(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recordFile != nil {
		slog.Info("seamless-record-start-ignored", "file", s.recordPath)
		return nil
	}
	f, err := os.OpenFile(outputPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	s.recordFile = f
	s.recordPath = outputPath
	slog.Info("seamless-record-start", "file", outputPath)
	return nil
}

// StopRecording closes the record file (capture keeps running).
func (s *SeamlessDvHub) StopRecording() {
	s.mu.Lock()
	handle := s.recordFile
	path := s.recordPath
	s.recordFile = nil
	s.recordPath = ""
	s.mu.Unlock()
	if handle != nil {
		handle.Sync()
		handle.Close()
		slog.Info("seamless-record-stop", "file", path)
	}
}

// Subscribe registers an MJPEG subscriber (ensuring capture is running first).
func (s *SeamlessDvHub) Subscribe() (uint64, <-chan []byte, error) {
	if err := s.EnsureRunning(); err != nil {
		return 0, nil, err
	}
	id, ch := s.hub.Subscribe()
	return id, ch, nil
}

// Unsubscribe removes an MJPEG subscriber.
func (s *SeamlessDvHub) Unsubscribe(id uint64) { s.hub.Unsubscribe(id) }

// Stop tears down capture, sentinels subscribers and closes the record file.
func (s *SeamlessDvHub) Stop() {
	s.mu.Lock()
	s.running = false
	ffmpeg := s.ffmpeg
	dvgrab := s.dvgrab
	s.ffmpeg = nil
	s.dvgrab = nil
	dvOut := s.dvgrabStdout
	ffIn := s.ffmpegStdin
	ffOut := s.ffmpegStdout
	s.dvgrabStdout = nil
	s.ffmpegStdin = nil
	s.ffmpegStdout = nil
	handle := s.recordFile
	s.recordFile = nil
	s.recordPath = ""
	s.mu.Unlock()

	s.hub.CloseAll()

	if handle != nil {
		handle.Sync()
		handle.Close()
	}

	if ffmpeg != nil {
		ffmpeg.Terminate(3 * time.Second)
	}
	if dvgrab != nil {
		dvgrab.Terminate(3 * time.Second)
	}

	// Close our pipe ends so the pump/reader goroutines unblock.
	if ffIn != nil {
		ffIn.Close()
	}
	if dvOut != nil {
		dvOut.Close()
	}
	if ffOut != nil {
		ffOut.Close()
	}
	slog.Info("seamless-hub-stop")
}

// pumpLoop reads DV chunks from dvgrab and writes them to ffmpeg stdin and (when
// recording) the open record file. On any read/write failure it tears the hub
// down, mirroring _pump_loop calling self.stop().
func (s *SeamlessDvHub) pumpLoop(dvOut io.Reader, ffIn io.Writer) {
	buf := make([]byte, mjpegChunkSize)
	for {
		s.mu.Lock()
		running := s.running
		recordFile := s.recordFile
		s.mu.Unlock()
		if !running {
			break
		}

		n, err := dvOut.Read(buf)
		if n > 0 {
			if _, werr := ffIn.Write(buf[:n]); werr != nil {
				slog.Warn("seamless-pump-ffmpeg-write-failed", "error", werr)
				break
			}
			if recordFile != nil {
				if _, ferr := recordFile.Write(buf[:n]); ferr != nil {
					slog.Warn("seamless-pump-file-write-failed", "error", ferr)
				}
			}
		}
		if err != nil {
			break
		}
	}
	slog.Info("seamless-pump-stop")
	s.Stop()
}
