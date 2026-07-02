package capture

import (
	"reflect"
	"testing"
)

// These are golden tests: the exact argv the appliance feeds to ffmpeg/dvgrab
// matters (it mirrors the Python originals and the encoder/mux behaviour the
// board depends on). Asserting the full slices catches accidental drift in
// ordering, flags, or the shared ffmpeg prefix.

const rtspURL = "rtsp://127.0.0.1:8554/live"

// ffmpeg prefix shared by every pipeline (see ffmpegBase).
var base = []string{
	"ffmpeg", "-hide_banner", "-loglevel", "error",
	"-fflags", "nobuffer", "-flags", "low_delay",
}

func join(parts ...[]string) []string {
	var out []string
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func TestDvgrabArgs(t *testing.T) {
	if got, want := DvgrabArgs(), []string{"dvgrab", "--format", "raw", "-"}; !reflect.DeepEqual(got, want) {
		t.Errorf("DvgrabArgs() = %v, want %v", got, want)
	}
}

func TestFFmpegArgBuilders(t *testing.T) {
	rtsp := []string{"-c:v", "libx264", "-f", "rtsp", rtspURL}
	mjpegMapped := []string{
		"-map", "0:v", "-vf", "fps=10,scale=960:-1", "-q:v", "5",
		"-f", "mpjpeg", "-flush_packets", "1", "pipe:1",
	}
	mjpegPlain := []string{
		"-vf", "fps=10,scale=960:-1", "-q:v", "5",
		"-f", "mpjpeg", "-flush_packets", "1", "pipe:1",
	}

	tests := []struct {
		name string
		got  []string
		want []string
	}{
		{
			name: "seamless: dv in, rtsp + mapped mjpeg fanout",
			got:  SeamlessFFmpegArgs(rtsp),
			want: join(base, []string{"-f", "dv", "-i", "pipe:0"}, rtsp, mjpegMapped),
		},
		{
			name: "preview dvgrab: dv in, rtsp out",
			got:  PreviewDvgrabFFmpegArgs(rtsp),
			want: join(base, []string{"-f", "dv", "-i", "pipe:0"}, rtsp),
		},
		{
			name: "preview iec61883: kernel input, rtsp out",
			got:  PreviewIec61883FFmpegArgs(rtsp),
			want: join(base, []string{"-f", "iec61883", "-i", "auto"}, rtsp),
		},
		{
			name: "direct mjpeg dvgrab: dv in, plain mjpeg (no map)",
			got:  DirectMjpegDvgrabFFmpegArgs(),
			want: join(base, []string{"-f", "dv", "-i", "pipe:0"}, mjpegPlain),
		},
		{
			name: "direct mjpeg iec61883: kernel input, plain mjpeg",
			got:  DirectMjpegIec61883FFmpegArgs(),
			want: join(base, []string{"-f", "iec61883", "-i", "auto"}, mjpegPlain),
		},
		{
			name: "broadcaster: rtsp in (tcp), mjpeg out at 15fps",
			got:  BroadcasterFFmpegArgs(rtspURL),
			want: join(base, []string{
				"-rtsp_transport", "tcp", "-i", rtspURL,
				"-vf", "fps=15,scale=960:-1", "-q:v", "5",
				"-f", "mpjpeg", "-flush_packets", "1", "pipe:1",
			}),
		},
		{
			name: "recorder mjpeg live output = mapped mjpeg",
			got:  RecorderMjpegLiveOutputArgs(),
			want: mjpegMapped,
		},
		{
			name: "recorder dvgrab mux: lossless dv copy + rtsp + mjpeg",
			got:  RecorderDvgrabMuxArgs("/rec/out.dv", rtsp, mjpegMapped),
			want: join(base, []string{"-f", "dv", "-i", "pipe:0", "-c", "copy", "-f", "dv", "/rec/out.dv"}, rtsp, mjpegMapped),
		},
		{
			name: "recorder iec61883 mux: lossless dv copy, no extra outputs",
			got:  RecorderIec61883MuxArgs("/rec/out.dv", nil, nil),
			want: join(base, []string{"-f", "iec61883", "-i", "auto", "-c", "copy", "-f", "dv", "/rec/out.dv"}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !reflect.DeepEqual(tt.got, tt.want) {
				t.Errorf("\n got: %v\nwant: %v", tt.got, tt.want)
			}
		})
	}
}
