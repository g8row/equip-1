# equip-1 companion

Companion is the local dashboard + API for live DV preview and recording.

It now supports both WebRTC and MJPEG preview paths, with runtime capture mode switching between:

- `dvgrab` mode: single-owner seamless hub (continuous capture owner)
- `ffmpeg-only` mode: `iec61883` capture path

No FIFO pipeline is used in the current implementation.

## Project Layout

- `api/` FastAPI backend
- `web/` React + Vite dashboard

Recent refactor work split part of the backend into modules:

- `api/config.py`
- `api/logging_setup.py`
- `api/process_utils.py`
- `api/encoders.py`
- `api/main.py` (still the orchestration/routes file while refactor continues)

## Run API

```bash
cd companion/api
python3 -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt
uvicorn main:app --reload --host 0.0.0.0 --port 8000
```

Environment variables:

- `EQUIP_CAPTURE_DIR` capture output directory (default `~/captures`)
- `EQUIP_RECORDING_CAPTURE_MODE` startup mode (`dvgrab` or `ffmpeg-only`)
- `EQUIP_MEDIAMTX_BINARY` mediamtx binary name/path
- `EQUIP_MEDIAMTX_RTSP_URL` RTSP ingest URL (default `rtsp://127.0.0.1:8554/live`)
- `EQUIP_MEDIAMTX_WHEP_PORT` WHEP port (default `8889`)
- `EQUIP_LOG_FILE` API log file path

## Run Web

```bash
cd companion/web
npm install
npm run dev
```

Default API base is `http://127.0.0.1:8000`.

Override:

```bash
VITE_API_BASE=http://<device-ip>:8000 npm run dev
```

## Streaming Behavior

### dvgrab mode

- A single seamless hub owns capture.
- WebRTC publish and MJPEG fanout come from the same capture owner.
- Recording toggles file writing without replacing the capture owner.

### ffmpeg-only mode

- Preview/recording use `ffmpeg -f iec61883 -i auto` paths.
- WebRTC and MJPEG are available through mediamtx/RTSP + broadcaster fallback flow.

### Mode switching

- Endpoint `POST /api/config/recording-capture-mode` updates mode.
- Active stream workers are reset so new sessions use the selected mode.
- Clients should reconnect after mode change.

## API Endpoints

- `GET /health`
- `GET /api/status`
- `POST /api/record/toggle`
- `POST /api/record/start`
- `POST /api/record/stop`
- `GET /api/config/recording-capture-mode`
- `POST /api/config/recording-capture-mode`
- `GET /api/storage`
- `GET /api/files`
- `GET /api/files/download/{name}`
- `GET /api/stream/requirements`
- `GET /api/stream/mjpeg`
- `POST /api/stream/whep`
- `PATCH /api/stream/whep`
- `GET /api/debug/runtime`

## Status Fields Worth Watching

`GET /api/status` includes:

- `recorder.capture_mode`
- `stream.capture_mode`
- `stream.pipeline`
- `stream.rtsp_video_encoder`
- `stream.whep_available`

These make it easier to verify what path is currently active after mode switches.

## Notes and Known Behavior

- Switching capture mode intentionally restarts stream workers to avoid mixed ownership.
- If mediamtx is unavailable, WebRTC is unavailable; MJPEG may still work via fallback paths.
- The API records video files with supported extensions (`.dv`, `.mkv`, `.mp4`, `.ts`) in the capture directory listing.

## Prerequisites on Device

- `ffmpeg`
- `mediamtx`
- `dvgrab` (required for `dvgrab` mode)
- FireWire capture device/kernel support for your selected mode

On Debian/Armbian-style systems:

```bash
sudo apt update
sudo apt install -y ffmpeg dvgrab
```
