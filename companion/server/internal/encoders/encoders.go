// Package encoders selects and builds ffmpeg RTSP video output arguments,
// porting api/encoders.py with high fidelity (candidate priority, webrtc
// compatibility set, preferred-override env vars, probing, caching and arg
// construction are reproduced exactly).
package encoders

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// candidatePriority is the fixed candidate list. H.264 encoders (which enable
// WHEP/WebRTC) come first; mjpeg is the FIFO-free fallback.
var candidatePriority = []string{
	"h264_rkmpp",
	"h264_v4l2m2m",
	"h264_nvenc",
	"h264_vaapi",
	"libx264",
	"mjpeg",
}

// webrtcCompatible is the set of encoders that mediamtx can republish over
// WebRTC/WHEP.
var webrtcCompatible = map[string]bool{
	"h264_rkmpp":   true,
	"h264_v4l2m2m": true,
	"h264_nvenc":   true,
	"h264_vaapi":   true,
	"libx264":      true,
}

var (
	encoderMu            sync.Mutex
	selectedRTSPEncoder  string
	selectedRTSPResolved bool
)

// IsWebRTCCompatible reports whether the encoder can be republished over WebRTC.
func IsWebRTCCompatible(encoder string) bool {
	return webrtcCompatible[encoder]
}

// ffmpegHasEncoder returns true if the local ffmpeg build exposes the named
// encoder. Mirrors _ffmpeg_has_encoder: parse `ffmpeg -hide_banner -encoders`
// and look for " <name>" in the combined output.
func ffmpegHasEncoder(name string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-encoders")
	out, err := cmd.CombinedOutput()
	if err != nil && len(out) == 0 {
		return false
	}
	return strings.Contains(string(out), " "+name)
}

// ffmpegEncoderIsUsable returns true if ffmpeg can actually initialize the
// encoder via a one-frame testsrc encode. Mirrors _ffmpeg_encoder_is_usable.
func ffmpegEncoderIsUsable(name string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner",
		"-loglevel", "error",
		"-f", "lavfi",
		"-i", "testsrc=size=320x240:rate=5",
		"-frames:v", "1",
		"-an",
		"-c:v", name,
		"-f", "null",
		"-",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return true
	}
	output := strings.ReplaceAll(strings.TrimSpace(string(out)), "\n", " | ")
	slog.Warn("ffmpeg-encoder-unusable", "encoder", name, "output", output)
	return false
}

// SelectRTSPVideoEncoder picks an available and usable RTSP video encoder,
// caching the result. Mirrors _select_rtsp_video_encoder. Returns an error when
// no candidate is usable.
func SelectRTSPVideoEncoder() (string, error) {
	encoderMu.Lock()
	defer encoderMu.Unlock()

	if selectedRTSPResolved {
		return selectedRTSPEncoder, nil
	}

	candidates := append([]string(nil), candidatePriority...)

	preferred := strings.TrimSpace(os.Getenv("EQUIP_FFMPEG_RTSP_VIDEO_ENCODER"))
	if preferred == "" {
		preferred = strings.TrimSpace(os.Getenv("EQUIP_FFMPEG_H264_ENCODER"))
	}
	if preferred != "" {
		reordered := []string{preferred}
		for _, c := range candidates {
			if c != preferred {
				reordered = append(reordered, c)
			}
		}
		candidates = reordered
	}

	for _, encoder := range candidates {
		if !ffmpegHasEncoder(encoder) {
			continue
		}
		if !ffmpegEncoderIsUsable(encoder) {
			continue
		}
		selectedRTSPEncoder = encoder
		selectedRTSPResolved = true
		slog.Info("ffmpeg-rtsp-video-encoder-selected",
			"encoder", encoder,
			"webrtc_compatible", IsWebRTCCompatible(encoder))
		return encoder, nil
	}

	return "", &NoEncoderError{}
}

// NoEncoderError indicates no usable RTSP video encoder was found.
type NoEncoderError struct{}

func (e *NoEncoderError) Error() string {
	return "No usable RTSP video encoder found. Tried: h264_rkmpp, h264_v4l2m2m, h264_nvenc, h264_vaapi, libx264, mjpeg"
}

// SafeSelectedRTSPEncoder returns the selected encoder or "" if none is usable,
// logging a warning instead of returning an error. Mirrors
// _safe_selected_rtsp_encoder.
func SafeSelectedRTSPEncoder() string {
	encoder, err := SelectRTSPVideoEncoder()
	if err != nil {
		slog.Warn("ffmpeg-rtsp-video-encoder-unavailable", "error", err)
		return ""
	}
	return encoder
}

// BuildRTSPVideoOutputArgs builds the ffmpeg RTSP output args for the selected
// encoder, without a FIFO. Mirrors _build_rtsp_video_output_args EXACTLY.
func BuildRTSPVideoOutputArgs(rtspURL string) ([]string, error) {
	encoder, err := SelectRTSPVideoEncoder()
	if err != nil {
		return nil, err
	}
	return buildArgsForEncoder(encoder, rtspURL), nil
}

// buildArgsForEncoder is the pure arg-construction logic, split out for
// table-testing.
func buildArgsForEncoder(encoder, rtspURL string) []string {
	args := []string{"-c:v", encoder}

	switch {
	case encoder == "libx264":
		args = append(args,
			"-preset", "ultrafast",
			"-tune", "zerolatency",
			"-x264-params", "keyint=5:min-keyint=5:scenecut=0:bframes=0",
		)
	case IsWebRTCCompatible(encoder):
		args = append(args, "-g", "5", "-bf", "0")
	default:
		args = append(args, "-q:v", "5", "-an")
	}

	if IsWebRTCCompatible(encoder) {
		args = append(args, "-c:a", "aac", "-b:a", "128k", "-ar", "44100")
	}

	args = append(args, "-f", "rtsp", "-rtsp_transport", "tcp", rtspURL)
	return args
}
