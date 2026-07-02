// Package config loads environment-driven configuration for the companion,
// mirroring the original Python api/config.py and api/config_state.py.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

// Config holds static, load-time configuration. Runtime-mutable capture mode
// lives in CaptureMode (see capture_mode.go).
type Config struct {
	CaptureDir       string
	MediamtxBinary   string
	MediamtxConfig   string // path to mediamtx.yml; empty = spawn with mediamtx's defaults
	MediamtxRTSPURL  string
	MediamtxWHEPPort int
	MediamtxWHEPURL  string
	APIPort          int
	APIBase          string // localhost base the net daemon uses to reach the API
	StartupMode      string // "dvgrab" | "ffmpeg-only"
	PreferredEncoder string
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Load reads configuration from the environment, applying the same defaults as
// the Python implementation and ensuring directories exist.
func Load() *Config {
	home, _ := os.UserHomeDir()
	captureDir := env("EQUIP_CAPTURE_DIR", filepath.Join(home, "captures"))
	_ = os.MkdirAll(captureDir, 0o755)

	whepPort, err := strconv.Atoi(env("EQUIP_MEDIAMTX_WHEP_PORT", "8889"))
	if err != nil {
		whepPort = 8889
	}
	apiPort, err := strconv.Atoi(env("EQUIP_API_PORT", "8000"))
	if err != nil {
		apiPort = 8000
	}

	preferred := os.Getenv("EQUIP_FFMPEG_RTSP_VIDEO_ENCODER")
	if preferred == "" {
		preferred = os.Getenv("EQUIP_FFMPEG_H264_ENCODER")
	}

	startup := env("EQUIP_RECORDING_CAPTURE_MODE", "dvgrab")
	if startup != "dvgrab" && startup != "ffmpeg-only" {
		startup = "dvgrab"
	}

	return &Config{
		CaptureDir:       captureDir,
		MediamtxBinary:   env("EQUIP_MEDIAMTX_BINARY", "mediamtx"),
		MediamtxConfig:   os.Getenv("EQUIP_MEDIAMTX_CONFIG"),
		MediamtxRTSPURL:  env("EQUIP_MEDIAMTX_RTSP_URL", "rtsp://127.0.0.1:8554/live"),
		MediamtxWHEPPort: whepPort,
		MediamtxWHEPURL:  fmt.Sprintf("http://127.0.0.1:%d/live/whep", whepPort),
		APIPort:          apiPort,
		APIBase:          fmt.Sprintf("http://127.0.0.1:%d", apiPort),
		StartupMode:      startup,
		PreferredEncoder: preferred,
	}
}

// ValidMode reports whether mode is a supported capture mode.
func ValidMode(mode string) bool {
	return mode == "dvgrab" || mode == "ffmpeg-only"
}

// CaptureMode is a thread-safe holder for the runtime-mutable recording capture
// mode, replacing the Python ConfigState dataclass.
type CaptureMode struct {
	mu   sync.RWMutex
	mode string
}

// NewCaptureMode returns a CaptureMode initialized to mode (falling back to
// "dvgrab" if mode is invalid).
func NewCaptureMode(mode string) *CaptureMode {
	if !ValidMode(mode) {
		mode = "dvgrab"
	}
	return &CaptureMode{mode: mode}
}

// Get returns the current capture mode.
func (c *CaptureMode) Get() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.mode
}

// Set updates the capture mode, returning an error for invalid values.
func (c *CaptureMode) Set(mode string) error {
	if !ValidMode(mode) {
		return fmt.Errorf("invalid recording capture mode: %s", mode)
	}
	c.mu.Lock()
	c.mode = mode
	c.mu.Unlock()
	return nil
}
