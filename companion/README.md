# equip-1 Companion

Local dashboard, HTTP API, and BLE provisioning software for the equip-1 portable FireWire DV recorder running on a [Radxa Rock 2F](https://radxa.com/products/rock2/2f/) (RK3528, Cortex-A53, aarch64).

---

## Architecture

Two Go binaries and one third-party streaming server:

| Binary | Role |
|---|---|
| `companion-api` | HTTP server on port 8000. Manages dvgrab/ffmpeg capture, mediamtx lifecycle, RTSP/WHEP preview, file listing, and serves the embedded React web UI. |
| `companion-net` | BLE GATT server + ConnMan WiFi provisioner. Advertises the device over BLE, accepts WiFi credentials from a phone, and hands back the assigned IP address. |
| `mediamtx` | Third-party RTSP/WebRTC/WHEP relay. Receives an RTSP stream from ffmpeg and re-publishes it as a WHEP endpoint for the web UI. |

The React web UI is embedded in `companion-api` at build time via Go's `//go:embed` directive (from `server/web/dist/`).

### Typical startup sequence

```
bluetooth.service   → (BLE adapter ready)
mediamtx.service    → (RTSP/WHEP relay ready on :8554 / :8889)
companion-api.service → (HTTP API + web UI ready on :8000)
companion-net.service → (BLE GATT + ConnMan WiFi provisioning)
```

### BLE provisioning flow

1. Phone scans for BLE device named `equip-1`.
2. Phone reads the **Device Status** GATT characteristic (JSON: recording state, storage).
3. Phone writes WiFi SSID + password to the **WiFi Credentials** characteristic.
4. companion-net calls ConnMan to join the network.
5. Phone reads the **Network Result** characteristic (JSON: IP address).
6. Phone switches to WiFi and opens `http://<ip>:8000` in the browser.

---

## Project layout

```
companion/
  server/           Go source (this module)
    cmd/
      companion-api/  main.go
      companion-net/  main.go
    internal/
      ble/            BLE GATT server (BlueZ5 D-Bus)
      capture/        dvgrab/ffmpeg capture management
      config/         Environment-driven config
      encoders/       H.264 encoder detection
      files/          Capture file listing
      httpapi/        HTTP router, handlers, static serving
      logging/        slog setup
      network/        ConnMan D-Bus client
      proc/           Process lifecycle helpers
      provisioning/   WiFi provisioning state machine
      recorder/       Recording toggle
      stream/         mediamtx, WHEP, MJPEG broadcaster
      sysinfo/        Battery / system info
    web/dist/         Pre-built React assets (embedded)
  web/              React + Vite source
  deploy/           Cross-compiled output (not committed)
    bin/
      companion-api
      companion-net
      mediamtx
```

---

## Prerequisites

| Tool | Version | Notes |
|---|---|---|
| Go | 1.26+ | For building the server |
| Node.js | 18+ | For building the web UI |
| mediamtx | v1.19.2+ arm64 | Download from [bluenviron/mediamtx releases](https://github.com/bluenviron/mediamtx/releases) |
| dvgrab | 3.5+ | On device; installed by the Yocto image |
| ffmpeg | 6.x+ | On device; installed by the Yocto image |

---

## Development setup

### Build the web UI

```bash
cd web
npm install
npm run build
cp -r dist ../server/web/dist
```

The `server/web/web.go` file embeds `web/dist/` into the binary using `//go:embed`.

### Build and run locally (macOS/Linux host)

```bash
cd server
go build ./...
```

For local testing you can run `companion-api` on your host (BLE and FireWire-specific features will not work without the target hardware):

```bash
EQUIP_API_PORT=8000 ./companion-api
```

---

## Cross-compilation for arm64 (Radxa Rock 2F)

Both binaries are pure Go with `CGO_ENABLED=0` — no C toolchain needed.

```bash
cd server

GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
  go build -o ../deploy/bin/companion-api ./cmd/companion-api

GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
  go build -o ../deploy/bin/companion-net ./cmd/companion-net
```

Download mediamtx arm64:

```bash
curl -L https://github.com/bluenviron/mediamtx/releases/download/v1.19.2/mediamtx_v1.19.2_linux_arm64v8.tar.gz \
  | tar -C deploy/bin -xz mediamtx
```

---

## Device deployment (manual / pre-Yocto)

```bash
DEVICE=root@<device-ip>

# Binaries
scp deploy/bin/companion-api  $DEVICE:/usr/bin/
scp deploy/bin/companion-net  $DEVICE:/usr/bin/
scp deploy/bin/mediamtx       $DEVICE:/usr/bin/

# Systemd units (from the meta-firewire-recorder layer)
LAYER=<path-to-meta-firewire-recorder>
scp $LAYER/recipes-core/companion/files/companion-api.service  $DEVICE:/lib/systemd/system/
scp $LAYER/recipes-core/companion/files/companion-net.service  $DEVICE:/lib/systemd/system/
scp $LAYER/recipes-core/companion/files/mediamtx.service       $DEVICE:/lib/systemd/system/

ssh $DEVICE "systemctl daemon-reload && \
  systemctl enable --now mediamtx companion-api companion-net bluetooth"
```

Capture files are written to `/data/captures` by default (override with `EQUIP_CAPTURE_DIR`).

---

## Environment variables

| Variable | Default | Description |
|---|---|---|
| `EQUIP_CAPTURE_DIR` | `~/captures` | Directory for recorded DV files |
| `EQUIP_MEDIAMTX_BINARY` | `mediamtx` | Path to the mediamtx binary |
| `EQUIP_MEDIAMTX_RTSP_URL` | `rtsp://127.0.0.1:8554/live` | RTSP ingest URL |
| `EQUIP_MEDIAMTX_WHEP_PORT` | `8889` | WHEP endpoint port |
| `EQUIP_API_PORT` | `8000` | HTTP API listen port |
| `EQUIP_BLE_NAME` | `equip-1` | BLE advertisement name |
| `EQUIP_FFMPEG_H264_ENCODER` | _(auto)_ | Force a specific H.264 encoder (e.g. `h264_rkmpp`) |
| `EQUIP_RECORDING_CAPTURE_MODE` | `dvgrab` | Startup capture mode: `dvgrab` or `ffmpeg-only` |
| `EQUIP_API_BASE` | `http://127.0.0.1:8000` | Base URL companion-net uses to reach companion-api |

---

## Yocto integration

The `meta-firewire-recorder` layer at `https://github.com/g8row/meta-firewire-recorder` contains a `companion` recipe at `recipes-core/companion/companion_0.1.bb`.

Before building the image, copy the pre-built binaries into the recipe's `files/` directory:

```bash
LAYER=<path-to-meta-firewire-recorder>/recipes-core/companion/files

cp deploy/bin/companion-api  $LAYER/
cp deploy/bin/companion-net  $LAYER/
cp deploy/bin/mediamtx       $LAYER/
```

The recipe installs binaries to `/usr/bin/`, installs all three systemd unit files, and enables `bluetooth.service`. `companion` is already in `IMAGE_INSTALL` in `firewire-recorder-image.bb`.

---

## Hardware

- **SoC**: Rockchip RK3528 (4× Cortex-A53 @ 1.8 GHz)
- **Board**: Radxa Rock 2F
- **WiFi/BT**: AIC8800D80 USB combo adapter (single-radio, USB ID `a69c:8d81`)
- **FireWire**: IEEE 1394 DV camera via Linux `raw1394`/`iec61883` stack
- **OS**: Custom Yocto `scarthgap` image with ConnMan + BlueZ5 + dvgrab + ffmpeg

### AIC8800D80 Bluetooth quirk

The AIC8800D80 reports HCI/LMP version 5.4 but the firmware rejects BT5 extended LE advertising and scanning commands with `-EBUSY`. `companion-net` works around this in two ways:

- **Userspace** (`internal/ble/hciadv.go`): After registering the GATT application with BlueZ, `companion-net` sends legacy `LE_Set_Advertising_*` HCI commands directly via `hcitool`, bypassing BlueZ's extended advertising path. BlueZ still handles incoming GATT connections. **This is the fix actually in production and fully verified on-device.**
- **Kernel-level quirk (not shipped)**: an earlier attempt added a `btusb` kernel patch setting `HCI_QUIRK_BROKEN_EXT_SCAN`. It was removed because (a) it only affects BlueZ's *scanning* HCI path, which this device never exercises (it's a BLE peripheral being scanned by phones, not a scanner itself — the actual bug we hit was on the *advertising* path, already fixed above), and (b) the hand-authored patch had malformed hunk headers that would likely fail `do_patch`. If a kernel-level fix is wanted later, regenerate it as a real `git diff` against this BSP's actual `drivers/bluetooth/btusb.c` rather than hand-typing hunks.

---

## Potential Upgrades

1. **WebRTC data channel**: Replace BLE record control with a WebRTC data channel once the phone is on WiFi. Eliminates BLE round-trips during recording and allows richer control (timecode, clip info) without a separate BLE connection.

2. **h264_rkmpp hardware encoder**: Enable the RK3528 hardware H.264 encoder via `EQUIP_FFMPEG_H264_ENCODER=h264_rkmpp`. The `meta-rockchip` layer packages `rockchip-mpp` and `v4l-rkmpp`; both are already in `IMAGE_INSTALL` when `FIREWIRE_ENABLE_RKMPP=1`. This cuts CPU load and encoding latency for the RTSP stream significantly.

3. **ConnMan tethering → hostapd fallback**: The AIC8800 is a single-radio adapter; simultaneous STA (client) + AP (hotspot) is unreliable. If ConnMan WiFi tethering fails, fall back to a dedicated `hostapd` + `dnsmasq` AP mode. Both packages would need adding to the Yocto image.

4. **mDNS/Bonjour discovery**: Add `avahi-daemon` to the image (note: currently disabled in `firewire-recorder-image.bb`'s `disable_unused_services`). With avahi, the web client can reach the device as `equip-1.local` without needing the IP from BLE.

5. **HTTPS + API token**: Replace the open CORS HTTP API with mTLS or a static bearer token. The embedded web UI and mediamtx WHEP endpoint are currently unauthenticated on the LAN.

6. **Timecode sync**: Read the DV stream timecode from dvgrab (available via `--timestamp` or the raw DV stream headers) and expose it via a BLE characteristic and API field. Enables multi-camera sync workflows.

7. **DV scene / chapter splitting**: dvgrab supports `--timestamp` and scene detection (`--scene`). Automatically split recordings at scene boundaries to produce shorter, independently seekable clips.

8. **iOS Capacitor app**: The Android Capacitor app is shipped (`web/android/`) with full BLE provisioning (`@capacitor-community/bluetooth-le`). An equivalent iOS target needs code-signing setup and an Apple Developer account.

9. **Remote streaming relay**: mediamtx supports SRT output and WebRTC relay to WHIP endpoints. Add a UI control that starts a relay to a public STUN/TURN server and shows a QR code for sharing the live stream externally.

10. **Low-power idle mode**: Disable the WiFi radio between recordings when only BLE control is needed. ConnMan can be told to power off the WiFi interface; companion-net re-enables it on demand when WiFi provisioning is triggered.
