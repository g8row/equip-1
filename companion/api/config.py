import os
from pathlib import Path

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
