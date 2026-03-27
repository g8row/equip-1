import asyncio
import glob
import os
import queue
import signal
import shutil
import socket
import stat
import subprocess
import threading
import time
import logging
import typing
from logging.handlers import RotatingFileHandler
from dataclasses import dataclass
from pathlib import Path
from typing import Optional

import httpx
from fastapi import FastAPI
from fastapi import HTTPException
from fastapi import Request
from fastapi import Response
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import FileResponse
from fastapi.responses import StreamingResponse


# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------

CAPTURE_DIR = Path(os.environ.get("EQUIP_CAPTURE_DIR", str(Path.home() / "captures")))
CAPTURE_DIR.mkdir(parents=True, exist_ok=True)

# Recording capture mode: "dvgrab" or "ffmpeg-only"
RECORDING_CAPTURE_MODE = os.environ.get("EQUIP_RECORDING_CAPTURE_MODE", "dvgrab")
assert RECORDING_CAPTURE_MODE in ["dvgrab", "ffmpeg-only"], f"Invalid capture mode: {RECORDING_CAPTURE_MODE}"

# mediamtx settings
MEDIAMTX_BINARY = os.environ.get("EQUIP_MEDIAMTX_BINARY", "mediamtx")
MEDIAMTX_RTSP_URL = os.environ.get("EQUIP_MEDIAMTX_RTSP_URL", "rtsp://127.0.0.1:8554/live")
MEDIAMTX_WHEP_PORT = int(os.environ.get("EQUIP_MEDIAMTX_WHEP_PORT", "8889"))
MEDIAMTX_WHEP_URL = f"http://127.0.0.1:{MEDIAMTX_WHEP_PORT}/live/whep"

LOG_FILE = Path(os.environ.get("EQUIP_LOG_FILE", str(CAPTURE_DIR / "companion-api.log")))
LOG_FILE.parent.mkdir(parents=True, exist_ok=True)


# ---------------------------------------------------------------------------
# Logging
# ---------------------------------------------------------------------------

def _build_logger() -> logging.Logger:
    logger = logging.getLogger("equip.companion")
    if logger.handlers:
        return logger

    logger.setLevel(logging.INFO)
    formatter = logging.Formatter(
        "%(asctime)s %(levelname)s %(name)s %(message)s",
        datefmt="%Y-%m-%d %H:%M:%S",
    )

    stream_handler = logging.StreamHandler()
    stream_handler.setFormatter(formatter)
    logger.addHandler(stream_handler)

    file_handler = RotatingFileHandler(LOG_FILE, maxBytes=1_000_000, backupCount=3)
    file_handler.setFormatter(formatter)
    logger.addHandler(file_handler)

    logger.propagate = False
    return logger


logger = _build_logger()


_FFMPEG_ENCODER_LOCK = threading.Lock()
_SELECTED_RTSP_VIDEO_ENCODER: Optional[str] = None


def _spawn_stderr_logger(process: subprocess.Popen, process_name: str) -> None:
    """Drain subprocess stderr and mirror lines into API logs."""
    if process.stderr is None:
        return

    def _reader() -> None:
        try:
            for raw in iter(process.stderr.readline, b""):
                line = raw.decode("utf-8", errors="replace").strip()
                if line:
                    logger.warning("%s-stderr %s", process_name, line)
        except Exception as e:
            logger.debug("%s-stderr-reader-error error=%s", process_name, e)

    threading.Thread(target=_reader, name=f"{process_name}-stderr", daemon=True).start()


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
    except Exception as e:
        logger.warning("ffmpeg-encoder-probe-failed encoder=%s error=%s", encoder_name, e)
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
            candidates = [preferred] + [c for c in candidates if c != preferred]

        for encoder in candidates:
            if not _ffmpeg_has_encoder(encoder):
                continue
            if not _ffmpeg_encoder_is_usable(encoder):
                continue
            _SELECTED_RTSP_VIDEO_ENCODER = encoder
            logger.info("ffmpeg-rtsp-video-encoder-selected encoder=%s webrtc_compatible=%s", encoder, _is_webrtc_compatible_encoder(encoder))
            return encoder

    raise RuntimeError(
        "No usable RTSP video encoder found. Tried: h264_rkmpp, h264_v4l2m2m, h264_nvenc, h264_vaapi, libx264, mjpeg"
    )


def _build_rtsp_video_output_args(rtsp_url: str) -> list[str]:
    """Build RTSP output args for the selected encoder without using FIFO."""
    encoder = _select_rtsp_video_encoder()

    args = ["-c:v", encoder]

    if encoder == "libx264":
        # x264 path: full low-latency tuning.
        args.extend([
            "-preset", "ultrafast",
            "-tune", "zerolatency",
            "-x264-params", "keyint=5:min-keyint=5:scenecut=0:bframes=0",
        ])
    elif _is_webrtc_compatible_encoder(encoder):
        args.extend([
            "-g", "5",
            "-bf", "0",
        ])
    else:
        # MJPEG fallback for boards without usable H.264 encoders.
        args.extend([
            "-q:v", "5",
            "-an",
        ])

    if _is_webrtc_compatible_encoder(encoder):
        args.extend([
            "-c:a", "aac",
            "-b:a", "128k",
            "-ar", "44100",
        ])

    args.extend(["-f", "rtsp", "-rtsp_transport", "tcp", rtsp_url])
    return args


def _safe_selected_rtsp_encoder() -> Optional[str]:
    try:
        return _select_rtsp_video_encoder()
    except Exception as e:
        logger.warning("ffmpeg-rtsp-video-encoder-unavailable error=%s", e)
        return None


def _stream_mjpeg_direct_generate(stream_id: int) -> "typing.Iterator[bytes]":
    """Direct camera->MJPEG stream that does not rely on RTSP/mediamtx.

    This is used as a no-FIFO fallback when no WebRTC-compatible encoder is
    available on the device.
    """
    capture_mode = config.get_mode()
    dvgrab_process: Optional[subprocess.Popen] = None
    ffmpeg_process: Optional[subprocess.Popen] = None

    try:
        if capture_mode == "dvgrab":
            dvgrab_process = subprocess.Popen(
                ["dvgrab", "--format", "raw", "-"],
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                bufsize=0,
                start_new_session=True,
            )
            _spawn_stderr_logger(dvgrab_process, "mjpeg-direct-dvgrab")

            ffmpeg_process = subprocess.Popen(
                [
                    "ffmpeg",
                    "-hide_banner",
                    "-loglevel",
                    "error",
                    "-fflags",
                    "nobuffer",
                    "-flags",
                    "low_delay",
                    "-f",
                    "dv",
                    "-i",
                    "pipe:0",
                    "-vf",
                    "fps=10,scale=960:-1",
                    "-q:v",
                    "5",
                    "-f",
                    "mpjpeg",
                    "-flush_packets",
                    "1",
                    "pipe:1",
                ],
                stdin=dvgrab_process.stdout,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                bufsize=0,
                start_new_session=True,
            )
            _spawn_stderr_logger(ffmpeg_process, "mjpeg-direct-ffmpeg")
            if dvgrab_process.stdout is not None:
                dvgrab_process.stdout.close()
        else:
            ffmpeg_process = subprocess.Popen(
                [
                    "ffmpeg",
                    "-hide_banner",
                    "-loglevel",
                    "error",
                    "-fflags",
                    "nobuffer",
                    "-flags",
                    "low_delay",
                    "-f",
                    "iec61883",
                    "-i",
                    "auto",
                    "-vf",
                    "fps=10,scale=960:-1",
                    "-q:v",
                    "5",
                    "-f",
                    "mpjpeg",
                    "-flush_packets",
                    "1",
                    "pipe:1",
                ],
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                bufsize=0,
                start_new_session=True,
            )
            _spawn_stderr_logger(ffmpeg_process, "mjpeg-direct-ffmpeg-only")

        if ffmpeg_process is None or ffmpeg_process.stdout is None:
            return

        _register_direct_mjpeg(stream_id, dvgrab_process, ffmpeg_process)

        logger.info(
            "mjpeg-direct-start capture_mode=%s ffmpeg_pid=%s dvgrab_pid=%s",
            capture_mode,
            ffmpeg_process.pid,
            dvgrab_process.pid if dvgrab_process else None,
        )

        while True:
            if ffmpeg_process.poll() is not None:
                logger.info("mjpeg-direct-ffmpeg-exit rc=%s", ffmpeg_process.returncode)
                break
            chunk = ffmpeg_process.stdout.read(_MJPEG_CHUNK_SIZE)
            if not chunk:
                logger.info("mjpeg-direct-eof")
                break
            yield chunk

    finally:
        _unregister_direct_mjpeg(stream_id)
        _terminate_process(ffmpeg_process)
        _terminate_process(dvgrab_process)
        logger.info("mjpeg-direct-stop")


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

_REQUEST_LOCK = threading.Lock()
_ACTIVE_REQUESTS: dict[str, dict] = {}
_DIRECT_MJPEG_LOCK = threading.Lock()
_ACTIVE_DIRECT_MJPEG: dict[int, tuple[Optional[subprocess.Popen], Optional[subprocess.Popen]]] = {}
_RECORDING_MJPEG_LOCK = threading.Lock()
_RECORDING_MJPEG_SUBSCRIBERS: dict[int, queue.Queue] = {}
_RECORDING_MJPEG_RUNNING = False
_RECORDING_MJPEG_THREAD: Optional[threading.Thread] = None


def _register_direct_mjpeg(stream_id: int, dvgrab_process: Optional[subprocess.Popen], ffmpeg_process: Optional[subprocess.Popen]) -> None:
    with _DIRECT_MJPEG_LOCK:
        _ACTIVE_DIRECT_MJPEG[stream_id] = (dvgrab_process, ffmpeg_process)
    logger.info(
        "mjpeg-direct-register stream_id=%s dvgrab_pid=%s ffmpeg_pid=%s",
        stream_id,
        dvgrab_process.pid if dvgrab_process else None,
        ffmpeg_process.pid if ffmpeg_process else None,
    )


def _unregister_direct_mjpeg(stream_id: int) -> None:
    with _DIRECT_MJPEG_LOCK:
        _ACTIVE_DIRECT_MJPEG.pop(stream_id, None)
    logger.info("mjpeg-direct-unregister stream_id=%s", stream_id)


def _active_direct_mjpeg_count() -> int:
    with _DIRECT_MJPEG_LOCK:
        return len(_ACTIVE_DIRECT_MJPEG)


def _stop_all_direct_mjpeg_streams() -> None:
    with _DIRECT_MJPEG_LOCK:
        active = list(_ACTIVE_DIRECT_MJPEG.items())
        _ACTIVE_DIRECT_MJPEG.clear()

    if not active:
        return

    logger.info("mjpeg-direct-stop-all count=%s", len(active))
    for stream_id, (dvgrab_process, ffmpeg_process) in active:
        logger.info("mjpeg-direct-stop stream_id=%s", stream_id)
        _terminate_process(ffmpeg_process)
        _terminate_process(dvgrab_process)


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


def _subscribe_recording_mjpeg() -> tuple[int, queue.Queue]:
    cid = time.time_ns()
    q: queue.Queue = queue.Queue(maxsize=_MJPEG_CLIENT_QUEUE_DEPTH)
    with _RECORDING_MJPEG_LOCK:
        _RECORDING_MJPEG_SUBSCRIBERS[cid] = q
    logger.info("record-mjpeg-subscriber-add cid=%s total=%s", cid, len(_RECORDING_MJPEG_SUBSCRIBERS))
    return cid, q


def _unsubscribe_recording_mjpeg(cid: int) -> None:
    with _RECORDING_MJPEG_LOCK:
        _RECORDING_MJPEG_SUBSCRIBERS.pop(cid, None)
        total = len(_RECORDING_MJPEG_SUBSCRIBERS)
    logger.info("record-mjpeg-subscriber-remove cid=%s total=%s", cid, total)


def _terminate_process(process: Optional[subprocess.Popen], timeout: float = 3.0) -> None:
    if process is None:
        return
    if process.poll() is not None:
        logger.info("process-already-exited pid=%s rc=%s", process.pid, process.returncode)
        return

    logger.info("process-stop-start pid=%s", process.pid)
    try:
        os.killpg(process.pid, signal.SIGTERM)
    except ProcessLookupError:
        return

    try:
        process.wait(timeout=timeout)
        logger.info("process-stop-graceful pid=%s rc=%s", process.pid, process.returncode)
        return
    except subprocess.TimeoutExpired:
        logger.warning("process-stop-timeout pid=%s", process.pid)

    try:
        os.killpg(process.pid, signal.SIGKILL)
    except ProcessLookupError:
        return

    try:
        process.wait(timeout=1)
        logger.info("process-stop-force pid=%s rc=%s", process.pid, process.returncode)
    except subprocess.TimeoutExpired:
        logger.error("process-stop-force-timeout pid=%s", process.pid)


# ---------------------------------------------------------------------------
# mediamtx manager
# ---------------------------------------------------------------------------

class MediamtxManager:
    """Manages the mediamtx subprocess lifecycle."""

    def __init__(self) -> None:
        self._process: Optional[subprocess.Popen] = None
        self._lock = threading.Lock()
        self._last_start_attempt_ts: float = 0.0
        self._restart_cooldown_seconds = 5.0

    def start(self) -> bool:
        with self._lock:
            if self._process is not None and self._process.poll() is None:
                logger.info("mediamtx-already-running pid=%s", self._process.pid)
                return True

            now = time.time()
            if (now - self._last_start_attempt_ts) < self._restart_cooldown_seconds:
                logger.info(
                    "mediamtx-start-cooldown remaining=%.1fs",
                    self._restart_cooldown_seconds - (now - self._last_start_attempt_ts),
                )
                return False
            self._last_start_attempt_ts = now

            if shutil.which(MEDIAMTX_BINARY) is None:
                logger.warning(
                    "mediamtx-not-found binary=%s. WebRTC streaming will be unavailable. "
                    "Install from https://github.com/bluenviron/mediamtx/releases",
                    MEDIAMTX_BINARY,
                )
                return False

            try:
                self._process = subprocess.Popen(
                    [MEDIAMTX_BINARY],
                    stdout=subprocess.DEVNULL,
                    stderr=subprocess.PIPE,
                    start_new_session=True,
                )
                _spawn_stderr_logger(self._process, "mediamtx")
                logger.info("mediamtx-start pid=%s", self._process.pid)
                # Give mediamtx a moment to bind its ports before clients connect
                time.sleep(0.5)
                return True
            except Exception as e:
                logger.error("mediamtx-start-failed error=%s", e)
                return False

    def stop(self) -> None:
        with self._lock:
            _terminate_process(self._process)
            self._process = None
            logger.info("mediamtx-stopped")

    def is_running(self) -> bool:
        with self._lock:
            return self._process is not None and self._process.poll() is None

    def refresh(self) -> None:
        """Restart mediamtx if it has crashed."""
        with self._lock:
            if self._process is None:
                return
            if self._process.poll() is not None:
                logger.warning("mediamtx-crashed rc=%s — restarting", self._process.returncode)
                self._process = None
        self.start()


# ---------------------------------------------------------------------------
# MJPEG broadcaster
# ---------------------------------------------------------------------------
# One ffmpeg reads from the RTSP stream (mediamtx) and decodes+encodes MJPEG.
# All HTTP /api/stream/mjpeg clients subscribe to receive the same frames via
# thread-safe queues.  Slow clients get frames dropped; they never block others.

_MJPEG_CHUNK_SIZE = 8192
_MJPEG_CLIENT_QUEUE_DEPTH = 40  # frames before drop
_VIDEO_GLOBS = ("*.dv", "*.mkv", "*.mp4", "*.ts")


class MjpegBroadcaster:
    """Single ffmpeg RTSP→MJPEG reader, fanning out to N HTTP clients."""

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._clients: dict[int, queue.Queue] = {}
        self._ffmpeg: Optional[subprocess.Popen] = None
        self._thread: Optional[threading.Thread] = None
        self._running = False

    # ------------------------------------------------------------------
    # Public API
    # ------------------------------------------------------------------

    def start(self) -> None:
        """Start the broadcaster (idempotent)."""
        with self._lock:
            if self._running:
                return
            self._running = True

        cmd = [
            "ffmpeg",
            "-hide_banner",
            "-loglevel", "error",
            "-fflags", "nobuffer",
            "-flags", "low_delay",
            "-rtsp_transport", "tcp",
            "-i", MEDIAMTX_RTSP_URL,
            "-vf", "fps=15,scale=960:-1",
            "-q:v", "5",
            "-f", "mpjpeg",
            "-flush_packets", "1",
            "pipe:1",
        ]

        try:
            self._ffmpeg = subprocess.Popen(
                cmd,
                stdout=subprocess.PIPE,
                stderr=subprocess.DEVNULL,
                bufsize=0,
                start_new_session=True,
            )
            logger.info("mjpeg-broadcaster-start ffmpeg-pid=%s rtsp=%s", self._ffmpeg.pid, MEDIAMTX_RTSP_URL)
        except Exception as e:
            logger.error("mjpeg-broadcaster-ffmpeg-failed error=%s", e)
            self._running = False
            return

        self._thread = threading.Thread(target=self._reader_loop, name="mjpeg-broadcaster", daemon=True)
        self._thread.start()

    def stop(self) -> None:
        """Stop broadcaster and notify all clients."""
        with self._lock:
            self._running = False
            ffmpeg = self._ffmpeg
            self._ffmpeg = None

        _terminate_process(ffmpeg)

        # Send sentinel to all waiting clients
        with self._lock:
            for q in self._clients.values():
                try:
                    q.put_nowait(None)
                except queue.Full:
                    pass
        logger.info("mjpeg-broadcaster-stopped")

    def subscribe(self) -> tuple[int, "queue.Queue[Optional[bytes]]"]:
        cid = time.time_ns()
        q: queue.Queue[Optional[bytes]] = queue.Queue(maxsize=_MJPEG_CLIENT_QUEUE_DEPTH)
        with self._lock:
            self._clients[cid] = q
        logger.info("mjpeg-subscriber-add cid=%s total=%s", cid, len(self._clients))
        return cid, q

    def unsubscribe(self, cid: int) -> None:
        with self._lock:
            self._clients.pop(cid, None)
            remaining = len(self._clients)
        logger.info("mjpeg-subscriber-remove cid=%s total=%s", cid, remaining)

    def subscriber_count(self) -> int:
        with self._lock:
            return len(self._clients)

    def is_running(self) -> bool:
        with self._lock:
            return self._running

    # ------------------------------------------------------------------
    # Internal
    # ------------------------------------------------------------------

    def _reader_loop(self) -> None:
        """Read chunks from ffmpeg stdout and fan out to all subscribers."""
        ffmpeg = self._ffmpeg
        if ffmpeg is None or ffmpeg.stdout is None:
            return

        logger.info("mjpeg-broadcaster-reader-start")
        while self._running:
            if ffmpeg.poll() is not None:
                logger.warning("mjpeg-broadcaster-ffmpeg-died rc=%s", ffmpeg.returncode)
                break
            chunk = ffmpeg.stdout.read(_MJPEG_CHUNK_SIZE)
            if not chunk:
                logger.info("mjpeg-broadcaster-eof")
                break
            with self._lock:
                for q in self._clients.values():
                    try:
                        q.put_nowait(chunk)
                    except queue.Full:
                        pass  # slow client — drop frame, never block

        # Notify all clients of EOF
        with self._lock:
            for q in self._clients.values():
                try:
                    q.put_nowait(None)
                except queue.Full:
                    pass
        logger.info("mjpeg-broadcaster-reader-done")


# ---------------------------------------------------------------------------
# ConfigState
# ---------------------------------------------------------------------------

class SeamlessDvHub:
    """Single-owner DV capture hub for seamless streaming/record transitions.

    Pipeline:
      dvgrab --format raw -  ->  ffmpeg (DV -> RTSP + MJPEG pipe)

    - WebRTC preview stays alive via continuous RTSP publish to mediamtx.
    - MJPEG subscribers read chunks from one shared ffmpeg process.
    - Recording toggles only file writing on/off; capture ownership is unchanged.
    """

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._running = False
        self._dvgrab: Optional[subprocess.Popen] = None
        self._ffmpeg: Optional[subprocess.Popen] = None
        self._pump_thread: Optional[threading.Thread] = None
        self._reader_thread: Optional[threading.Thread] = None
        self._subscribers: dict[int, queue.Queue] = {}
        self._record_file_handle: Optional[typing.BinaryIO] = None
        self._record_file_path: Optional[str] = None

    def ensure_running(self) -> None:
        with self._lock:
            if self._running and self._dvgrab is not None and self._ffmpeg is not None:
                if self._dvgrab.poll() is None and self._ffmpeg.poll() is None:
                    return

        self.stop()

        if not mediamtx.is_running():
            mediamtx.start()

        rtsp_args = _build_rtsp_video_output_args(MEDIAMTX_RTSP_URL)

        dvgrab_process = subprocess.Popen(
            ["dvgrab", "--format", "raw", "-"],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            bufsize=0,
            start_new_session=True,
        )
        _spawn_stderr_logger(dvgrab_process, "seamless-dvgrab")

        ffmpeg_process = subprocess.Popen(
            [
                "ffmpeg",
                "-hide_banner",
                "-loglevel",
                "error",
                "-fflags",
                "nobuffer",
                "-flags",
                "low_delay",
                "-f",
                "dv",
                "-i",
                "pipe:0",
                *rtsp_args,
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
            ],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            bufsize=0,
            start_new_session=True,
        )
        _spawn_stderr_logger(ffmpeg_process, "seamless-ffmpeg")

        with self._lock:
            self._dvgrab = dvgrab_process
            self._ffmpeg = ffmpeg_process
            self._running = True

        self._pump_thread = threading.Thread(target=self._pump_loop, name="seamless-dv-pump", daemon=True)
        self._reader_thread = threading.Thread(target=self._reader_loop, name="seamless-mjpeg-reader", daemon=True)
        self._pump_thread.start()
        self._reader_thread.start()
        logger.info("seamless-hub-start dvgrab_pid=%s ffmpeg_pid=%s", dvgrab_process.pid, ffmpeg_process.pid)

    def is_running(self) -> bool:
        with self._lock:
            return self._running and self._dvgrab is not None and self._ffmpeg is not None and self._dvgrab.poll() is None and self._ffmpeg.poll() is None

    def start_recording(self, output_path: Path) -> None:
        self.ensure_running()
        with self._lock:
            if self._record_file_handle is not None:
                logger.info("seamless-record-start-ignored file=%s", self._record_file_path)
                return
            self._record_file_handle = open(output_path, "wb")
            self._record_file_path = str(output_path)
            logger.info("seamless-record-start file=%s", self._record_file_path)

    def stop_recording(self) -> None:
        with self._lock:
            handle = self._record_file_handle
            path = self._record_file_path
            self._record_file_handle = None
            self._record_file_path = None
        if handle is not None:
            try:
                handle.flush()
            except Exception:
                pass
            handle.close()
            logger.info("seamless-record-stop file=%s", path)

    def subscribe(self) -> tuple[int, queue.Queue]:
        self.ensure_running()
        cid = time.time_ns()
        q: queue.Queue = queue.Queue(maxsize=_MJPEG_CLIENT_QUEUE_DEPTH)
        with self._lock:
            self._subscribers[cid] = q
            total = len(self._subscribers)
        logger.info("seamless-subscriber-add cid=%s total=%s", cid, total)
        return cid, q

    def unsubscribe(self, cid: int) -> None:
        with self._lock:
            self._subscribers.pop(cid, None)
            total = len(self._subscribers)
        logger.info("seamless-subscriber-remove cid=%s total=%s", cid, total)

    def stop(self) -> None:
        with self._lock:
            self._running = False
            ffmpeg_process = self._ffmpeg
            dvgrab_process = self._dvgrab
            self._ffmpeg = None
            self._dvgrab = None
            handle = self._record_file_handle
            self._record_file_handle = None
            self._record_file_path = None
            subscribers = list(self._subscribers.values())
            self._subscribers.clear()

        for q in subscribers:
            try:
                q.put_nowait(None)
            except queue.Full:
                pass

        if handle is not None:
            try:
                handle.flush()
            except Exception:
                pass
            handle.close()

        _terminate_process(ffmpeg_process)
        _terminate_process(dvgrab_process)
        logger.info("seamless-hub-stop")

    def _pump_loop(self) -> None:
        while True:
            with self._lock:
                if not self._running:
                    break
                dvgrab_process = self._dvgrab
                ffmpeg_process = self._ffmpeg
                record_file = self._record_file_handle

            if dvgrab_process is None or ffmpeg_process is None:
                break
            if dvgrab_process.stdout is None or ffmpeg_process.stdin is None:
                break
            if dvgrab_process.poll() is not None or ffmpeg_process.poll() is not None:
                break

            chunk = dvgrab_process.stdout.read(_MJPEG_CHUNK_SIZE)
            if not chunk:
                break

            try:
                ffmpeg_process.stdin.write(chunk)
                ffmpeg_process.stdin.flush()
            except Exception as e:
                logger.warning("seamless-pump-ffmpeg-write-failed error=%s", e)
                break

            if record_file is not None:
                try:
                    record_file.write(chunk)
                except Exception as e:
                    logger.warning("seamless-pump-file-write-failed error=%s", e)

        logger.info("seamless-pump-stop")
        self.stop()

    def _reader_loop(self) -> None:
        while True:
            with self._lock:
                if not self._running:
                    break
                ffmpeg_process = self._ffmpeg
                subscribers = list(self._subscribers.values())

            if ffmpeg_process is None or ffmpeg_process.stdout is None:
                break
            if ffmpeg_process.poll() is not None:
                break

            chunk = ffmpeg_process.stdout.read(_MJPEG_CHUNK_SIZE)
            if not chunk:
                break

            for q in subscribers:
                try:
                    q.put_nowait(chunk)
                except queue.Full:
                    pass

        logger.info("seamless-reader-stop")


@dataclass
class ConfigState:
    """Runtime configuration that can be changed via API."""
    recording_capture_mode: str = "dvgrab"
    capture_mode_lock: threading.Lock = None  # type: ignore[assignment]

    def __post_init__(self) -> None:
        self.capture_mode_lock = threading.Lock()

    def set_mode(self, mode: str) -> None:
        if mode not in ["dvgrab", "ffmpeg-only"]:
            raise ValueError(f"Invalid recording capture mode: {mode}")
        with self.capture_mode_lock:
            self.recording_capture_mode = mode
            logger.info("config-set recording_capture_mode=%s", mode)

    def get_mode(self) -> str:
        with self.capture_mode_lock:
            return self.recording_capture_mode


# ---------------------------------------------------------------------------
# RecorderState
# ---------------------------------------------------------------------------

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

        capture_mode = config.get_mode()
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

            seamless_hub.start_recording(output_path)
            self.mode = "recording"
            self.start_time = time.time()
            self.current_file = str(output_path)
            logger.info("record-start complete file=%s", self.current_file)
            return

        # ---- Ensure mediamtx is running --------------------------------
        if not mediamtx.is_running():
            if not mediamtx.start():
                raise RuntimeError(
                    "mediamtx is not running and could not be started. "
                    "Install from https://github.com/bluenviron/mediamtx/releases "
                    f"and ensure '{MEDIAMTX_BINARY}' is in PATH."
                )

        # ---- Stop any live preview that owns the FireWire bus ----------
        preview.stop()
        _stop_all_direct_mjpeg_streams()
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

        capture_mode = config.get_mode()

        if capture_mode == "dvgrab":
            seamless_hub.stop_recording()
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

        capture_mode = config.get_mode()
        if capture_mode == "dvgrab":
            if not seamless_hub.is_running():
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


# ---------------------------------------------------------------------------
# PreviewPush — push live preview to mediamtx when not recording
# ---------------------------------------------------------------------------

class PreviewPush:
    """Single dvgrab/iec61883 → ffmpeg → RTSP process for live preview.

    Started lazily when the first MJPEG client connects (or when WebRTC is
    requested) and stopped when recording starts or the API shuts down.
    Only one FireWire source process is ever running at a time.
    """

    # Seconds to wait before retrying after a failure (prevents process storm)
    _RETRY_COOLDOWN = 3.0

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._dvgrab: Optional[subprocess.Popen] = None
        self._ffmpeg: Optional[subprocess.Popen] = None
        self._last_failure_ts: float = 0.0

    def ensure_running(self) -> None:
        """Start preview push if not already running and not recording."""
        with self._lock:
            if state.is_recording:
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

            capture_mode = config.get_mode()
            logger.info("preview-push-start capture_mode=%s", capture_mode)

            if not mediamtx.is_running():
                mediamtx.start()

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


# ---------------------------------------------------------------------------
# Singletons
# ---------------------------------------------------------------------------

mediamtx = MediamtxManager()
mjpeg_broadcaster = MjpegBroadcaster()
seamless_hub = SeamlessDvHub()
config = ConfigState(recording_capture_mode=RECORDING_CAPTURE_MODE)
state = RecorderState()
preview = PreviewPush()

app = FastAPI(title="equip-1 companion api", version="0.2.0")

app.add_middleware(
    CORSMiddleware,
    allow_origins=["*"],
    allow_credentials=False,
    allow_methods=["*"],
    allow_headers=["*"],
)


# ---------------------------------------------------------------------------
# Middleware
# ---------------------------------------------------------------------------

@app.middleware("http")
async def request_logging_middleware(request: Request, call_next):
    started = time.time()
    req_id = f"{int(started * 1000)}-{os.getpid()}"
    client = request.client.host if request.client else "unknown"
    with _REQUEST_LOCK:
        _ACTIVE_REQUESTS[req_id] = {
            "id": req_id,
            "method": request.method,
            "path": request.url.path,
            "client": client,
            "started_at": int(started),
        }

    logger.info("request-start id=%s method=%s path=%s client=%s", req_id, request.method, request.url.path, client)
    try:
        response = await call_next(request)
    except Exception:
        elapsed_ms = int((time.time() - started) * 1000)
        logger.exception("request-error id=%s method=%s path=%s duration_ms=%s", req_id, request.method, request.url.path, elapsed_ms)
        with _REQUEST_LOCK:
            _ACTIVE_REQUESTS.pop(req_id, None)
        raise

    elapsed_ms = int((time.time() - started) * 1000)
    logger.info("request-end id=%s method=%s path=%s status=%s duration_ms=%s", req_id, request.method, request.url.path, response.status_code, elapsed_ms)
    with _REQUEST_LOCK:
        _ACTIVE_REQUESTS.pop(req_id, None)
    return response


# ---------------------------------------------------------------------------
# Lifecycle
# ---------------------------------------------------------------------------

@app.on_event("startup")
def on_startup() -> None:
    logger.info("startup-begin")
    mediamtx.start()
    logger.info("startup-complete")


@app.on_event("shutdown")
def on_shutdown() -> None:
    logger.warning("shutdown-begin")
    seamless_hub.stop()
    _stop_recording_mjpeg_fanout()
    mjpeg_broadcaster.stop()
    preview.stop()
    _stop_all_direct_mjpeg_streams()
    state.stop()
    mediamtx.stop()
    logger.warning("shutdown-complete")


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def _storage_stats() -> dict:
    s = os.statvfs(CAPTURE_DIR)
    total = s.f_blocks * s.f_frsize
    free = s.f_bavail * s.f_frsize
    used = total - free
    return {
        "total_bytes": total,
        "used_bytes": used,
        "free_bytes": free,
        "used_percent": round((used / total) * 100, 2) if total else 0,
    }


def _list_videos(limit: int = 30) -> list[dict]:
    videos = []
    candidates: list[Path] = []
    for pattern in _VIDEO_GLOBS:
        candidates.extend(CAPTURE_DIR.glob(pattern))

    # Deduplicate by resolved path in case patterns overlap.
    unique_candidates = {p.resolve(): p for p in candidates}.values()

    for path in sorted(unique_candidates, key=lambda p: p.stat().st_mtime, reverse=True):
        meta = path.stat()
        videos.append({
            "name": path.name,
            "size_bytes": meta.st_size,
            "modified_unix": int(meta.st_mtime),
            "download_path": f"/api/files/download/{path.name}",
        })
        if len(videos) >= limit:
            break
    return videos


def _resolve_capture_file(name: str) -> Path:
    if not name or "/" in name or "\\" in name:
        raise HTTPException(status_code=400, detail="Invalid file name")

    candidate = (CAPTURE_DIR / name).resolve()
    base = CAPTURE_DIR.resolve()

    if base not in candidate.parents and candidate != base:
        raise HTTPException(status_code=400, detail="Invalid file path")
    if not candidate.exists() or not candidate.is_file():
        raise HTTPException(status_code=404, detail="File not found")

    return candidate


def _check_stream_requirements() -> dict:
    # Any /dev/fw* node means a FireWire device is present (fw0, fw1, …)
    fw_nodes = glob.glob("/dev/fw[0-9]*")
    return {
        "dvgrab": shutil.which("dvgrab") is not None,
        "ffmpeg": shutil.which("ffmpeg") is not None,
        "mediamtx": shutil.which(MEDIAMTX_BINARY) is not None,
        "camera_present": bool(fw_nodes),
        "camera_devices": fw_nodes,
    }


def _active_stream_pipeline() -> str:
    mode = config.get_mode()
    if mode == "dvgrab":
        return "dvgrab-seamless-hub" if seamless_hub.is_running() else "dvgrab-seamless-hub-idle"

    if state.is_recording:
        return "ffmpeg-only-recording"
    if preview.is_alive():
        return "ffmpeg-only-preview"
    if mjpeg_broadcaster.is_running():
        return "ffmpeg-only-mjpeg-broadcaster"
    if _active_direct_mjpeg_count() > 0:
        return "ffmpeg-only-direct-mjpeg"
    return "ffmpeg-only-idle"


def _reset_stream_workers_for_mode_change(new_mode: str) -> None:
    """Reset active stream workers so mode changes are immediately observable.

    Clients may need to reconnect after mode switch.
    """
    logger.info("capture-mode-switch-reset mode=%s", new_mode)
    _stop_recording_mjpeg_fanout()
    mjpeg_broadcaster.stop()
    preview.stop()
    _stop_all_direct_mjpeg_streams()
    seamless_hub.stop()


# ---------------------------------------------------------------------------
# Routes — Health & Status
# ---------------------------------------------------------------------------

@app.get("/health")
def health() -> dict:
    return {
        "ok": True,
        "service": "equip-1-companion-api",
        "hostname": socket.gethostname(),
    }


@app.get("/api/status")
def status() -> dict:
    state.refresh_process_state()
    req = _check_stream_requirements()
    rtsp_encoder = _safe_selected_rtsp_encoder() if req["ffmpeg"] else None
    capture_mode = config.get_mode()
    return {
        "recorder": {
            "mode": state.mode,
            "elapsed_seconds": state.elapsed_seconds,
            "current_file": state.current_file,
            "capture_mode": capture_mode,
        },
        "storage": _storage_stats(),
        "files": _list_videos(limit=10),
        "network": {
            "mode": "local-network",
            "hint": "Assumes device/API is reachable on your LAN for this prototype",
        },
        "stream": {
            "available": req["ffmpeg"] and req["camera_present"],
            "requirements": req,
            "mjpeg_url": "/api/stream/mjpeg",
            "whep_proxy_url": "/api/stream/whep",
            "mediamtx_running": mediamtx.is_running(),
            "mediamtx_whep_port": MEDIAMTX_WHEP_PORT,
            "rtsp_video_encoder": rtsp_encoder,
            "whep_available": _is_webrtc_compatible_encoder(rtsp_encoder) if rtsp_encoder else False,
            "source": "recording" if state.is_recording else "preview",
            "capture_mode": capture_mode,
            "pipeline": _active_stream_pipeline(),
        },
    }


# ---------------------------------------------------------------------------
# Routes — Recording
# ---------------------------------------------------------------------------

@app.post("/api/record/toggle")
def toggle_recording() -> dict:
    try:
        state.toggle()
    except RuntimeError as error:
        logger.warning("record-toggle-failed error=%s", error)
        raise HTTPException(status_code=503, detail=str(error)) from error
    return {
        "mode": state.mode,
        "elapsed_seconds": state.elapsed_seconds,
        "current_file": state.current_file,
    }


@app.post("/api/record/start")
def start_recording() -> dict:
    try:
        state.start()
    except RuntimeError as error:
        logger.warning("record-start-failed error=%s", error)
        raise HTTPException(status_code=503, detail=str(error)) from error
    return {
        "mode": state.mode,
        "elapsed_seconds": state.elapsed_seconds,
        "current_file": state.current_file,
    }


@app.post("/api/record/stop")
def stop_recording() -> dict:
    state.stop()
    return {
        "mode": state.mode,
        "elapsed_seconds": state.elapsed_seconds,
        "current_file": state.current_file,
    }


# ---------------------------------------------------------------------------
# Routes — Config
# ---------------------------------------------------------------------------

@app.get("/api/config/recording-capture-mode")
def get_recording_capture_mode() -> dict:
    return {
        "current_mode": config.get_mode(),
        "available_modes": ["dvgrab", "ffmpeg-only"],
        "recorder_is_active": state.is_recording,
    }


@app.post("/api/config/recording-capture-mode")
def set_recording_capture_mode(body: dict) -> dict:
    if state.is_recording:
        raise HTTPException(
            status_code=409,
            detail="Cannot change recording mode while recording is active. Stop recording first.",
        )

    new_mode = body.get("mode")
    if not new_mode:
        raise HTTPException(status_code=400, detail="Missing 'mode' field in request body")

    try:
        config.set_mode(new_mode)
        _reset_stream_workers_for_mode_change(new_mode)
    except ValueError as error:
        raise HTTPException(status_code=400, detail=str(error)) from error

    return {
        "current_mode": config.get_mode(),
        "available_modes": ["dvgrab", "ffmpeg-only"],
        "stream_reconnect_required": True,
        "message": f"Recording capture mode changed to {new_mode}",
    }


# ---------------------------------------------------------------------------
# Routes — Files
# ---------------------------------------------------------------------------

@app.get("/api/files")
def files() -> dict:
    return {
        "capture_dir": str(CAPTURE_DIR),
        "items": _list_videos(limit=100),
    }


@app.get("/api/files/download/{name}")
def download_file(name: str) -> FileResponse:
    file_path = _resolve_capture_file(name)
    logger.info("file-download name=%s size=%s", file_path.name, file_path.stat().st_size)
    return FileResponse(
        path=file_path,
        filename=file_path.name,
        media_type="application/octet-stream",
    )


@app.get("/api/storage")
def storage() -> dict:
    return _storage_stats()


# ---------------------------------------------------------------------------
# Routes — Streaming
# ---------------------------------------------------------------------------

@app.get("/api/stream/requirements")
def stream_requirements() -> dict:
    checks = _check_stream_requirements()
    return {
        "ok": all(checks.values()),
        "checks": checks,
    }


@app.get("/api/stream/mjpeg")
def stream_mjpeg() -> StreamingResponse:
    """MJPEG fallback stream.

    Reads from mediamtx RTSP (which has either recording or preview data)
    via a single shared broadcaster thread.  Multiple clients all subscribe
    to the same broadcaster — no data-splitting, no FireWire conflicts.
    """
    req = _check_stream_requirements()
    if not req["ffmpeg"]:
        raise HTTPException(status_code=503, detail="ffmpeg is not installed")

    capture_mode = config.get_mode()
    rtsp_encoder = _safe_selected_rtsp_encoder() if req["ffmpeg"] else None
    webrtc_ok = _is_webrtc_compatible_encoder(rtsp_encoder) if rtsp_encoder else False

    # dvgrab mode always uses the seamless hub as single capture owner.
    if capture_mode == "dvgrab":
        seamless_hub.ensure_running()

        def generate_seamless_mjpeg():
            cid, q = seamless_hub.subscribe()
            try:
                while True:
                    try:
                        chunk = q.get(timeout=10.0)
                    except queue.Empty:
                        logger.warning("seamless-client-timeout cid=%s", cid)
                        break
                    if chunk is None:
                        logger.info("seamless-client-eof cid=%s", cid)
                        break
                    yield chunk
            except GeneratorExit:
                logger.info("seamless-client-disconnect cid=%s", cid)
            finally:
                seamless_hub.unsubscribe(cid)

        logger.info("mjpeg-seamless-hub reason=dvgrab-single-owner encoder=%s", rtsp_encoder)
        return StreamingResponse(
            generate_seamless_mjpeg(),
            media_type="multipart/x-mixed-replace; boundary=ffmpeg",
            headers={"Cache-Control": "no-store"},
        )

    # No-FIFO fallback path: when RTSP/WebRTC-compatible encoder is not usable,
    # bypass mediamtx and stream MJPEG directly from the capture source.
    if not webrtc_ok:
        if _active_direct_mjpeg_count() > 0:
            raise HTTPException(
                status_code=429,
                detail="Direct MJPEG preview supports one client at a time on this device.",
            )
        stream_id = time.time_ns()
        logger.info("mjpeg-direct-fallback reason=no-webrtc-compatible-encoder encoder=%s", rtsp_encoder)
        return StreamingResponse(
            _stream_mjpeg_direct_generate(stream_id),
            media_type="multipart/x-mixed-replace; boundary=ffmpeg",
            headers={"Cache-Control": "no-store"},
        )
    # Do NOT gate on camera_present here — the preview push handles that and
    # will fail naturally if the camera is absent; the broadcaster timeout will
    # close the connection cleanly for the client.

    # Ensure we have something pushing to mediamtx
    if not state.is_recording:
        preview.ensure_running()
        # Give the preview a moment to connect to mediamtx and start sending frames
        time.sleep(0.8)

    # Ensure mediamtx is up and the broadcaster is running
    if not mjpeg_broadcaster.is_running():
        mjpeg_broadcaster.start()
        time.sleep(0.5)  # brief warm-up

    def generate():
        cid, q = mjpeg_broadcaster.subscribe()
        try:
            while True:
                try:
                    chunk = q.get(timeout=10.0)
                except queue.Empty:
                    logger.warning("mjpeg-client-timeout cid=%s", cid)
                    break
                if chunk is None:
                    logger.info("mjpeg-client-eof cid=%s", cid)
                    break
                yield chunk
        except GeneratorExit:
            logger.info("mjpeg-client-disconnect cid=%s", cid)
        finally:
            mjpeg_broadcaster.unsubscribe(cid)
            # Stop the broadcaster when the last client leaves
            if mjpeg_broadcaster.subscriber_count() == 0:
                mjpeg_broadcaster.stop()
                # Stop the preview push when nobody is watching (save resources)
                if not state.is_recording:
                    preview.stop()

    return StreamingResponse(
        generate(),
        media_type="multipart/x-mixed-replace; boundary=ffmpeg",
        headers={"Cache-Control": "no-store"},
    )


@app.post("/api/stream/whep")
async def whep_proxy(request: Request) -> Response:
    """Proxy WHEP signalling to mediamtx.

    The browser sends its WebRTC offer SDP here; we forward it to mediamtx
    and return the answer SDP.  The actual media UDP/DTLS stream connects
    directly from the browser to mediamtx (ICE candidates point to the SBC IP).

    mediamtx returns 404 until an ffmpeg publisher has connected.  We wait
    up to ~3 s and retry so the browser doesn't need to know about this
    internal timing race.
    """
    if not mediamtx.is_running():
        raise HTTPException(status_code=503, detail="mediamtx is not running")

    encoder = _safe_selected_rtsp_encoder()
    if not encoder:
        raise HTTPException(
            status_code=503,
            detail="No usable RTSP video encoder is available on this device. Use /api/stream/mjpeg only.",
        )
    if not _is_webrtc_compatible_encoder(encoder):
        raise HTTPException(
            status_code=503,
            detail=(
                f"WebRTC unavailable: selected RTSP encoder '{encoder}' is not WebRTC-compatible. "
                "Use /api/stream/mjpeg for immediate preview."
            ),
        )

    # Ensure something is pushing to the RTSP path
    if config.get_mode() == "dvgrab":
        seamless_hub.ensure_running()
        await asyncio.sleep(0.8)
    elif not state.is_recording:
        preview.ensure_running()
        # Give the preview ffmpeg time to connect and start publishing to mediamtx
        # before we forward the SDP offer (mediamtx returns 404 with no publisher).
        await asyncio.sleep(2.0)

    body = await request.body()
    last_resp = None
    try:
        async with httpx.AsyncClient(timeout=10.0) as client:
            for attempt in range(4):  # up to ~6 s total wait
                resp = await client.post(
                    MEDIAMTX_WHEP_URL,
                    content=body,
                    headers={"Content-Type": "application/sdp"},
                )
                last_resp = resp
                logger.info("whep-proxy attempt=%s status=%s", attempt + 1, resp.status_code)
                if resp.status_code != 404:
                    break
                # 404 = no publisher yet; wait and retry
                await asyncio.sleep(1.5)

        if last_resp.status_code == 404:
            # Still no publisher after retries -- tell the browser to retry shortly
            return Response(
                content=b'{"error": "stream not ready -- camera may be disconnected"}',
                status_code=503,
                media_type="application/json",
                headers={"Access-Control-Allow-Origin": "*", "Retry-After": "3"},
            )

        return Response(
            content=last_resp.content,
            status_code=last_resp.status_code,
            media_type=last_resp.headers.get("content-type", "application/sdp"),
            headers={"Access-Control-Allow-Origin": "*"},
        )
    except httpx.ConnectError as e:
        logger.error("whep-proxy-connect-error error=%s", e)
        raise HTTPException(status_code=502, detail=f"Cannot reach mediamtx at {MEDIAMTX_WHEP_URL}") from e
    except Exception as e:
        logger.exception("whep-proxy-error error=%s", e)
        raise HTTPException(status_code=500, detail="WHEP proxy error") from e


# ICE candidate PATCH forwarding (some WHEP clients send trickle ICE)
@app.patch("/api/stream/whep")
async def whep_patch_proxy(request: Request) -> Response:
    body = await request.body()
    async with httpx.AsyncClient(timeout=5.0) as client:
        resp = await client.patch(
            MEDIAMTX_WHEP_URL,
            content=body,
            headers=dict(request.headers),
        )
    return Response(content=resp.content, status_code=resp.status_code)


# ---------------------------------------------------------------------------
# Routes — Debug
# ---------------------------------------------------------------------------

@app.get("/api/debug/runtime")
def debug_runtime() -> dict:
    with _REQUEST_LOCK:
        active_requests = list(_ACTIVE_REQUESTS.values())
    return {
        "active_request_count": len(active_requests),
        "active_requests": active_requests,
        "mediamtx_running": mediamtx.is_running(),
        "mjpeg_broadcaster_running": mjpeg_broadcaster.is_running(),
        "mjpeg_subscriber_count": mjpeg_broadcaster.subscriber_count(),
        "preview_push_alive": preview.is_alive(),
        "recorder_mode": state.mode,
        "recorder_dvgrab_pid": state.dvgrab_process.pid if state.dvgrab_process else None,
        "recorder_mux_pid": state.mux_process.pid if state.mux_process else None,
        "recorder_current_file": state.current_file,
        "capture_mode": config.get_mode(),
    }
