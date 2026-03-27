import os
import signal
import subprocess
import threading
from typing import Optional

from logging_setup import get_logger

logger = get_logger()


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
        except Exception as error:
            logger.debug("%s-stderr-reader-error error=%s", process_name, error)

    threading.Thread(target=_reader, name=f"{process_name}-stderr", daemon=True).start()


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
