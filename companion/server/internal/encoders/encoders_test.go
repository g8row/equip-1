package encoders

import (
	"reflect"
	"testing"
	"time"
)

func TestBuildArgsForEncoder(t *testing.T) {
	const rtsp = "rtsp://127.0.0.1:8554/live"
	tests := []struct {
		name    string
		encoder string
		want    []string
	}{
		{
			name:    "libx264 zerolatency",
			encoder: "libx264",
			want: []string{
				"-c:v", "libx264",
				"-preset", "ultrafast",
				"-tune", "zerolatency",
				"-x264-params", "keyint=25:min-keyint=25:scenecut=0:bframes=0",
				"-c:a", "aac", "-b:a", "128k", "-ar", "48000",
				"-f", "rtsp", "-rtsp_transport", "tcp", rtsp,
			},
		},
		{
			name:    "hardware h264 webrtc gop",
			encoder: "h264_rkmpp",
			want: []string{
				"-c:v", "h264_rkmpp",
				"-g", "25", "-bf", "0",
				"-c:a", "aac", "-b:a", "128k", "-ar", "48000",
				"-f", "rtsp", "-rtsp_transport", "tcp", rtsp,
			},
		},
		{
			name:    "mjpeg fallback no audio",
			encoder: "mjpeg",
			want: []string{
				"-c:v", "mjpeg",
				"-q:v", "5", "-an",
				"-f", "rtsp", "-rtsp_transport", "tcp", rtsp,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildArgsForEncoder(tt.encoder, rtsp)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("buildArgsForEncoder(%q)\n got: %v\nwant: %v", tt.encoder, got, tt.want)
			}
		})
	}
}

// resetEncoderCache clears the package-level probe cache so a test starts
// from a known state and doesn't leak into whichever test runs next.
func resetEncoderCache(t *testing.T) {
	t.Helper()
	encoderMu.Lock()
	selectedRTSPResolved = false
	selectedRTSPEncoder = ""
	probeFailedAt = time.Time{}
	encoderMu.Unlock()
	t.Cleanup(func() {
		encoderMu.Lock()
		selectedRTSPResolved = false
		selectedRTSPEncoder = ""
		probeFailedAt = time.Time{}
		encoderMu.Unlock()
	})
}

// TestSelectRTSPVideoEncoderCachesNegativeResult verifies T2.4's negative
// cache: a prior full-probe failure within probeFailureTTL is served from
// the cache — the fast path must not touch probeFailedAt again (which would
// mean it re-ran the full candidate probe).
func TestSelectRTSPVideoEncoderCachesNegativeResult(t *testing.T) {
	resetEncoderCache(t)

	encoderMu.Lock()
	probeFailedAt = time.Now()
	stamp := probeFailedAt
	encoderMu.Unlock()

	_, err := SelectRTSPVideoEncoder()
	if _, ok := err.(*NoEncoderError); !ok {
		t.Fatalf("expected a cached NoEncoderError, got %v", err)
	}

	encoderMu.Lock()
	after := probeFailedAt
	encoderMu.Unlock()
	if !after.Equal(stamp) {
		t.Fatalf("probeFailedAt changed (%v -> %v): the cached-negative path re-ran the probe", stamp, after)
	}
}

// TestSelectRTSPVideoEncoderReprobesAfterTTL verifies a probe failure older
// than probeFailureTTL is NOT served from the cache: a fresh probe runs and
// (since it fails again in this ffmpeg-less test environment) records a new,
// later probeFailedAt.
func TestSelectRTSPVideoEncoderReprobesAfterTTL(t *testing.T) {
	resetEncoderCache(t)

	stale := time.Now().Add(-2 * probeFailureTTL)
	encoderMu.Lock()
	probeFailedAt = stale
	encoderMu.Unlock()

	if _, err := SelectRTSPVideoEncoder(); err == nil {
		t.Skip("a usable encoder was found in this environment; cache-expiry re-probe can't be observed this way")
	}

	encoderMu.Lock()
	after := probeFailedAt
	encoderMu.Unlock()
	if !after.After(stale) {
		t.Fatalf("expected a fresh probe past the TTL to advance probeFailedAt (was %v, still %v)", stale, after)
	}
}

func TestIsWebRTCCompatible(t *testing.T) {
	yes := []string{"h264_rkmpp", "h264_v4l2m2m", "h264_nvenc", "h264_vaapi", "libx264"}
	for _, e := range yes {
		if !IsWebRTCCompatible(e) {
			t.Errorf("expected %q to be webrtc-compatible", e)
		}
	}
	if IsWebRTCCompatible("mjpeg") {
		t.Errorf("mjpeg must not be webrtc-compatible")
	}
}
