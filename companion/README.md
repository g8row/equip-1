# equip-1 companion (localhost prototype)

This folder is a first-pass local prototype for testing the companion app flow before Bluetooth/Wi-Fi pairing.

## What is included

- `api/`: FastAPI server with recorder status + basic control endpoints
- `web/`: React (Vite) dashboard for localhost testing
- Web includes: automatic API server select, manual server override, and live MJPEG preview panel

## Server autodiscovery

The web app tries in this order when you click `Auto Select`:

1. Known local addresses (`VITE_API_BASE`, `127.0.0.1`, `localhost`, browser host + `:8000`)
2. LAN scan on likely private subnets (`192.168.1.x`, `192.168.0.x`, `10.0.0.x`, `10.0.1.x`) plus prefixes inferred from your current host/manual input

Discovery probes `GET /health` and expects `{ "ok": true }`.

Note: this is browser-based scanning, so discovery speed depends on network size and browser request limits.

## Run API

```bash
cd companion/api
python3 -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt
uvicorn main:app --reload --host 0.0.0.0 --port 8000
```

Optional env var:

- `EQUIP_CAPTURE_DIR`: directory to scan for `.dv` files (defaults to `~/captures`)

## Run Web App

```bash
cd companion/web
npm install
npm run dev
```

The app runs on `http://localhost:5173` and calls `http://127.0.0.1:8000` by default.

To point the web app at another API host:

```bash
VITE_API_BASE=http://<device-ip>:8000 npm run dev
```

## API endpoints

- `GET /health`
- `GET /api/status`
- `POST /api/record/toggle`
- `POST /api/record/start`
- `POST /api/record/stop`
- `GET /api/storage`
- `GET /api/files`
- `GET /api/files/download/{name}`
- `GET /api/stream/requirements`
- `GET /api/stream/mjpeg`
- `GET /api/debug/runtime`

## Stream startup behavior

The UI now waits for server discovery/selection and a successful initial status refresh before opening the MJPEG stream.

- This avoids race conditions where stream startup was attempted while server discovery was still in progress.
- The stream card includes a `Restart Stream` button for recovery.

## Debugging stuck shutdowns

If Uvicorn reports `Waiting for background tasks to complete`, query:

- `GET /api/debug/runtime`

This returns active request metadata plus active stream worker PIDs (`ffmpeg`/`dvgrab`) so you can identify what is still running.

On app shutdown, the API now force-cleans stream workers, direct preview workers, and recorder processes and logs a runtime snapshot.

## Stream prerequisites

For the live stream endpoint, the API host needs:

- `dvgrab` installed
- `ffmpeg` installed
- A camera accessible at `/dev/fw1`

Quick install on Debian/Armbian:

```bash
sudo apt update
sudo apt install -y dvgrab ffmpeg
```

## Current recording behavior

Recording is now wired to actual `dvgrab` + `ffmpeg` processes.

- During recording, the backend writes `.dv` files and simultaneously exposes preview frames.
- Live preview remains available while capture continues.

## Streaming architecture (FIFO-based)

The stream preview pipeline has been refactored to use **named FIFO pipes** instead of UDP. This provides reliable, observable streaming with zero packet loss.

### Why FIFO over UDP?

- **Reliable**: FIFO guarantees all frames reach the client (no silent packet drops)
- **Observable**: Process failures are detectable and logged
- **Simpler**: Native OS mechanism, no buffer overflow surprises
- **Backpressure**: Natural flow control (writer blocks if client is slow)

### Architecture

**Recording mode** (concurrent recording + preview):
```
dvgrab [raw DV]
  ↓
ffmpeg mux [tee output to 2 targets]
  ├─ Track 1: /home/user/captures/capture_20260327_114523.dv (disk file)
  └─ Track 2: /tmp/equip_preview_stream.fifo (named pipe)
              ↓
         ffmpeg [per-client reader]
              ↓
         MJPEG transcode
              ↓
         Browser preview
```

**Idle mode** (direct preview, no recording):
```
dvgrab [raw DV]
  ↓
ffmpeg [real-time transcode to MJPEG]
  ↓
Browser preview
```

### Health monitoring

The API monitors mux process health during streaming:
- Every read-cycle checks if mux process is alive
- If mux crashes, stream terminates cleanly (no timeout hang)
- Exponential backoff prevents CPU thrashing on repeated failures
- All events logged with PIDs and error codes

### Troubleshooting stream issues

**Stream stops after starting recording**:
- Check logs: `tail ~/captures/companion-api.log | grep stream-mux-process-died`
- Ensure mux process has sufficient resources (CPU, memory)
- Verify FIFO exists: `ls -l /tmp/equip_preview_stream.fifo`

**FIFO creation errors**:
- Check `/tmp` is writable: `touch /tmp/test && rm /tmp/test`
- Set alternate temp dir via env: `TMPDIR=/var/tmp uvicorn main:app --port 8000`

**Multiple clients all disconnect simultaneously**:
- This should not happen with FIFO (each reader is independent)
- Check mux aliveness: `ps aux | grep ffmpeg | grep tee`

See [STREAM_ARCHITECTURE_REFACTOR.md](../STREAM_ARCHITECTURE_REFACTOR.md) for detailed design documentation.
