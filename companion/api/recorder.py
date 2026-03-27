"""Recording state management for equip-1 companion."""

import glob
import queue
import shutil
import subprocess
import threading
import time
import typing
from dataclasses import dataclass
from pathlib import Path
from typing import TYPE_CHECKING, Callable, Optional

from config import CAPTURE_DIR
from config import MEDIAMTX_BINARY
from config import MEDIAMTX_RTSP_URL
from encoders import _build_rtsp_video_output_args
from encoders import _is_webrtc_compatible_encoder
from encoders import _safe_selected_rtsp_encoder
from logging_setup import get_logger
from process_utils import _spawn_stderr_logger
from process_utils import _terminate_process


if TYPE_CHECKING:
    from config_state import ConfigState
    from managers import MediamtxManager
    from managers import SeamlessDvHub
    from preview import PreviewPush


logger = get_logger()

# Recording MJPEG fanout thread state
_MJPEG_CHUNK_SIZE = 8192
_RECORDING_MJPEG_LOCK = threading.Lock()
_RECORDING_MJPEG_SUBSCRIBERS: dict[int, queue.Queue] = {}
_RECORDING_MJPEG_RUNNING = False
_RECORDING_MJPEG_THREAD: Optional[threading.Thread] = None


def _start_recording_mjpeg_fanout(ffmpeg_process: subprocess.Popen) -> None:
    """Start fanout thread reading recording ffmpeg stdout and broadcasting chunks."""
    global _RECORDING_MJPEG_RUNNING, _RECORDING_MJPEG_THREAD
    if ffmpeg_process.stdout is None:
        return

    with _RECORDING_MJPEG_LOCK:
        _RECORDING_MJPEG_RUNNING = True

    def _reader() -> None:
        global _RECORDING_MJPEG_RUNNING
        logger.info("record-mjpeg-fanout-start ffmpeg_pid=%s", ffmpeg_process.pid)
        try:
            while True:
                if ffmpeg_process.poll() is not None:
                    logger.info("record-mjpeg-fanout-ffmpeg-exit rc=%s", ffmpeg_process.returncode)
                    break
                chunk = ffmpeg_process.stdout.read(_MJPEG_CHUNK_SIZE)
                if not chunk:
                    logger.info("record-mjpeg-fanout-eof")
                    break
                with _RECORDING_MJPEG_LOCK:
                    queues = list(_RECORDING_MJPEG_SUBSCRIBERS.values())
                for q in queues:
                    try:
                        q.put_nowait(chunk)
                    except queue.Full:
                        pass
        finally:
            with _RECORDING_MJPEG_LOCK:
                for q in _RECORDING_MJPEG_SUBSCRIBERS.values():
                    try:
                        q.put_nowait(None)
                    except queue.Full:
                        pass
                _RECORDING_MJPEG_SUBSCRIBERS.clear()
                _RECORDING_MJPEG_RUNNING = False
            logger.info("record-mjpeg-fanout-stop")

    _RECORDING_MJPEG_THREAD = threading.Thread(target=_reader, name="record-mjpeg-fanout", daemon=True)
    _RECORDING_MJPEG_THREAD.start()


def _stop_recording_mjpeg_fanout() -> None:
    global _RECORDING_MJPEG_RUNNING
    with _RECORDING_MJPEG_LOCK:
        for q in _RECORDING_MJPEG_SUBSCRIBERS.values():
            try:
                q.put_nowait(None)
            except queue.Full:
                pass
        _RECORDING_MJPEG_SUBSCRIBERS.clear()
        _RECORDING_MJPEG_RUNNING = False


@dataclass
class RecorderState:
    """Manages the recording pipeline.

    dvgrab mode:
        dvgrab --format raw -
          → ffmpeg  [output 1] -c copy -f dv  capture.dv     (lossless disk)
                    [output 2] -c:v libx264 … -f rtsp …/live  (mediamtx → WebRTC)

    ffmpeg-only mode:
        ffmpeg -f iec61883 -i auto
                    [output 1] -c copy -f dv  capture.dv     (lossless disk)
                    [output 2] -c:v libx264 … -f rtsp …/live  (mediamtx → WebRTC)
    """

    config: "ConfigState"
    mediamtx: "MediamtxManager"
    seamless_hub: "SeamlessDvHub"
    preview: "PreviewPush"
    stop_all_direct_mjpeg: Callable[[], None]

    mode: str = "idle"
    start_time: Optional[float] = None
    dvgrab_process: Optional[subprocess.Popen] = None
    mux_process: Optional[subprocess.Popen] = None
    current_file: Optional[str] = None

    def toggle(self) -> None:
        if self.mode == "idle":
            self.start()
        else:
            self.stop()

    def start(self) -> None:
        self.refresh_process_state()
        if self.mode == "recording":
            logger.info("record-start-ignored mode=recording current_file=%s", self.current_file)
            return

        capture_mode = self.config.get_mode()
        logger.info("record-start capture_mode=%s", capture_mode)

        # ---- Requirements check ----------------------------------------
        if shutil.which("ffmpeg") is None:
            raise RuntimeError("ffmpeg is not installed")
        # dvgrab mode requires a FireWire device; ffmpeg-only uses the kernel
        # iec61883 module which can work even without a visible /dev/fw* node
        if capture_mode == "dvgrab":
            fw_nodes = glob.glob("/dev/fw[0-9]*")
            if not fw_nodes:
                raise RuntimeError("Camera not found — no /dev/fw* device present")
            if shutil.which("dvgrab") is None:
                raise RuntimeError("dvgrab is not installed")

        selected_encoder = _safe_selected_rtsp_encoder()
        webrtc_ok = _is_webrtc_compatible_encoder(selected_encoder) if selected_encoder else False

        # dvgrab mode uses one always-on capture hub for stream + recording tap.
        if capture_mode == "dvgrab":
            timestamp = time.strftime("%Y%m%d_%H%M%S")
            output_path = CAPTURE_DIR / f"capture_{timestamp}.dv"
            logger.info("record-start seamless-hub output=%s encoder=%s", output_path, selected_encoder)

            self.seamless_hub.start_recording(output_path)
            self.mode = "recording"
            self.start_time = time.time()
            self.current_file = str(output_path)
            logger.info("record-start complete file=%s", self.current_file)
            return

        # ---- Ensure mediamtx is running --------------------------------
        if not self.mediamtx.is_running():
            if not self.mediamtx.start():
                raise RuntimeError(
                    "mediamtx is not running and could not be started. "
                    "Install from https://github.com/bluenviron/mediamtx/releases "
                    f"and ensure '{MEDIAMTX_BINARY}' is in PATH."
                )

        # ---- Stop any live preview that owns the FireWire bus ----------
        self.preview.stop()
        self.stop_all_direct_mjpeg()
        # Give the bus a moment to release
        time.sleep(0.3)

        # ---- Build file path -------------------------------------------
        timestamp = time.strftime("%Y%m%d_%H%M%S")
        output_path = CAPTURE_DIR / f"capture_{timestamp}.dv"
        logger.info("record-start requested output=%s", output_path)

        # ---- Build ffmpeg command ---------------------------------------
        # Always write lossless DV to disk.
        # Add RTSP output only when a WebRTC-compatible encoder is available.
        enable_rtsp_output = _is_webrtc_compatible_encoder(selected_encoder) if selected_encoder else False
        rtsp_output_args = _build_rtsp_video_output_args(MEDIAMTX_RTSP_URL) if enable_rtsp_output else []
        mjpeg_live_output_args = [
            "-map",
            "0:v",
            "-vf",
            "fps=10,scale=960:-1",
            "-q:v",
            "5",
            "-f",
            "mpjpeg",
            "-flush_packets",
            "1",
            "pipe:1",
        ] if not enable_rtsp_output else []
        logger.info(
            "record-start-stream-path encoder=%s rtsp_enabled=%s",
            selected_encoder,
            enable_rtsp_output,
        )

        if capture_mode == "dvgrab":
            self.dvgrab_process = subprocess.Popen(
                ["dvgrab", "--format", "raw", "-"],
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                bufsize=0,
                start_new_session=True,
            )
            logger.info("record-start dvgrab-pid=%s", self.dvgrab_process.pid)
            _spawn_stderr_logger(self.dvgrab_process, "record-dvgrab")

            self.mux_process = subprocess.Popen(
                [
                    "ffmpeg",
                    "-hide_banner",
                    "-loglevel", "error",
                    "-fflags", "nobuffer",
                    "-flags", "low_delay",
                    "-f", "dv",
                    "-i", "pipe:0",
                    # Output 1: lossless
                    "-c", "copy",
                    "-f", "dv",
                    str(output_path),
                    *rtsp_output_args,
                    *mjpeg_live_output_args,
                ],
                stdin=self.dvgrab_process.stdout,
                stdout=subprocess.PIPE if not enable_rtsp_output else subprocess.DEVNULL,
                stderr=subprocess.PIPE,
                bufsize=0,
                start_new_session=True,
            )
            logger.info("record-start mux-pid=%s rtsp=%s", self.mux_process.pid, MEDIAMTX_RTSP_URL)
            _spawn_stderr_logger(self.mux_process, "record-ffmpeg")

            if self.dvgrab_process.stdout is not None:
                self.dvgrab_process.stdout.close()

        else:  # ffmpeg-only / iec61883
            self.mux_process = subprocess.Popen(
                [
                    "ffmpeg",
                    "-hide_banner",
                    "-loglevel", "error",
                    "-fflags", "nobuffer",
                    "-flags", "low_delay",
                    "-f", "iec61883",
                    "-i", "auto",
                    # Output 1: lossless
                    "-c", "copy",
                    "-f", "dv",
                    str(output_path),
                    *rtsp_output_args,
                    *mjpeg_live_output_args,
                ],
                stdout=subprocess.PIPE if not enable_rtsp_output else subprocess.DEVNULL,
                stderr=subprocess.PIPE,
                bufsize=0,
                start_new_session=True,
            )
            logger.info("record-start ffmpeg-direct-pid=%s rtsp=%s", self.mux_process.pid, MEDIAMTX_RTSP_URL)
            _spawn_stderr_logger(self.mux_process, "record-ffmpeg-direct")

        self.mode = "recording"
        self.start_time = time.time()
        self.current_file = str(output_path)

        if not enable_rtsp_output and self.mux_process is not None:
            _start_recording_mjpeg_fanout(self.mux_process)

        time.sleep(0.2)
        if self.mux_process.poll() is not None:
            rc = self.mux_process.returncode
            self.stop()
            raise RuntimeError(f"ffmpeg mux exited immediately (rc={rc})")
        if capture_mode == "dvgrab" and self.dvgrab_process is not None and self.dvgrab_process.poll() is not None:
            rc = self.dvgrab_process.returncode
            self.stop()
            raise RuntimeError(f"dvgrab exited immediately (rc={rc})")

        logger.info("record-start complete file=%s", self.current_file)

    def stop(self) -> None:
        logger.info("record-stop requested mode=%s file=%s", self.mode, self.current_file)

        capture_mode = self.config.get_mode()

        if capture_mode == "dvgrab":
            self.seamless_hub.stop_recording()
            self.mode = "idle"
            self.start_time = None
            logger.info("record-stop complete")
            return

        self.mode = "idle"
        self.start_time = None
        _stop_recording_mjpeg_fanout()

        _terminate_process(self.mux_process)
        _terminate_process(self.dvgrab_process)

        self.mux_process = None
        self.dvgrab_process = None
        logger.info("record-stop complete")

    def refresh_process_state(self) -> None:
        if self.mode != "recording":
            return

        capture_mode = self.config.get_mode()
        if capture_mode == "dvgrab":
            if not self.seamless_hub.is_running():
                logger.error("record-process-died details=%s", [("seamless-hub", "not-running")])
                self.mode = "idle"
                self.start_time = None
            return

        dead = []
        if self.dvgrab_process is not None and self.dvgrab_process.poll() is not None:
            dead.append(("dvgrab", self.dvgrab_process.returncode))
        if self.mux_process is not None and self.mux_process.poll() is not None:
            dead.append(("ffmpeg-mux", self.mux_process.returncode))

        if dead:
            logger.error("record-process-died details=%s", dead)
            self.stop()

    @property
    def is_recording(self) -> bool:
        return self.mode == "recording"

    @property
    def elapsed_seconds(self) -> int:
        if self.mode != "recording" or self.start_time is None:
            return 0
        return int(time.time() - self.start_time)
