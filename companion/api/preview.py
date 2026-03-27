"""Live preview streaming to mediamtx."""

import subprocess
import threading
import time
from typing import TYPE_CHECKING, Optional

from config import MEDIAMTX_RTSP_URL
from encoders import _build_rtsp_video_output_args
from logging_setup import get_logger
from process_utils import _spawn_stderr_logger
from process_utils import _terminate_process


if TYPE_CHECKING:
    from config_state import ConfigState
    from managers import MediamtxManager
    from recorder import RecorderState


logger = get_logger()


class PreviewPush:
    """Single dvgrab/iec61883 → ffmpeg → RTSP process for live preview.

    Started lazily when the first MJPEG client connects (or when WebRTC is
    requested) and stopped when recording starts or the API shuts up.
    Only one FireWire source process is ever running at a time.
    """

    # Seconds to wait before retrying after a failure (prevents process storm)
    _RETRY_COOLDOWN = 3.0

    def __init__(
        self,
        config_state: "ConfigState",
        mediamtx_manager: "MediamtxManager",
        recorder_state: "RecorderState",
    ) -> None:
        self._config = config_state
        self._mediamtx = mediamtx_manager
        self._recorder = recorder_state
        self._lock = threading.Lock()
        self._dvgrab: Optional[subprocess.Popen] = None
        self._ffmpeg: Optional[subprocess.Popen] = None
        self._last_failure_ts: float = 0.0

    def ensure_running(self) -> None:
        """Start preview push if not already running and not recording."""
        with self._lock:
            if self._recorder.is_recording:
                return  # Recording owns the bus + mediamtx
            if self._is_alive():
                return

            # Cooldown: if the last attempt failed recently, don't hammer the
            # FireWire bus with new processes — wait for the camera to settle.
            elapsed_since_fail = time.time() - self._last_failure_ts
            if elapsed_since_fail < self._RETRY_COOLDOWN:
                logger.info(
                    "preview-push-cooldown remaining=%.1fs",
                    self._RETRY_COOLDOWN - elapsed_since_fail,
                )
                return

            capture_mode = self._config.get_mode()
            logger.info("preview-push-start capture_mode=%s", capture_mode)

            if not self._mediamtx.is_running():
                self._mediamtx.start()

            rtsp_args = _build_rtsp_video_output_args(MEDIAMTX_RTSP_URL)

            if capture_mode == "dvgrab":
                try:
                    self._dvgrab = subprocess.Popen(
                        ["dvgrab", "--format", "raw", "-"],
                        stdout=subprocess.PIPE,
                        stderr=subprocess.PIPE,
                        bufsize=0,
                        start_new_session=True,
                    )
                    _spawn_stderr_logger(self._dvgrab, "preview-dvgrab")
                except FileNotFoundError:
                    logger.error("preview-push-dvgrab-not-found")
                    return

                try:
                    self._ffmpeg = subprocess.Popen(
                        [
                            "ffmpeg",
                            "-hide_banner",
                            "-loglevel", "error",
                            "-fflags", "nobuffer",
                            "-flags", "low_delay",
                            "-f", "dv",
                            "-i", "pipe:0",
                            *rtsp_args,
                        ],
                        stdin=self._dvgrab.stdout,
                        stdout=subprocess.DEVNULL,
                        stderr=subprocess.PIPE,
                        bufsize=0,
                        start_new_session=True,
                    )
                    _spawn_stderr_logger(self._ffmpeg, "preview-ffmpeg")
                except Exception as e:
                    logger.error("preview-push-ffmpeg-failed error=%s", e)
                    _terminate_process(self._dvgrab)
                    self._dvgrab = None
                    return

                if self._dvgrab.stdout is not None:
                    self._dvgrab.stdout.close()

                # Brief sanity-check: if processes die within 500 ms, camera is absent
                time.sleep(0.5)
                if self._ffmpeg.poll() is not None or self._dvgrab.poll() is not None:
                    logger.error(
                        "preview-push-early-exit dvgrab-rc=%s ffmpeg-rc=%s",
                        self._dvgrab.poll(),
                        self._ffmpeg.poll(),
                    )
                    _terminate_process(self._ffmpeg)
                    _terminate_process(self._dvgrab)
                    self._ffmpeg = None
                    self._dvgrab = None
                    self._last_failure_ts = time.time()
                    return

                logger.info(
                    "preview-push-running dvgrab-pid=%s ffmpeg-pid=%s",
                    self._dvgrab.pid,
                    self._ffmpeg.pid,
                )

            else:  # ffmpeg-only
                try:
                    self._ffmpeg = subprocess.Popen(
                        [
                            "ffmpeg",
                            "-hide_banner",
                            "-loglevel", "error",
                            "-fflags", "nobuffer",
                            "-flags", "low_delay",
                            "-f", "iec61883",
                            "-i", "auto",
                            *rtsp_args,
                        ],
                        stdout=subprocess.DEVNULL,
                        stderr=subprocess.PIPE,
                        bufsize=0,
                        start_new_session=True,
                    )
                    _spawn_stderr_logger(self._ffmpeg, "preview-ffmpeg-direct")
                except Exception as e:
                    logger.error("preview-push-ffmpeg-only-failed error=%s", e)
                    return

                # Brief sanity-check for iec61883 start
                time.sleep(0.5)
                if self._ffmpeg.poll() is not None:
                    logger.error(
                        "preview-push-early-exit ffmpeg-rc=%s (iec61883 unavailable?)",
                        self._ffmpeg.returncode,
                    )
                    self._ffmpeg = None
                    self._last_failure_ts = time.time()
                    return

                logger.info("preview-push-running ffmpeg-pid=%s", self._ffmpeg.pid)

    def stop(self) -> None:
        with self._lock:
            _terminate_process(self._ffmpeg)
            _terminate_process(self._dvgrab)
            self._ffmpeg = None
            self._dvgrab = None
            logger.info("preview-push-stopped")

    def is_alive(self) -> bool:
        with self._lock:
            return self._is_alive()

    def _is_alive(self) -> bool:
        return self._ffmpeg is not None and self._ffmpeg.poll() is None
