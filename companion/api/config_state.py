"""Runtime configuration state for equip-1 companion."""

import threading
from dataclasses import dataclass

from logging_setup import get_logger


logger = get_logger()


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
