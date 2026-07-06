// Package httpapi wires the HTTP routes, porting api/main.py to net/http with
// Go 1.22 method+path ServeMux patterns. CORS is restricted to the packaged
// app's fixed origin (see allowedOrigin); the embedded web UI is same-origin
// and needs no CORS headers at all. Every request is logged.
package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"

	"equip1/companion/server/internal/config"
	"equip1/companion/server/internal/files"
	"equip1/companion/server/internal/network"
	"equip1/companion/server/internal/recorder"
	"equip1/companion/server/internal/stream"
	"equip1/companion/server/internal/sysinfo"
)

// Server holds the singletons and serves the API.
type Server struct {
	cfg         *config.Config
	captureMode *config.CaptureMode
	mediamtx    *stream.MediamtxManager
	broadcaster *stream.MjpegBroadcaster
	seamless    *stream.SeamlessDvHub
	preview     *stream.PreviewPush
	directMjpeg *stream.DirectMjpegManager
	recorder    *recorder.RecorderState
	files       *files.Store
	network     *network.Manager

	reqMu          sync.Mutex
	activeRequests map[string]map[string]any
}

// Deps bundles the server's dependencies.
type Deps struct {
	Config      *config.Config
	CaptureMode *config.CaptureMode
	Mediamtx    *stream.MediamtxManager
	Broadcaster *stream.MjpegBroadcaster
	Seamless    *stream.SeamlessDvHub
	Preview     *stream.PreviewPush
	DirectMjpeg *stream.DirectMjpegManager
	Recorder    *recorder.RecorderState
	Files       *files.Store
	Network     *network.Manager // optional; nil = return placeholder responses
}

// New constructs a Server.
func New(d Deps) *Server {
	return &Server{
		cfg:            d.Config,
		captureMode:    d.CaptureMode,
		mediamtx:       d.Mediamtx,
		broadcaster:    d.Broadcaster,
		seamless:       d.Seamless,
		preview:        d.Preview,
		directMjpeg:    d.DirectMjpeg,
		recorder:       d.Recorder,
		files:          d.Files,
		network:        d.Network,
		activeRequests: make(map[string]map[string]any),
	}
}

// Handler returns the fully-wired http.Handler (mux + middleware chain).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /api/status", s.handleStatus)

	mux.HandleFunc("POST /api/record/toggle", s.handleRecordToggle)
	mux.HandleFunc("POST /api/record/start", s.handleRecordStart)
	mux.HandleFunc("POST /api/record/stop", s.handleRecordStop)

	mux.HandleFunc("GET /api/config/recording-capture-mode", s.handleGetCaptureMode)
	mux.HandleFunc("POST /api/config/recording-capture-mode", s.handleSetCaptureMode)

	mux.HandleFunc("GET /api/files", s.handleFiles)
	mux.HandleFunc("GET /api/files/download/{name}", s.handleDownload)
	mux.HandleFunc("GET /api/files/thumbnail/{name}", s.handleThumbnail)
	mux.HandleFunc("DELETE /api/files/{name}", s.handleDelete)
	mux.HandleFunc("GET /api/storage", s.handleStorage)

	mux.HandleFunc("GET /api/stream/requirements", s.handleStreamRequirements)
	mux.HandleFunc("GET /api/stream/mjpeg", s.handleStreamMjpeg)
	mux.HandleFunc("POST /api/stream/whep", s.handleWhepPost)
	mux.HandleFunc("PATCH /api/stream/whep", s.handleWhepPatch)

	mux.HandleFunc("GET /api/debug/runtime", s.handleDebugRuntime)
	mux.HandleFunc("GET /api/system/power", s.handleSystemPower)
	mux.HandleFunc("POST /api/system/restart", s.handleSystemRestart)

	mux.HandleFunc("GET /api/network", s.handleGetNetwork)
	mux.HandleFunc("POST /api/network/wifi", s.handleSetWifi)
	mux.HandleFunc("POST /api/network/ap", s.handleSetAP)
	mux.HandleFunc("GET /api/network/scan", s.handleScanWifi)

	// SPA fallback for everything else (non-/api, non-/health).
	mux.HandleFunc("/", s.handleStatic)

	return s.logging(cors(mux))
}

// ---------------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------------

// allowedOrigin is the packaged Capacitor app's fixed origin (see
// web/capacitor.config.ts: default androidScheme "https", hostname
// "localhost" — same on iOS). The app's fetches to the board's real LAN/AP IP
// are cross-origin from the WebView's point of view even though the request
// itself is same-device, so they need this reflected explicitly; the
// embedded web UI is served by companion-api itself and never sends an
// Origin header for its own same-origin calls.
const allowedOrigin = "https://localhost"

// cors replaces the previous wildcard CORSMiddleware port: only the packaged
// app's origin is granted cross-origin access, so an arbitrary third-party
// site loaded in a browser on the same LAN/AP can no longer read this API
// from a visitor's tab. Preflight OPTIONS requests are still short-circuited
// for every origin; requests from a non-allowed origin simply get no
// Access-Control-* headers, which the browser then blocks itself.
func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin == allowedOrigin {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Allow-Methods", "*")
			h.Set("Access-Control-Allow-Headers", "*")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// logging mirrors request_logging_middleware: track active requests and log
// start/end with duration.
func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		reqID := time.Now().Format("20060102150405.000000") // unique-ish per request
		client := r.RemoteAddr

		s.reqMu.Lock()
		s.activeRequests[reqID] = map[string]any{
			"id":         reqID,
			"method":     r.Method,
			"path":       r.URL.Path,
			"client":     client,
			"started_at": started.Unix(),
		}
		s.reqMu.Unlock()

		slog.Info("request-start", "id", reqID, "method", r.Method, "path", r.URL.Path, "client", client)

		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)

		elapsedMs := time.Since(started).Milliseconds()
		slog.Info("request-end", "id", reqID, "method", r.Method, "path", r.URL.Path,
			"status", sw.status, "duration_ms", elapsedMs)

		s.reqMu.Lock()
		delete(s.activeRequests, reqID)
		s.reqMu.Unlock()
	})
}

// statusWriter captures the response status code and supports http.Flusher for
// streaming endpoints.
type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, detail string) {
	writeJSON(w, status, map[string]any{"detail": detail})
}

// ---------------------------------------------------------------------------
// Health & recorder
// ---------------------------------------------------------------------------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	hostname, _ := os.Hostname()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"service":  "equip-1-companion-api",
		"hostname": hostname,
	})
}

func (s *Server) recorderPayload() map[string]any {
	return map[string]any{
		"mode":             s.recorder.Mode(),
		"elapsed_seconds":  s.recorder.ElapsedSeconds(),
		"current_file":     nullableString(s.recorder.CurrentFile()),
		"last_stop_reason": nullableString(s.recorder.LastStopReason()),
		"last_stop_at":     nullableUnix(s.recorder.LastStopAt()),
	}
}

// writeRecordActionError maps a recorder.Start/Toggle error to a status
// code: a concurrent start in flight is a 409 (the caller should back off
// and re-poll status, not treat it as a hard failure), everything else stays
// the existing 503.
func writeRecordActionError(w http.ResponseWriter, logMsg string, err error) {
	if errors.Is(err, recorder.ErrAlreadyStarting) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	slog.Warn(logMsg, "error", err)
	writeError(w, http.StatusServiceUnavailable, err.Error())
}

func (s *Server) handleRecordToggle(w http.ResponseWriter, r *http.Request) {
	if err := s.recorder.Toggle(); err != nil {
		writeRecordActionError(w, "record-toggle-failed", err)
		return
	}
	writeJSON(w, http.StatusOK, s.recorderPayload())
}

func (s *Server) handleRecordStart(w http.ResponseWriter, r *http.Request) {
	if err := s.recorder.Start(); err != nil {
		writeRecordActionError(w, "record-start-failed", err)
		return
	}
	writeJSON(w, http.StatusOK, s.recorderPayload())
}

func (s *Server) handleRecordStop(w http.ResponseWriter, r *http.Request) {
	s.recorder.Stop()
	writeJSON(w, http.StatusOK, s.recorderPayload())
}

// ---------------------------------------------------------------------------
// Config — capture mode
// ---------------------------------------------------------------------------

func (s *Server) handleGetCaptureMode(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"current_mode":       s.captureMode.Get(),
		"available_modes":    []string{"dvgrab", "ffmpeg-only"},
		"recorder_is_active": s.recorder.IsRecording(),
	})
}

func (s *Server) handleSetCaptureMode(w http.ResponseWriter, r *http.Request) {
	if s.recorder.IsRecording() {
		writeError(w, http.StatusConflict,
			"Cannot change recording mode while recording is active. Stop recording first.")
		return
	}

	var body struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "Missing 'mode' field in request body")
		return
	}
	if body.Mode == "" {
		writeError(w, http.StatusBadRequest, "Missing 'mode' field in request body")
		return
	}

	if err := s.captureMode.Set(body.Mode); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.resetStreamWorkersForModeChange(body.Mode)

	writeJSON(w, http.StatusOK, map[string]any{
		"current_mode":              s.captureMode.Get(),
		"available_modes":           []string{"dvgrab", "ffmpeg-only"},
		"stream_reconnect_required": true,
		"message":                   "Recording capture mode changed to " + body.Mode,
	})
}

// resetStreamWorkersForModeChange mirrors _reset_stream_workers_for_mode_change.
func (s *Server) resetStreamWorkersForModeChange(newMode string) {
	slog.Info("capture-mode-switch-reset", "mode", newMode)
	s.recorder.StopMjpegFanout()
	s.broadcaster.Stop()
	s.preview.Stop()
	s.directMjpeg.StopAll()
	s.seamless.Stop()
}

// ---------------------------------------------------------------------------
// Files & storage
// ---------------------------------------------------------------------------

func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"capture_dir": s.files.Dir(),
		"items":       s.files.List(100, s.recorder.CurrentFile()),
	})
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	path, err := s.files.Resolve(name)
	if err != nil {
		s.writeFileError(w, err)
		return
	}
	if info, statErr := os.Stat(path); statErr == nil {
		slog.Info("file-download", "name", name, "size", info.Size())
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+name+"\"")
	http.ServeFile(w, r, path)
}

// handleThumbnail serves a cached/lazily-generated JPEG frame grab for a
// capture file. Generation can take a couple seconds on first request; the
// result is cached on disk so subsequent loads are instant.
func (s *Server) handleThumbnail(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	path, err := s.files.Thumbnail(name)
	if err != nil {
		s.writeFileError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeFile(w, r, path)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.files.Delete(name, s.recorder.CurrentFile()); err != nil {
		s.writeFileError(w, err)
		return
	}
	slog.Info("file-delete", "name", name)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": name})
}

func (s *Server) writeFileError(w http.ResponseWriter, err error) {
	switch e := err.(type) {
	case *files.ErrInvalidName:
		writeError(w, http.StatusBadRequest, e.Detail)
	case *files.ErrNotFound:
		writeError(w, http.StatusNotFound, e.Error())
	case *files.ErrActiveRecording:
		writeError(w, http.StatusConflict, e.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

func (s *Server) handleStorage(w http.ResponseWriter, r *http.Request) {
	stats, err := s.files.Stats()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

// ---------------------------------------------------------------------------
// Debug & system
// ---------------------------------------------------------------------------

func (s *Server) handleDebugRuntime(w http.ResponseWriter, r *http.Request) {
	s.reqMu.Lock()
	active := make([]map[string]any, 0, len(s.activeRequests))
	for _, req := range s.activeRequests {
		active = append(active, req)
	}
	s.reqMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"active_request_count":      len(active),
		"active_requests":           active,
		"mediamtx_running":          s.mediamtx.IsRunning(),
		"mjpeg_broadcaster_running": s.broadcaster.IsRunning(),
		"mjpeg_subscriber_count":    s.broadcaster.SubscriberCount(),
		"preview_push_alive":        s.preview.IsAlive(),
		"recorder_mode":             s.recorder.Mode(),
		"recorder_dvgrab_pid":       nullableInt(s.recorder.DvgrabPid()),
		"recorder_mux_pid":          nullableInt(s.recorder.MuxPid()),
		"recorder_current_file":     nullableString(s.recorder.CurrentFile()),
		"capture_mode":              s.captureMode.Get(),
	})
}

func (s *Server) handleSystemPower(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, sysinfo.Power())
}

// handleSystemRestart restarts companion-net (BLE/WiFi daemon) and mediamtx
// via systemd — a remote fix for "BLE stopped advertising" or "stream is
// wedged" without needing SSH. Deliberately does NOT restart companion-api
// itself: this handler is running inside that process, and self-restarting
// mid-request is a needless race to get right when force-quitting the app
// already gives the user an equivalent "start over" for the API/UI side.
func (s *Server) handleSystemRestart(w http.ResponseWriter, r *http.Request) {
	cmd := exec.Command("systemctl", "restart", "companion-net", "mediamtx")
	if err := cmd.Run(); err != nil {
		slog.Error("system-restart-failed", "error", err)
		writeError(w, http.StatusInternalServerError, "Restart failed: "+err.Error())
		return
	}
	slog.Info("system-restart", "services", "companion-net,mediamtx")
	writeJSON(w, http.StatusOK, map[string]any{"restarted": []string{"companion-net", "mediamtx"}})
}

// nullableString returns nil for empty strings so JSON encodes null (matching
// Python's None for unset fields).
func nullableString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func nullableInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}

// nullableUnix returns nil for a zero time.Time, else its Unix seconds.
func nullableUnix(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.Unix()
}
