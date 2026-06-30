# equip-1 Companion — Plan & Progress

> Cross-platform companion for a FireWire DV recorder on a **Radxa Rock 2F** (RK3528, aarch64).
> Live viewfinder + recording control, BLE discovery → WiFi handoff, Nothing-OS-style UI.
> Last updated: 2026-06-30 (session 2).

---

## 1. Product decisions (locked)

| Area | Decision |
|---|---|
| Client platforms | iOS, Android, desktop |
| Client build | **Capacitor** native shell reusing one **React** web UI (native BLE on phone; same UI served by device) |
| Pages | Viewfinder + controls · File browser · Settings · Connect/onboarding |
| Design | Full **Nothing-OS** system: dark canvas, dot-matrix font (Doto), monochrome + single red accent `#cc1e1e` |
| BLE role | Provisioning **+ lightweight controls** (status, WiFi creds, AP control, record start/stop) |
| WiFi model | Join known networks, **fall back to own AP** |
| Backend | **Rewrite in Go** — single static aarch64 binary per service, web UI embedded, native D-Bus for BLE/ConnMan |
| Streaming | Keep **mediamtx** (WHEP/WebRTC ~100ms, MJPEG fallback) — unchanged |
| Device serves UI | Yes — FastAPI→Go binary embeds & serves `web/dist` at `http://<device-ip>:8000` |

---

## 2. Verified device environment (probed + `g8row/meta-firewire-recorder`)

Custom **Yocto** image `firewire-recorder-image` (MACHINE `rockchip-rk3528-rock-2f`), **BusyBox** userland, **systemd** init. **User can edit the image.**

- **Network stack: ConnMan** (+ `wpa-supplicant`, `iw`, `rfkill`, `connman-tools`). No NetworkManager / hostapd / dnsmasq. → AP mode via **ConnMan WiFi tethering** (built-in gdhcp).
- **Bluetooth: bluez5** installed, `bluetooth.service` currently **inactive** (enable it).
- **WiFi/BT chip: AIC8800** USB combo. **Single radio** → AP and station mutually exclusive. ⚠️ **AIC8800 AP mode + ConnMan tethering must be verified on-device — top risk.** Fallback: add `hostapd`+`dnsmasq` to the image.
- **Media:** `ffmpeg`, `x264`, `dvgrab` present; `/dev/fw0` present, but current smoke test reports no attached AV/C camera (`dvgrab`: "Error: no camera exists"; ffmpeg iec61883: "No AV/C devices found."). `h264_rkmpp` only if `FIREWIRE_ENABLE_RKMPP` was set, else `libx264` (also WebRTC-compatible). Encoder probe handles either.
- **mediamtx: NOT in image** — temporarily copied upstream `mediamtx` v1.19.2 linux arm64 to `/usr/bin/mediamtx` for dev testing; still ship it (Go static binary) + systemd unit in the image.
- **Companion app: NOT in image** — ship binaries + units (scp for dev; meta recipe for prod).
- `python3`+`pip` present, package-management enabled. **avahi/mDNS disabled** (client uses same-origin + BLE-handoff IP, not mDNS). **No battery sysfs** → BLE `bat/chg` = null.
- Dev/test SSH: `root@192.168.234.236`.

Go on Linux uses **`github.com/godbus/dbus/v5`** (CGO-free) for ConnMan + BlueZ; cross-compiles cleanly to arm64.

---

## 3. Architecture

One **Go module** `companion/server` → **two static binaries**, plus mediamtx, all systemd-managed and independent:

```
mediamtx.service       RTSP/WHEP streaming engine (shipped binary)
companion-api          HTTP API: record/files/stream/status + EMBEDS & serves web UI   (:8000)
companion-net          BLE GATT server + WiFi (ConnMan) state machine                  (root)
```

`companion-net` is separate so BLE advertising + AP fallback keep the device reachable **even if the API crashes**. It drives recording over localhost HTTP (`/api/record/*`).

Client = one React build shipped two ways: (a) embedded in `companion-api`, served at the device IP; (b) bundled in a Capacitor iOS/Android app adding native BLE provisioning.

### Repo layout
```
companion/
  server/                 # NEW Go backend (replaces api/ Python once parity-verified)
    cmd/companion-api/ cmd/companion-net/
    internal/{config,logging,proc,encoders,capture,stream,recorder,files,sysinfo,network,provisioning,ble,httpapi}
    web/dist/             # React build copied here for //go:embed
  web/                    # React + Vite app (now multi-page + Nothing-OS)
  api/                    # OLD Python backend — reference during port, retire after
  deploy/systemd/         # unit files (to be created)
```

---

## 4. Workstreams & progress

Legend: ✅ done · 🔄 in progress · ⬜ todo

### A. Go backend — parity port (`companion-api`)
- ✅ Go 1.26 installed; module `equip1/companion/server` initialized
- ✅ `internal/config` (env + runtime CaptureMode) — written by hand
- ✅ `logging, proc, encoders, capture, stream (mediamtx/hub/seamless/preview/whep), recorder, files, sysinfo, httpapi, cmd/companion-api`, web embed, `DELETE /api/files`, `GET /api/system/power` — port landed
- ✅ Orchestrator review of seamless-hub port + MJPEG endpoint parity completed; no code changes required from review
- 🔄 On-device smoke test vs Python reference: API/status/config/files/storage/power/static embed pass; encoder selection picks `libx264`; stream/record process cleanup passes when no camera is attached. True WHEP latency and seamless record-without-preview-drop still blocked on attached AV/C camera.

### B. Frontend — multi-page + Nothing-OS (`web/`)
- ✅ **Done, `npm run build` passes.** react-router; pages Viewfinder/Files/Settings/Connect; `AppShell`; extracted `WhepPlayer`/`MjpegPlayer`; `ServerContext` (shared 1.5s poll); `ui/` components (Button/Card/Tab/StatusDot/Toggle) + `tokens.css`
- ✅ React `dist` copied into `server/web/dist`; rebuilt arm64 `companion-api`; device serves embedded root/assets and BrowserRouter deep links.
- ✅ Design tokens: `--bg #0b0b0b`, text `#f5f5f5`/`#8a8a8a`, accent `#cc1e1e`; Doto (dot-matrix) + IBM Plex Mono; dot-grid motifs
- ✅ `api.js` extended: `deleteFile, getNetwork, setWifi, setAp, scanWifi, getPower, getRuntimeDebug`; same-origin base resolution
- ✅ Stream UI errors handled: WHEP/MJPEG now show camera/MediaMTX/encoder/no-frame states instead of blank preview failures.
- ⬜ **Reconcile API contract** (see §5) — frontend currently *guesses* network/power/scan response shapes
- ⬜ Light orchestrator review of extracted WHEP logic

### C. Network + BLE daemon (`companion-net`)
- ✅ `internal/network` — ConnMan via godbus: GetStatus (WiFi + ethernet), ScanWifi, agent+Service.Connect, SetAP tethering
- ✅ `internal/provisioning` — state machine: boot→known→AP-tether fallback; default passphrase; recognises ethernet as connected
- ✅ `internal/ble` — BlueZ5 GATT server + advertisement; WiFi creds write → provisioning.ApplyCredentials; AP control; record control; status notify every 5s
- ✅ `cmd/companion-net` — network manager created first, shared with provisioning + BLE; proper startup order
- ✅ BLE GATT running on-device: adapter `88:00:44:00:04:F4`, name `equip-1`, service UUID advertised
- ✅ **AIC8800 BT**: mainline `btusb` (not vendor `aic_btusb`) creates HCI device; verified on device. Yocto recipe updated to blacklist `aic_btusb` and load `btusb` instead
- ✅ **rfkill-unblock.service**: systemd oneshot runs after sysinit (modules loaded) to unblock AIC8800 WiFi soft-block before bluetooth.service
- ⚠️ **AIC8800 AP mode** via ConnMan tethering returns "Invalid arguments" — single-radio constraint or ConnMan version limitation; AP fallback path is coded but unverified. Hostapd fallback may be needed.

### D. Capacitor native shell + BLE provisioning UX
- ✅ `capacitor.config.ts` (appId `com.equip1.companion`), `@capacitor/android`, `@capacitor/cli`, `@capacitor-community/bluetooth-le@8.2.0`, `@capacitor/preferences`
- ✅ `npx cap add android` + `npx cap sync android` — project at `web/android/`
- ✅ `lib/native.js` — real `Capacitor.isNativePlatform()` detection (lazy, cached)
- ✅ `lib/ble.js` — all GATT UUIDs + `bleInit/scanForDevice/readStatus/writeWifiCreds/writeApControl/subscribeNetResult/readWifiScan/disconnect` (lazy imported, tree-shaken in web build to 1.37 kB chunk)
- ✅ `Connect.jsx` — 6-state BLE machine (scanning→found→connecting→provisioning→connected→error); BLE section hidden on web (`isNative()` gate); AP hotspot instructions; manual IP and LAN discovery preserved
- ✅ `Settings.jsx` — WiFi scan UI with signal bars, connected SSID label, reconnect/use buttons
- ✅ AndroidManifest.xml — all BLE + network permissions (pre-API-31 + API-31+)
- ✅ **AIC8800 BLE fixed**: raw HCI legacy advertising via `hciadv.go` — BlueZ registers GATT app, `hcitool` sends `LE_Set_Advertising_*` directly to bypass broken BT5 extended adv path. Device visible as "equip-1" at -56 dBm with full service UUID in scan response.
- ✅ **Android BLE provisioning tested end-to-end** on real device (Pixel 8): scan found equip-1 → GATT connected → read status characteristic → IP `192.168.88.220` applied to app header automatically. APK at `web/android/app/build/outputs/apk/debug/app-debug.apk` (Java 21 / Temurin required).

### E. Boot / deploy
- ✅ `deploy/systemd/{mediamtx,companion-api,companion-net,rfkill-unblock}.service` created
- ✅ Cross-compiled arm64 (`CGO_ENABLED=0`, `-ldflags="-s -w"`): 8.3 MB companion-api, 7.0 MB companion-net
- ✅ `deploy/deploy.sh` — one-shot script: build → scp → daemon-reload → enable → restart
- ✅ Deployed to `root@192.168.88.220`; all 5 services active after reboot
- ✅ Reboot test passed: all services up within 30s; BLE advertising on boot; API reachable at :8000
- ✅ Yocto bake path: `meta-firewire-recorder/recipes-core/companion/companion_0.1.bb` + `files/`; `kernel-module-aic8800` recipe updated (blacklist aic_btusb, load btusb). Copy binaries to `files/` before bitbake.
- ✅ `deploy/mediamtx.yml` — mediamtx config with `paths: all_others:` (accept any publish path) and logLevel=error; deployed to `/etc/mediamtx.yml` on board
- ✅ `mediamtx.yml` added to Yocto recipe + `deploy.sh`
- ✅ `mediamtx.service` updated: `ExecStart=/usr/bin/mediamtx /etc/mediamtx.yml`
- ✅ mediamtx `IsRunning()` fix: now detects externally-running mediamtx via port 8554 TCP probe (was always false when managed by systemd separately)
- ✅ Camera streaming verified: dvgrab captures DV camera (GUID 0800460101636e92), MJPEG delivers 14 JPEG frames (~48KB each) in 5s, all at /dev/fw1
- ⬜ Production image build (user triggers bitbake after verifying with camera attached)

---

## 5. BLE GATT layout (planned)

One custom 128-bit service, **advertised** (iOS filters by UUID). Base e.g. `e2710000-b5a3-f393-e0a9-e50e24dcca9e`:

| Characteristic | suffix | Props | Payload |
|---|---|---|---|
| Device Status | `0001` | Read, Notify | `{"fw","rec","ip","ssid","ap","bat","chg"}` |
| WiFi Credentials | `0002` | Write (encrypt) | `{"ssid","psk"}` → join |
| AP Control | `0003` | Write | `0x01` on / `0x00` off |
| Record Control | `0004` | Write | `0x01` start / `0x00` stop / `0x02` toggle |
| Network Result | `0005` | Read, Notify | `{"state","ssid","ip","err"}` |
| WiFi Scan (opt) | `0006` | Read | nearby/known SSIDs |

Constraints: short name `equip-1-XXXX` in scan response; `requestMtu(185)`; notified JSON < ~150 B; long-write for creds; `encrypt-write` (Just-Works) on sensitive chars.

---

## 6. Open items / risks to reconcile

- **API contract**: ✅ resolved — `GET /api/network` → `{mode,ssid,ip,ap}`, `POST /api/network/wifi` → `{ssid,psk}`, `POST /api/network/ap` → `{enabled,ssid,passphrase}`, `GET /api/network/scan` → `{networks:[{ssid,strength,state}]}`, `GET /api/system/power` → `{battery,charging}`. All wired in httpapi; frontend api.js matches.
- **AIC8800 AP mode** via ConnMan tethering — verify early; hostapd+dnsmasq image fallback ready.
- **SPA deep links**: backend `static.go` must serve `index.html` fallback for non-`/api` routes (BrowserRouter) — confirm in review.
- **Camera attachment**: `/dev/fw0` alone is not enough to prove a DV camera is online; current board reports no AV/C device to both dvgrab and ffmpeg iec61883. Re-test real streaming/recording with the camera powered/connected.
- **Encoder**: confirmed current device selects `libx264`; `h264_v4l2m2m` is listed but unusable, and `h264_rkmpp` is absent in this image.
- **Security (deferred, flagged)**: API is `CORS *` + no auth on `0.0.0.0`; BLE writes `encrypt-write` + optional PIN; AP ships per-device WPA2 PSK; `companion-net` runs as root (validate D-Bus args, no shell interpolation).

---

## 7. Verification (target)

1. ✅ `cd server && go build ./... && go vet ./... && go test ./...`; `GOOS=linux GOARCH=arm64 go build ./...`.
2. ✅ `cd web && npm run build`; copy `dist`→`server/web/dist`; rebuild Go; confirm embedded UI serves root/assets/deep links and `/api/*` parity.
3. ✅ `root@192.168.88.220` (new IP): all API routes verified; reboot resilience confirmed; BLE GATT server advertising on boot; WiFi scan returns real networks. Still needs: WHEP/seamless-record parity with camera attached; ConnMan WiFi station connect tested; BLE WiFi handoff end-to-end; AP mode verification (currently errors).
4. ✅ `npx cap sync android` + `gradlew assembleDebug` (Java 21); Android BLE provisioning flow tested end-to-end on real Pixel 8 device. iOS pending code-signing / Apple Developer account.
