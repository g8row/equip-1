# equip-1 companion — working notes for Claude

Portable DV recorder appliance: FireWire camcorder → RK3528 board (Radxa ROCK 2F)
→ records to microSD, streams a live preview, controlled from an Android app.
Runs a mainline-kernel Yocto image (the `wrynose` meta-layer fork).

**Read `STATUS.md` first** — it tracks what actually works vs. what's blocked,
verified live on hardware. This file is the durable "how it fits together and
what will bite you" reference.

## Layout

- `server/cmd/companion-api` — HTTP API (status, files, recording, streaming,
  network). Spawns `mediamtx` as a child. Listens on `:8000`.
- `server/cmd/companion-net` — BLE GATT server + WiFi/AP control (via ConnMan
  D-Bus). Advertises the device for discovery; accepts WiFi creds / AP toggle.
- `web/` — React app (Vite) wrapped with Capacitor into the Android app.
  Pages: `Connect` (pairing), `Viewfinder` (live + record), `Files`, `Settings`.
- `deploy/` — board deployment bits. Image recipe lives in the separate
  `meta-firewire-recorder` layer; kernel/BT bits in the `meta-rockchip` fork.

## Hardware constraints that shape the whole design

The **AIC8800D80** is a single-radio WiFi+BT combo chip. These are not bugs to
fix in the app — they are physics the app must respect:

1. **Single radio: AP XOR station, never both.** Hosting the hotspot means it
   cannot scan or join WiFi, and vice versa. Any "connect to WiFi" flow must
   scan *before* starting the AP and cache the list. Any AP toggle or WiFi-join
   may sever the connection you're currently using.
2. **BLE extended advertising is broken.** The chip reports BT 5.4 but the
   firmware's extended LE doesn't transmit. `bluetoothd`'s `RegisterAdvertisement`
   fails, so `companion-net` falls back to raw legacy HCI adv (`hciadv.go`).
   `bluetoothd` *also* emits its own connectable adv (name + 16-bit UUIDs) that
   competes on-air with the legacy adv → UUID-filtered scans are unreliable.
   **App-side fix:** the app scans unfiltered and matches the device **name**
   (`equip-1`), which is in every packet. Real fix is a kernel
   `HCI_QUIRK_BROKEN_EXT_ADV` patch (board work, not done yet).
3. **5GHz station association fails** (`status_code=1`); 2.4GHz works. On a
   dual-band SSID ConnMan picks the 5GHz BSSID → join fails. Only 2.4GHz-only
   networks provision reliably today. Driver reg params reset to `00` on init;
   `/lib/firmware/regulatory.db` is missing. Marked "figure out 5GHz later."

## Connection flow (what the app actually does)

Discover over BLE → probe whether the device is reachable on the current LAN
(having an IP ≠ same network) → if not, offer **Join device hotspot** (Capacitor
WiFi `WifiNetworkSpecifier`, `autoRouteTraffic:true` binds app traffic to the AP
gateway `192.168.0.1:8000`) → optionally, over the AP, pick a WiFi network for
the device (list was pre-fetched over BLE before the AP started). AP passphrase
`equip1device` and SSID `equip-1` must stay in sync between `bluez.go` and
`web/src/lib/wifi.js`.

## Gotchas (learned the hard way)

- **Capacitor plugin proxy is thenable-poisoned.** Never `return` a plugin proxy
  as the resolved value of an async function — JS calls `proxy.then` → native
  error. Destructure from the awaited module namespace, await only real methods.
- **BusyBox on the board:** `head -n N` (not `-N`), no `ps aux`/`ss`/`netstat`/
  `timeout`/`curl`/`pkill`. Parse `/proc/net/tcp`; run `curl` from the Mac; use
  `kill` not `pkill`.
- **`mediamtx` needs a config with `paths: all_others:`** or it rejects all RTSP
  publishing (400). Config-less child spawn is the default → WHEP fails.
- **Don't `rmmod`/`modprobe` the AIC WiFi driver** — it wedges the board (needs a
  watchdog reboot).
- The board's "stream ready" health check can read `true` in a brief up-window
  while mediamtx crash-loops — see `web/src/lib/stream.js`.
- **MJPEG on native:** the Android WebView silently blocks a cross-protocol
  `<img src="http://...">` (mixed content) even with `MIXED_CONTENT_ALWAYS_ALLOW`,
  but `fetch()` works. So `MjpegPlayer` on native fetches the multipart mpjpeg
  stream and feeds `<img>` per-frame `blob:` URLs (`lib/mjpeg.js` —
  `createFrameParser` splits on the in-body `Content-length` framing, not the
  response Content-Type/boundary). The web build still uses a direct `<img src>`.
- **Status bar coloring (Android):** web content on this device does NOT paint
  the status bar (the WebView sits below it — a CSS band at `top:0` lands under
  the bar). Use `@capacitor/status-bar`: `setOverlaysWebView(true)` +
  `setStyle(Style.Dark)` (= light icons) + `setBackgroundColor`. `overlay:true`
  is deliberate — `overlay:false` double-counts `env(safe-area-inset-top)` (it
  still reports 40px) and pushes the header down. Nav-bar glyphs are set in
  `MainActivity` via `WindowInsetsControllerCompat`. `systemBars.js` tints the
  bar accent-red while recording.

## Testing on hardware (no rebuild loop)

- Board over SSH: `root@192.168.88.220` (ethernet). Services: `companion-api`,
  `companion-net`, `connman`. Hot-deploy a Go binary: build with
  `CGO_ENABLED=0 GOOS=linux GOARCH=arm64`, `scp -O` to `/usr/bin/`, restart the
  unit. This persists on the rootfs but is NOT in the image — fold real fixes
  back into the meta-layer (STATUS §8/§10).
- Android app over adb + Chrome DevTools Protocol: `adb forward tcp:9222
  localabstract:webview_devtools_remote_<pid>`, then drive the WebView by
  evaluating JS over the CDP websocket (helper: a small `ws_eval.py`). Toggle
  radios with `adb shell svc wifi|bluetooth enable|disable`.
- To force the AP path in testing, the phone must be **wifi-enabled but not on
  the board's LAN** (else the app reaches the board directly and skips the AP).
- **Reaching a server from the WebView in tests:** `adb reverse` + `localhost`/
  `127.0.0.1` does NOT work — Capacitor serves the app from `https://localhost`
  and Chromium won't route loopback fetches out. Use a real LAN IP the phone can
  reach. Over the device AP, the join is a `WifiNetworkSpecifier` **local-only**
  network (no default route) — only the app's own `joinDeviceAp()` flow holds the
  `bindProcessToNetwork`; an out-of-band CDP `connect()` won't stay bound. And
  ConnMan tethering only forwards **specific** gateway ports to AP clients (8000
  reaches the API; arbitrary ports like 8899 don't).

## Design principle

When something in the UI feels off, check it against the hardware matrix above.
The recurring bug class is the UI offering an action the hardware/back end will
refuse (silent WiFi join, AP+station, record with no camera/space). Prefer
disabling or gating the action with an honest reason over letting it fail.
