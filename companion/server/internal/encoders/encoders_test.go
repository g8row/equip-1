package encoders

import (
	"reflect"
	"testing"
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
				"-x264-params", "keyint=5:min-keyint=5:scenecut=0:bframes=0",
				"-c:a", "aac", "-b:a", "128k", "-ar", "44100",
				"-f", "rtsp", "-rtsp_transport", "tcp", rtsp,
			},
		},
		{
			name:    "hardware h264 webrtc gop",
			encoder: "h264_rkmpp",
			want: []string{
				"-c:v", "h264_rkmpp",
				"-g", "5", "-bf", "0",
				"-c:a", "aac", "-b:a", "128k", "-ar", "44100",
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
