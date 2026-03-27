"""Stream and media management classes for equip-1 companion.

Includes:
- MediamtxManager: Controls mediamtx WebRTC streaming server lifecycle
- MjpegBroadcaster: Fans out RTSP/MJPEG to multiple HTTP clients
- SeamlessDvHub: Single-owner DV capture hub for seamless streaming/recording
"""

import queue
import subprocess
import threading
import time
import typing
from pathlib import Path
from typing import Optional

from config import MEDIAMTX_BINARY
from config import MEDIAMTX_RTSP_URL
from encoders import _build_rtsp_video_output_args
from logging_setup import get_logger
from process_utils import _spawn_stderr_logger
from process_utils import _terminate_process


logger = get_logger()

# Constants for MJPEG streaming
_MJPEG_CHUNK_SIZE = 8192
_MJPEG_CLIENT_QUEUE_DEPTH = 40  # frames before drop


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

            if subprocess.run(
                ["which", MEDIAMTX_BINARY], capture_output=True
            ).returncode != 0:
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


class SeamlessDvHub:
    """Single-owner DV capture hub for seamless streaming/record transitions.

    Pipeline:
      dvgrab --format raw -  ->  ffmpeg (DV -> RTSP + MJPEG pipe)

    - WebRTC preview stays alive via continuous RTSP publish to mediamtx.
    - MJPEG subscribers read chunks from one shared ffmpeg process.
    - Recording toggles only file writing on/off; capture ownership is unchanged.
    """

    def __init__(self, mediamtx_manager: MediamtxManager) -> None:
        self._mediamtx = mediamtx_manager
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

        if not self._mediamtx.is_running():
            self._mediamtx.start()

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
