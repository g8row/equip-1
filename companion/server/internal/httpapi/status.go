package httpapi

import (
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"equip1/companion/server/internal/encoders"
	"equip1/companion/server/internal/stream"
)

// firewireCameras returns the /dev/fw* nodes for *remote* FireWire devices —
// an actual camcorder on the bus, not the board's own controller.
//
// The local OHCI controller always enumerates as a node (/dev/fw0, is_local=1),
// so globbing /dev/fw* reports a "camera" even with nothing attached. A DV
// camcorder only appears as a remote node (is_local=0) while it is connected
// *and powered on*: plugged in but switched off, it powers down its FireWire
// PHY and drops off the bus entirely (verified on a Sony DCR-TRV900E — off: only
// fw0; on: fw1 with is_local=0). So a remote node is our "a live camera is ready
// to capture" signal, and its absence covers both no-camera and camera-off.
func firewireCameras() []string {
	nodes, _ := filepath.Glob("/sys/bus/firewire/devices/fw[0-9]*")
	cams := []string{}
	for _, n := range nodes {
		base := filepath.Base(n)
		// Skip unit sub-devices (fw1.0 etc.); only the node carries is_local.
		if strings.Contains(base, ".") {
			continue
		}
		local, err := os.ReadFile(filepath.Join(n, "is_local"))
		if err != nil || strings.TrimSpace(string(local)) != "0" {
			continue
		}
		cams = append(cams, "/dev/"+base)
	}
	return cams
}

// streamRequirements mirrors _check_stream_requirements.
func (s *Server) streamRequirements() map[string]any {
	cams := firewireCameras()
	return map[string]any{
		"dvgrab":         lookPathOK("dvgrab"),
		"ffmpeg":         lookPathOK("ffmpeg"),
		"mediamtx":       lookPathOK(s.cfg.MediamtxBinary),
		"camera_present": len(cams) > 0,
		"camera_devices": cams,
	}
}

func lookPathOK(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// activeStreamPipeline mirrors _active_stream_pipeline.
func (s *Server) activeStreamPipeline() string {
	mode := s.captureMode.Get()
	if mode == "dvgrab" {
		if s.seamless.IsRunning() {
			return "dvgrab-seamless-hub"
		}
		return "dvgrab-seamless-hub-idle"
	}
	if s.recorder.IsRecording() {
		return "ffmpeg-only-recording"
	}
	if s.preview.IsAlive() {
		return "ffmpeg-only-preview"
	}
	if s.broadcaster.IsRunning() {
		return "ffmpeg-only-mjpeg-broadcaster"
	}
	if s.directMjpeg.Count() > 0 {
		return "ffmpeg-only-direct-mjpeg"
	}
	return "ffmpeg-only-idle"
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.recorder.RefreshProcessState()
	req := s.streamRequirements()
	ffmpegOK := req["ffmpeg"].(bool)

	var rtspEncoder any
	webrtcAvailable := false
	if ffmpegOK {
		if enc := encoders.SafeSelectedRTSPEncoder(); enc != "" {
			rtspEncoder = enc
			webrtcAvailable = encoders.IsWebRTCCompatible(enc)
		}
	}

	captureMode := s.captureMode.Get()
	source := "preview"
	if s.recorder.IsRecording() {
		source = "recording"
	}

	stats, _ := s.files.Stats()

	writeJSON(w, http.StatusOK, map[string]any{
		"recorder": map[string]any{
			"mode":             s.recorder.Mode(),
			"elapsed_seconds":  s.recorder.ElapsedSeconds(),
			"current_file":     nullableString(s.recorder.CurrentFile()),
			"capture_mode":     captureMode,
			"last_stop_reason": nullableString(s.recorder.LastStopReason()),
			"last_stop_at":     nullableUnix(s.recorder.LastStopAt()),
		},
		"storage": stats,
		"files":   s.files.List(10, s.recorder.CurrentFile()),
		"network": s.networkStatus(),
		"stream": map[string]any{
			"available":          ffmpegOK && req["camera_present"].(bool),
			"requirements":       req,
			"mjpeg_url":          "/api/stream/mjpeg",
			"whep_proxy_url":     "/api/stream/whep",
			"mediamtx_running":   s.mediamtx.IsRunning(),
			"mediamtx_whep_port": s.cfg.MediamtxWHEPPort,
			"rtsp_video_encoder": rtspEncoder,
			"whep_available":     webrtcAvailable,
			"source":             source,
			"capture_mode":       captureMode,
			"pipeline":           s.activeStreamPipeline(),
		},
	})
}

func (s *Server) handleStreamRequirements(w http.ResponseWriter, r *http.Request) {
	checks := s.streamRequirements()
	ok := true
	for _, v := range checks {
		switch val := v.(type) {
		case bool:
			if !val {
				ok = false
			}
		case []string:
			if len(val) == 0 {
				ok = false
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     ok,
		"checks": checks,
	})
}

const mjpegContentType = "multipart/x-mixed-replace; boundary=ffmpeg"

// handleStreamMjpeg ports the /api/stream/mjpeg endpoint logic from main.py,
// including the dvgrab-seamless path, the no-webrtc-encoder direct fallback with
// one-client limit, and the broadcaster path.
func (s *Server) handleStreamMjpeg(w http.ResponseWriter, r *http.Request) {
	req := s.streamRequirements()
	if !req["ffmpeg"].(bool) {
		writeError(w, http.StatusServiceUnavailable, "ffmpeg is not installed")
		return
	}

	captureMode := s.captureMode.Get()
	rtspEncoder := encoders.SafeSelectedRTSPEncoder()
	webrtcOK := rtspEncoder != "" && encoders.IsWebRTCCompatible(rtspEncoder)

	// dvgrab mode always uses the seamless hub as single capture owner.
	if captureMode == "dvgrab" {
		if err := s.seamless.EnsureRunning(); err != nil {
			writeError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		id, ch, err := s.seamless.Subscribe()
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		defer s.seamless.Unsubscribe(id)
		slog.Info("mjpeg-seamless-hub", "reason", "dvgrab-single-owner", "encoder", rtspEncoder)
		streamChannel(w, r, ch, id, "seamless")
		return
	}

	// No-FIFO fallback: bypass mediamtx when no WebRTC-compatible encoder exists.
	if !webrtcOK {
		if s.directMjpeg.Count() > 0 {
			writeError(w, http.StatusTooManyRequests,
				"Direct MJPEG preview supports one client at a time on this device.")
			return
		}
		slog.Info("mjpeg-direct-fallback", "reason", "no-webrtc-compatible-encoder", "encoder", rtspEncoder)
		ds, err := s.directMjpeg.Start(captureMode)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		defer ds.Close()
		streamReader(w, r, ds)
		return
	}

	// Ensure something is pushing to mediamtx.
	if !s.recorder.IsRecording() {
		s.preview.EnsureRunning()
		time.Sleep(800 * time.Millisecond) // let preview connect & start frames
	}

	// Ensure the broadcaster is running.
	if !s.broadcaster.IsRunning() {
		s.broadcaster.Start()
		time.Sleep(500 * time.Millisecond) // brief warm-up
	}

	id, ch := s.broadcaster.Subscribe()
	defer func() {
		s.broadcaster.Unsubscribe(id)
		// Stop the broadcaster (and preview) when the last client leaves.
		if s.broadcaster.SubscriberCount() == 0 {
			s.broadcaster.Stop()
			if !s.recorder.IsRecording() {
				s.preview.Stop()
			}
		}
	}()
	streamChannel(w, r, ch, id, "mjpeg")
}

// streamChannel writes frames from a hub channel to the HTTP client, flushing
// each chunk, until the nil sentinel, a 10s idle timeout, or client disconnect.
func streamChannel(w http.ResponseWriter, r *http.Request, ch <-chan []byte, id uint64, label string) {
	w.Header().Set("Content-Type", mjpegContentType)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	ctx := r.Context()
	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case <-ctx.Done():
			slog.Info(label+"-client-disconnect", "cid", id)
			return
		case chunk, ok := <-ch:
			if !ok || chunk == nil {
				slog.Info(label+"-client-eof", "cid", id)
				return
			}
			if _, err := w.Write(chunk); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			if !timeout.Stop() {
				select {
				case <-timeout.C:
				default:
				}
			}
			timeout.Reset(10 * time.Second)
		case <-timeout.C:
			slog.Warn(label+"-client-timeout", "cid", id)
			return
		}
	}
}

// streamReader copies from an io.Reader (direct ffmpeg stdout) to the client,
// flushing each chunk, until EOF or client disconnect.
func streamReader(w http.ResponseWriter, r *http.Request, src io.Reader) {
	w.Header().Set("Content-Type", mjpegContentType)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	ctx := r.Context()
	buf := make([]byte, 8192)
	for {
		select {
		case <-ctx.Done():
			slog.Info("mjpeg-direct-client-disconnect")
			return
		default:
		}
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			slog.Info("mjpeg-direct-eof")
			return
		}
	}
}

// handleWhepPost ports the WHEP offer proxy with publisher warm-up and 404→503.
func (s *Server) handleWhepPost(w http.ResponseWriter, r *http.Request) {
	if !s.mediamtx.IsRunning() {
		writeError(w, http.StatusServiceUnavailable, "mediamtx is not running")
		return
	}

	encoder := encoders.SafeSelectedRTSPEncoder()
	if encoder == "" {
		writeError(w, http.StatusServiceUnavailable,
			"No usable RTSP video encoder is available on this device. Use /api/stream/mjpeg only.")
		return
	}
	if !encoders.IsWebRTCCompatible(encoder) {
		writeError(w, http.StatusServiceUnavailable,
			"WebRTC unavailable: selected RTSP encoder '"+encoder+"' is not WebRTC-compatible. "+
				"Use /api/stream/mjpeg for immediate preview.")
		return
	}

	// Ensure something is publishing to the RTSP path.
	if s.captureMode.Get() == "dvgrab" {
		if err := s.seamless.EnsureRunning(); err != nil {
			writeError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		time.Sleep(800 * time.Millisecond)
	} else if !s.recorder.IsRecording() {
		s.preview.EnsureRunning()
		time.Sleep(2 * time.Second)
	}

	body, _ := io.ReadAll(r.Body)
	resp, err := stream.ForwardOffer(r.Context(), s.cfg.MediamtxWHEPURL, body)
	if err != nil {
		if err == stream.ErrWHEPConnect {
			writeError(w, http.StatusBadGateway, "Cannot reach mediamtx at "+s.cfg.MediamtxWHEPURL)
			return
		}
		writeError(w, http.StatusInternalServerError, "WHEP proxy error")
		return
	}

	hw := w.Header()
	if origin := r.Header.Get("Origin"); origin == allowedOrigin {
		hw.Set("Access-Control-Allow-Origin", origin)
	}
	if resp.RetryAfter != "" {
		hw.Set("Retry-After", resp.RetryAfter)
	}
	hw.Set("Content-Type", resp.ContentType)
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)
}

// handleWhepPatch ports the trickle-ICE PATCH passthrough.
func (s *Server) handleWhepPatch(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	resp, err := stream.ForwardPatch(r.Context(), s.cfg.MediamtxWHEPURL, body, r.Header)
	if err != nil {
		writeError(w, http.StatusBadGateway, "WHEP patch proxy error")
		return
	}
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)
}
