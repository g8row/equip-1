import os
import subprocess
import threading
from typing import Optional

from logging_setup import get_logger

logger = get_logger()

_FFMPEG_ENCODER_LOCK = threading.Lock()
_SELECTED_RTSP_VIDEO_ENCODER: Optional[str] = None


def _ffmpeg_has_encoder(encoder_name: str) -> bool:
    """Return True if the local ffmpeg build exposes a specific encoder."""
    try:
        result = subprocess.run(
            ["ffmpeg", "-hide_banner", "-encoders"],
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
            timeout=5,
            check=False,
        )
        output = result.stdout or ""
        return f" {encoder_name}" in output
    except Exception:
        return False


def _ffmpeg_encoder_is_usable(encoder_name: str) -> bool:
    """Return True if ffmpeg can actually initialize the encoder on this device."""
    try:
        result = subprocess.run(
            [
                "ffmpeg",
                "-hide_banner",
                "-loglevel",
                "error",
                "-f",
                "lavfi",
                "-i",
                "testsrc=size=320x240:rate=5",
                "-frames:v",
                "1",
                "-an",
                "-c:v",
                encoder_name,
                "-f",
                "null",
                "-",
            ],
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
            timeout=8,
            check=False,
        )
        if result.returncode == 0:
            return True
        output = (result.stdout or "").strip().replace("\n", " | ")
        logger.warning("ffmpeg-encoder-unusable encoder=%s output=%s", encoder_name, output)
        return False
    except Exception as error:
        logger.warning("ffmpeg-encoder-probe-failed encoder=%s error=%s", encoder_name, error)
        return False


def _is_webrtc_compatible_encoder(encoder_name: str) -> bool:
    return encoder_name in {"h264_rkmpp", "h264_v4l2m2m", "h264_nvenc", "h264_vaapi", "libx264"}


def _select_rtsp_video_encoder() -> str:
    """Pick an available and usable RTSP video encoder.

    Priority:
      1) H.264 hardware/software encoders (enables WHEP/WebRTC)
      2) MJPEG fallback (keeps MJPEG stream alive without FIFO)
    """
    global _SELECTED_RTSP_VIDEO_ENCODER
    with _FFMPEG_ENCODER_LOCK:
        if _SELECTED_RTSP_VIDEO_ENCODER is not None:
            return _SELECTED_RTSP_VIDEO_ENCODER

        candidates = ["h264_rkmpp", "h264_v4l2m2m", "h264_nvenc", "h264_vaapi", "libx264", "mjpeg"]

        preferred = os.environ.get("EQUIP_FFMPEG_RTSP_VIDEO_ENCODER", "").strip() or os.environ.get(
            "EQUIP_FFMPEG_H264_ENCODER", ""
        ).strip()
        if preferred:
            candidates = [preferred] + [candidate for candidate in candidates if candidate != preferred]

        for encoder in candidates:
            if not _ffmpeg_has_encoder(encoder):
                continue
            if not _ffmpeg_encoder_is_usable(encoder):
                continue
            _SELECTED_RTSP_VIDEO_ENCODER = encoder
            logger.info(
                "ffmpeg-rtsp-video-encoder-selected encoder=%s webrtc_compatible=%s",
                encoder,
                _is_webrtc_compatible_encoder(encoder),
            )
            return encoder

    raise RuntimeError(
        "No usable RTSP video encoder found. Tried: h264_rkmpp, h264_v4l2m2m, h264_nvenc, h264_vaapi, libx264, mjpeg"
    )


def _build_rtsp_video_output_args(rtsp_url: str) -> list[str]:
    """Build RTSP output args for the selected encoder without using FIFO."""
    encoder = _select_rtsp_video_encoder()

    args = ["-c:v", encoder]

    if encoder == "libx264":
        args.extend(
            [
                "-preset",
                "ultrafast",
                "-tune",
                "zerolatency",
                "-x264-params",
                "keyint=5:min-keyint=5:scenecut=0:bframes=0",
            ]
        )
    elif _is_webrtc_compatible_encoder(encoder):
        args.extend(["-g", "5", "-bf", "0"])
    else:
        args.extend(["-q:v", "5", "-an"])

    if _is_webrtc_compatible_encoder(encoder):
        args.extend(["-c:a", "aac", "-b:a", "128k", "-ar", "44100"])

    args.extend(["-f", "rtsp", "-rtsp_transport", "tcp", rtsp_url])
    return args


def _safe_selected_rtsp_encoder() -> Optional[str]:
    try:
        return _select_rtsp_video_encoder()
    except Exception as error:
        logger.warning("ffmpeg-rtsp-video-encoder-unavailable error=%s", error)
        return None
