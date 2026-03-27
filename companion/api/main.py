import asyncio
import glob
import os
import queue
import shutil
import socket
import subprocess
import threading
import time
import typing
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

from config import CAPTURE_DIR
from config import MEDIAMTX_BINARY
from config import MEDIAMTX_RTSP_URL
from config import MEDIAMTX_WHEP_PORT
from config import MEDIAMTX_WHEP_URL
from config import RECORDING_CAPTURE_MODE
from encoders import _build_rtsp_video_output_args
from encoders import _is_webrtc_compatible_encoder
from encoders import _safe_selected_rtsp_encoder
from logging_setup import get_logger
from managers import MediamtxManager
from managers import MjpegBroadcaster
from managers import SeamlessDvHub
from config_state import ConfigState
from preview import PreviewPush
from recorder import RecorderState
from recorder import _start_recording_mjpeg_fanout
from recorder import _stop_recording_mjpeg_fanout
from process_utils import _spawn_stderr_logger
from process_utils import _terminate_process


# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
logger = get_logger()

# Constants for direct MJPEG fallback
_VIDEO_GLOBS = ("*.dv", "*.mkv", "*.mp4", "*.ts")
_MJPEG_CHUNK_SIZE = 8192
_MJPEG_CLIENT_QUEUE_DEPTH = 40  # frames before drop


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


# ---------------------------------------------------------------------------
# mediamtx manager
# ---------------------------------------------------------------------------

# ---------------------------------------------------------------------------
# MJPEG broadcaster
# ---------------------------------------------------------------------------
# One ffmpeg reads from the RTSP stream (mediamtx) and decodes+encodes MJPEG.
# All HTTP /api/stream/mjpeg clients subscribe to receive the same frames via
# thread-safe queues.  Slow clients get frames dropped; they never block others.

# Note: MjpegBroadcaster class is now in managers.py module

# Removed: class MjpegBroadcaster:
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


# Classes are now imported from their modules:
# - MediamtxManager from managers.py
# - MjpegBroadcaster from managers.py  
# - SeamlessDvHub from managers.py
# - ConfigState from config_state.py
# - PreviewPush from preview.py
# - RecorderState from recorder.py


# ---------------------------------------------------------------------------
# Singletons
# ---------------------------------------------------------------------------

mediamtx = MediamtxManager()
mjpeg_broadcaster = MjpegBroadcaster()
seamless_hub = SeamlessDvHub(mediamtx)
config = ConfigState(recording_capture_mode=RECORDING_CAPTURE_MODE)
preview = PreviewPush(config, mediamtx, None)  # RecorderState set after initialization
state = RecorderState(
    config=config,
    mediamtx=mediamtx,
    seamless_hub=seamless_hub,
    preview=preview,
    stop_all_direct_mjpeg=_stop_all_direct_mjpeg_streams,
)
# Update preview's recorder_state reference
preview._recorder = state

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
