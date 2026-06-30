// Package capture builds the dvgrab/ffmpeg/iec61883 argv slices shared between
// the stream and recorder packages, so command construction lives in one
// testable place. The exact argument ordering mirrors the Python originals.
package capture

// ffmpegBase is the common ffmpeg prefix used by every pipeline.
func ffmpegBase() []string {
	return []string{
		"ffmpeg",
		"-hide_banner",
		"-loglevel", "error",
		"-fflags", "nobuffer",
		"-flags", "low_delay",
	}
}

// mjpegPipeOutput is the mpjpeg output stage written to pipe:1. withMap adds the
// "-map 0:v" selector used by the seamless hub and recorder live-fanout paths.
func mjpegPipeOutput(withMap bool) []string {
	var args []string
	if withMap {
		args = append(args, "-map", "0:v")
	}
	args = append(args,
		"-vf", "fps=10,scale=960:-1",
		"-q:v", "5",
		"-f", "mpjpeg",
		"-flush_packets", "1",
		"pipe:1",
	)
	return args
}

// DvgrabArgs returns the dvgrab raw-DV-to-stdout command.
func DvgrabArgs() []string {
	return []string{"dvgrab", "--format", "raw", "-"}
}

// SeamlessFFmpegArgs builds the seamless hub ffmpeg: DV from pipe:0 → RTSP
// (rtspArgs) plus a second mpjpeg output on pipe:1.
func SeamlessFFmpegArgs(rtspArgs []string) []string {
	args := append(ffmpegBase(), "-f", "dv", "-i", "pipe:0")
	args = append(args, rtspArgs...)
	args = append(args, mjpegPipeOutput(true)...)
	return args
}

// PreviewDvgrabFFmpegArgs builds preview ffmpeg consuming DV on pipe:0 → RTSP.
func PreviewDvgrabFFmpegArgs(rtspArgs []string) []string {
	args := append(ffmpegBase(), "-f", "dv", "-i", "pipe:0")
	return append(args, rtspArgs...)
}

// PreviewIec61883FFmpegArgs builds preview ffmpeg using the iec61883 kernel
// input → RTSP.
func PreviewIec61883FFmpegArgs(rtspArgs []string) []string {
	args := append(ffmpegBase(), "-f", "iec61883", "-i", "auto")
	return append(args, rtspArgs...)
}

// DirectMjpegDvgrabFFmpegArgs builds the no-RTSP direct MJPEG ffmpeg consuming
// DV on pipe:0.
func DirectMjpegDvgrabFFmpegArgs() []string {
	args := append(ffmpegBase(), "-f", "dv", "-i", "pipe:0")
	return append(args, mjpegPipeOutput(false)...)
}

// DirectMjpegIec61883FFmpegArgs builds the no-RTSP direct MJPEG ffmpeg using the
// iec61883 kernel input.
func DirectMjpegIec61883FFmpegArgs() []string {
	args := append(ffmpegBase(), "-f", "iec61883", "-i", "auto")
	return append(args, mjpegPipeOutput(false)...)
}

// BroadcasterFFmpegArgs builds the RTSP→MJPEG broadcaster ffmpeg (15fps).
func BroadcasterFFmpegArgs(rtspURL string) []string {
	args := append(ffmpegBase(),
		"-rtsp_transport", "tcp",
		"-i", rtspURL,
		"-vf", "fps=15,scale=960:-1",
		"-q:v", "5",
		"-f", "mpjpeg",
		"-flush_packets", "1",
		"pipe:1",
	)
	return args
}

// RecorderMjpegLiveOutputArgs returns the mpjpeg live-fanout output appended to
// the recorder mux when no RTSP output is enabled.
func RecorderMjpegLiveOutputArgs() []string {
	return mjpegPipeOutput(true)
}

// RecorderDvgrabMuxArgs builds the ffmpeg-only-mode dvgrab mux: lossless DV to
// disk plus optional rtspOutputArgs and mjpegLiveArgs.
func RecorderDvgrabMuxArgs(outputPath string, rtspOutputArgs, mjpegLiveArgs []string) []string {
	args := append(ffmpegBase(),
		"-f", "dv", "-i", "pipe:0",
		"-c", "copy", "-f", "dv", outputPath,
	)
	args = append(args, rtspOutputArgs...)
	args = append(args, mjpegLiveArgs...)
	return args
}

// RecorderIec61883MuxArgs builds the ffmpeg-only-mode iec61883 mux: lossless DV
// to disk plus optional rtspOutputArgs and mjpegLiveArgs.
func RecorderIec61883MuxArgs(outputPath string, rtspOutputArgs, mjpegLiveArgs []string) []string {
	args := append(ffmpegBase(),
		"-f", "iec61883", "-i", "auto",
		"-c", "copy", "-f", "dv", outputPath,
	)
	args = append(args, rtspOutputArgs...)
	args = append(args, mjpegLiveArgs...)
	return args
}
