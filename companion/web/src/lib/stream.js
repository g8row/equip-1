export function streamIssue(status, mode) {
  const stream = status?.stream;
  if (!stream) return null;

  const requirements = stream.requirements ?? {};
  if (requirements.ffmpeg === false) return "ffmpeg is not installed on the device.";
  if (requirements.camera_present === false) {
    return "No FireWire camera node was detected. Check camera power and cable.";
  }

  if (mode === "webrtc") {
    if (requirements.mediamtx === false) return "MediaMTX is not installed on the device.";
    if (stream.mediamtx_running === false) return "MediaMTX is not running.";
    if (stream.rtsp_video_encoder == null) return "No usable RTSP video encoder was found.";
    if (stream.whep_available === false) {
      return `WebRTC is unavailable with encoder ${stream.rtsp_video_encoder}. Use MJPEG instead.`;
    }
  }

  return null;
}

// Raw browser network-error text ("Failed to fetch" in Chromium/WebView,
// "Load failed" in Safari, "NetworkError when attempting to fetch resource"
// in Firefox) is meaningless to a user — it just means the device wasn't
// reachable at all, not an in-app stream problem. Catch it before any of
// the more specific pattern checks below.
const GENERIC_NETWORK_ERROR = /failed to fetch|load failed|networkerror/i;

export function describeStreamFailure(mode, message = "") {
  const text = String(message || "").trim();
  if (GENERIC_NETWORK_ERROR.test(text)) {
    return "Can't reach the device — check it's powered on and on the same network.";
  }
  if (text.includes("No AV/C devices found") || text.includes("no camera exists")) {
    return "No AV/C camera is responding. Power-cycle the DV camera and check the FireWire cable.";
  }
  if (text.includes("No usable RTSP video encoder")) {
    return "No usable RTSP video encoder was found. MJPEG may still work.";
  }
  if (text.includes("WebRTC unavailable")) {
    return "WebRTC is unavailable with the selected encoder. Switch to MJPEG.";
  }
  if (text.includes("Cannot reach mediamtx") || text.includes("mediamtx is not running")) {
    return "MediaMTX is not reachable. Restart the companion API or MediaMTX.";
  }
  if (mode === "mjpeg") {
    return "MJPEG produced no frames. Check camera power/cable, then restart the stream.";
  }
  return text || "The stream could not start.";
}

export async function responseDetail(response) {
  const text = await response.text().catch(() => "");
  if (!text) return response.statusText || `HTTP ${response.status}`;
  try {
    const body = JSON.parse(text);
    return body.detail || text;
  } catch {
    return text;
  }
}
