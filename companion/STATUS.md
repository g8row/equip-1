# equip-1 — Capability Status

_Last verified: 2026-07-02, on Radxa ROCK 2F (RK3528), kernel 6.18.24-yocto-standard, image built from the `wrynose` meta layers with the nftables ConnMan backend. Board on ethernet at `192.168.88.220`; test phone = Android (Pixel-class) over ADB._

This file records what is **actually tested and working** versus implemented-but-unverified versus broken. "Tested" means exercised on real hardware this session with the evidence noted — not "the code looks right."

Legend: ✅ tested working · 🟡 partial / flaky · ❌ broken · ⬜ not implemented · 🔧 fixed in source, not yet in a built+flashed image

---

## 1. Connection model (important — read first)

The device can be reached three ways, and **"the board has an IP" does NOT mean the phone can reach it.** Reachability depends on both ends:

**Board network modes:**
- **A. Hosting its own AP** — `wlan0` in AP mode, bridge `tether` = `192.168.0.1/24`, its own DHCP + NAT. Reachable at `192.168.0.1:8000` *only by a client joined to that AP*.
- **B. Joined an existing WiFi (client)** — has an IP on that LAN. Reachable only by devices on the *same* LAN.
- **C. Ethernet only** — has a LAN IP (e.g. `192.168.88.220`). Same-LAN reachability only.
- **D. Offline** — no IP.

**Phone states:** on the same WiFi/LAN as the board · on a *different* WiFi · WiFi off (cellular only) · joined the board's AP.

**Reachability matrix:**

| Phone | Board | HTTP API reachable? |
|---|---|---|
| Same WiFi/LAN as board | B or C | ✅ via board IP |
| Different WiFi | B or C | ❌ board has an IP but it is unroutable from the phone |
| WiFi off / cellular | any | ❌ (except BLE) |
| Joined board's AP | A | ✅ at `192.168.0.1:8000` |
| any | D | ❌ |

**BLE works independently of IP** (proximity radio) and is the bootstrap channel: discover the device, read its status, and provision it — *before* any IP path exists.

### Single-radio constraint (AIC8800) — verified live 2026-07-02

The board has **one** WiFi radio (`wlan0`). It can be in exactly one mode at a time:
- **AP mode** — hosts `equip-1`, phone joins directly (`192.168.0.1`). **Cannot scan or join other WiFi** — `connmanctl scan wifi` → `Device or resource busy`.
- **Station mode** — scans + joins a WiFi network (verified: sees 5 nearby APs). No AP.

**Implications for the flow:**
1. You cannot get a fresh WiFi scan while the AP is up. To let the phone pick a network *after* joining the AP, the board must **scan before entering AP mode and cache the list**, then serve the cache over HTTP (the live `/api/network/scan` endpoint returns busy in AP mode). ⚠️ Not yet implemented server-side.
2. Applying a chosen network is a **mode switch**: board drops the AP (phone loses the `192.168.0.1` link) → station → joins the network → gets a LAN IP. The phone must then rejoin that same WiFi and re-find the board (LAN probe / mDNS). 
3. The board *can* host the AP and use ethernet at the same time (different interfaces), so a bench board on ethernet gives the AP-joined phone internet via NAT.

### Revised connection flow (hardware-correct)

1. **Direct LAN first:** if a known/discoverable board IP responds on the phone's current network → use it. (both-on-same-WiFi / same ethernet LAN)
2. **Else BLE discover** + read board status. If the board reports an IP **and a probe of it succeeds** → connected. (shared subnet — must *probe*, not assume; see Issue #7)
3. **Else provision.** Two routes:
   - **(a) Onto a shared WiFi over BLE** — board scans (station, radio free during BLE), sends list over BLE, phone picks + password, board joins; phone (on that WiFi) finds it. No AP. Best when a usable WiFi exists for both.
   - **(b) Direct AP link (primary per product choice)** — board starts `equip-1` AP, phone joins via WifiNetworkSpecifier (one system-dialog tap), talks at `192.168.0.1:8000`. Fully usable here (stream/control/files). *Optional* onward "connect device to WiFi" uses the pre-scanned cached list + the mode-switch in implication #2.

**Consequence for the app:** the current logic "if the device reports an IP → mark connected" is wrong (see Known Issues #3). The app must (a) actually probe the reported API and only claim connected if it responds, and (b) when there is no shared network, drive the AP-hosting path and then let the user pick a WiFi for the board to join *over that AP*.

**Intended full pairing flow (target design, not yet built):**
1. Discover device over BLE.
2. If phone + device already share a LAN and the API answers → done.
3. Otherwise: tell the device to host its AP (BLE `ap_control`), phone joins `equip-1` AP (one-tap system dialog via WifiNetworkSpecifier).
4. Over the AP (`192.168.0.1:8000`), phone shows the WiFi networks the device can see and lets the user pick one + enter its password.
5. Device joins that WiFi; both end up on the same LAN; phone can drop the AP.

---

## 2. Networking / connectivity

| Capability | Status | Evidence |
|---|---|---|
| Ethernet + companion-api over LAN | ✅ | `ssh root@192.168.88.220`, app connected to `192.168.88.220:8000`, `/api/status` served |
| WiFi AP tethering (WPA2, with passphrase) | ✅ | Phone joined `equip1-test`, got DHCP lease `192.168.0.2`, full internet through NAT; `tether=192.168.0.1/24`, `wlan0 type AP` ch.1 |
| NAT via nftables backend | ✅ | `nft_masq`/`nft_chain_nat`/`nf_nat` loaded, `ip_forward=1`, phone had working internet, no errors in ConnMan journal (contrast: old iptables backend failed with "iptables support missing") |
| AP with **open** (no-passphrase) network | ❌ | ConnMan silently refuses: `Tethering` stays `False`, `wlan0` stays `managed`. Adding any WPA2 passphrase makes it come up immediately. This is why the app-driven AP never started (see #4). |
| Board joins an existing WiFi (client) — 2.4GHz | ✅ | Proven live: standalone wpa_supplicant pinned to 2.4GHz joined `gurowifi` (`wpa_state=COMPLETED`, freq 2452) and got DHCP `192.168.88.183`. Full station path works on 2.4GHz. |
| Board joins an existing WiFi (client) — 5GHz / dual-band | ❌ | The board picks the 5GHz BSSID of a dual-band network and the **AP rejects the association** (`CTRL-EVENT-ASSOC-REJECT status_code=1`, tried 5765 MHz). See Issue #9 — this breaks ConnMan `Connect()` for any network the phone/router exposes on 5GHz (`connect: Operation timeout` / `Input/output error`). |

---

## 3. Bluetooth / BLE

| Capability | Status | Evidence |
|---|---|---|
| `hci0` via mainline `btusb` (vendor `aic_btusb` blacklisted) | ✅ | `lsmod`: `btusb` loaded, `aic_btusb` absent; `hciconfig` shows HCI/LMP 5.4 |
| rfkill unblocked so radios usable | 🟡 | Works after `rfkill-unblock` + udev rule, but BT registers its rfkill entry *after* the boot oneshot; the udev rule (`99-rfkill-unblock.rules`) is the intended catch. Needs a clean fresh-boot confirm (this session it was unblocked, but had needed a manual nudge on an earlier boot). |
| BLE advertising (device discoverable) | 🟡 improved | Root cause: chip reports BT5.4 ext-adv but firmware's extended LE is broken, so `bluetoothd`'s `RegisterAdvertisement` fails and companion-net falls back to raw legacy HCI adv (`hciadv.go`). Two fixes this session: (1) legacy adv now puts the 128-bit service UUID in the **primary** ADV packet (was scan-response only — Android's UUID filter never matched it; verified on-air via btmon that the UUID is now in the primary packet); (2) **bluetoothd still emits its own connectable adv** (name + standard 16-bit UUIDs) that competes on-air with the legacy adv, so a UUID-filtered scan still catches the wrong packet ~half the time. The real fix for that is the kernel `HCI_QUIRK_BROKEN_EXT_ADV` (Issue #1). Mitigated app-side — see next row. |
| BLE discovery from the app | ✅ | **Fixed + tested reliable.** `ble.js` now scans **unfiltered and matches on device name** (`equip-1`), which is present in *every* on-air packet, instead of relying on the UUID filter that the bluetoothd-vs-legacy conflict defeats. Discovery now succeeds consistently (previously failed most attempts this session). |
| BLE GATT connect + read status | ✅ | Works once discovered (name-scan made discovery reliable). |
| BLE `ap_control` → start AP | 🔧 | Board handler was passing an **empty** passphrase (open AP → ConnMan rejects). Fixed in source to use `equip1device`; the fixed `companion-net` binary is deployed on the board (`strings` confirms), but not yet E2E-tested through the app because of BLE discovery flakiness |
| BLE `wifi_creds` → provision board onto WiFi | ⬜ | Not tested |

**The clean fix for the flakiness** is a real kernel `btusb` patch setting `HCI_QUIRK_BROKEN_EXT_ADV` + `HCI_QUIRK_BROKEN_EXT_SCAN` for `a69c:8d81`, so `bluetoothd` itself uses legacy LE and the two paths stop fighting. The earlier hand-written patch was removed (malformed diff, would fail `do_patch`); it needs regenerating against the BSP's real `btusb.c`.

---

## 4. Companion app (Android)

| Capability | Status | Evidence |
|---|---|---|
| App loads, nav, UI render | ✅ | Driven via Chrome DevTools; Viewfinder/Files/Setup/Link all render |
| Offline / device-unreachable detection | ✅ | With phone WiFi off, app shows "Can't reach the device — check it's powered on and on the same network" instead of raw errors |
| MediaMTX raw-error translation | ✅ | Verified live: raw `path 'live' is not configured` JSON now shows "MediaMTX isn't ready yet…" |
| LAN auto-discover / manual address entry | 🟡 | Connected over LAN earlier; not re-verified across all subnet cases |
| Phone joins board's AP (WifiNetworkSpecifier) | ✅ | `@capgo/capacitor-wifi` v8.4.0 wired via `src/lib/wifi.js`. **Tested end-to-end on the phone:** BLE discover → "choose-route" → "Join device hotspot" → `ap_control` over BLE starts the AP → `CapacitorWifi.connect({autoRouteTraffic:true})` shows the one-tap system dialog (auto-approved on repeat) → app's traffic binds to the AP → reaches `192.168.0.1:8000`, dashboard/status/files all load over the hotspot. |
| Post-AP-join network selection (pick WiFi for the device) | ✅ app-side (board-limited) | **Built + tested end-to-end.** On the AP, the app shows a WiFi list (pre-fetched over BLE *before* the AP started, since the single radio can't scan in AP mode), user picks + enters password, app POSTs to `/api/network/wifi` over the AP (verified: request arrived from the phone at `192.168.0.2`), then guides the user to rejoin that network and re-finds the device on the LAN. Board-join itself works for **2.4GHz-only** networks; dual-band returns 502 (Issue #9 — the 5GHz limitation). Falls back to manual SSID entry if the pre-scan list is empty. |
| App gateway address for AP | ✅ | Fixed `192.168.1.1` → `192.168.0.1` (ConnMan's actual tether bridge); verified reachable from the phone over the AP. |
| App connection-state logic (probe before "connected") | ✅ | Fixed: `Connect.jsx` now probes the device's self-reported API and only claims connected if it responds (was declaring connected on any reported IP). Routes to `choose-route` otherwise. |

---

## 5. Streaming / recording

| Capability | Status | Evidence |
|---|---|---|
| mediamtx running | ✅ (as child) | mediamtx PID 301 is a **child of companion-api** (PID 287), spawned via `exec.Command` in `internal/stream/mediamtx.go`. Not a systemd unit on the current image. |
| MJPEG streaming E2E | ✅ | With a DV camera on `/dev/fw1`: `GET /api/stream/mjpeg` returned **14 valid JFIF frames in ~3s (~5fps)**, ~580KB, decoded to a real 960×768 image. Full pipeline (dvgrab → ffmpeg → mjpeg → HTTP) works. |
| WHEP (WebRTC) streaming E2E | ❌ → ✅ with config | **Root-caused:** mediamtx rejects *every* RTSP publish with `400 Bad Request` (reproduced with a clean synthetic H264 source, so not DV/codec-specific). Cause: companion-api spawns mediamtx with **no config arg** (`exec.Command(m.binary)`, `internal/stream/mediamtx.go:64`) and `/etc/mediamtx.yml` is **not installed** on this image, so mediamtx runs config-less and its default rejects publishing. Running mediamtx manually with the recipe's `mediamtx.yml` (`paths: all_others:`) → publish succeeds (`stream is available and online, 1 track H264`) and WHEP reads from `live`. See Known Issue #8. |
| Recording to microSD | ⬜ | Not tested this session (storage healthy: 14.3 GB free of 15.4 GB) |

---

## 6. System / image

| Capability | Status | Evidence |
|---|---|---|
| `bitbake firewire-recorder-image` builds cleanly (nftables) | ✅ | Confirmed by user after switching ConnMan to the nftables firewall backend + matching nft kernel modules |
| Core services active | ✅ | `companion-api`, `companion-net`, `connman`, `bluetooth` all `active` |
| Unused services disabled (rpcbind/avahi/ofono) | ✅ | Verified disabled (the `systemctl --root` fix replaced the broken SysV `find rc*.d` approach) |
| rootfs auto-expand on first boot | ✅ | dmesg this boot: `EXT4-fs resizing filesystem from 210040 to 3885435 blocks` → `resized`. The `sgdisk -e` fix (relocate stale GPT backup header) worked. |

---

## 7. Known issues / bugs

1. **BLE advertising flakiness (🟡, biggest blocker).** BlueZ ext-adv vs userspace legacy HCI fight → `0x0C` Command Disallowed → intermittent discovery. Real fix = kernel `HCI_QUIRK_BROKEN_EXT_ADV`+`EXT_SCAN` patch (regenerate against real `btusb.c`).
2. **`mediamtx.service` in the companion recipe conflicts with companion-api's child mediamtx** → both bind port 8000 → crash-loop. The current running image happens not to have the unit, so mediamtx is fine — but the recipe (`companion_0.1.bb` installs + enables `mediamtx.service`) will reintroduce the conflict on the next build. **Fix: drop `mediamtx.service` from the recipe** since companion-api owns mediamtx's lifecycle.
3. **App wrongly equates "device has an IP" with "connected"** (`Connect.jsx`): it also calls `setBleState("connected")` regardless of whether `refresh()` actually reached the API. Fails silently when phone and board are on different networks. Must gate on a real probe.
4. **AP-handoff was fully broken (🔧 fixed in source):** empty passphrase (ConnMan rejects open tethering) + wrong gateway IP. Both fixed in companion source; `companion-net` redeployed; app-side rebuild pending.
5. **No phone-side WiFi join (⬜):** AP handoff is manual. Plugin installed, flow not wired.
6. **rfkill BT soft-block boot race (🟡):** needs a clean fresh-boot confirmation that the udev rule catches the late-registering BT rfkill entry.
7. **App "has IP → connected" false-positive (see §1):** must probe the API before claiming connected.
8. **mediamtx runs without a config → WHEP publish 400 (❌):** companion-api spawns mediamtx with no config file, and `/etc/mediamtx.yml` isn't installed, so mediamtx's config-less default rejects all RTSP publishing. **Fix (two parts):** (a) install `mediamtx.yml` and have companion-api launch mediamtx with it — either `exec.Command(m.binary, configPath)` in `internal/stream/mediamtx.go`, or drop the file where mediamtx auto-searches (`./mediamtx.yml` in companion-api's CWD, or `/etc/mediamtx/mediamtx.yml`); (b) keep §7-issue-#2 in mind (don't also enable the standalone `mediamtx.service`). MJPEG is unaffected (it doesn't use mediamtx).
9. **Board picks 5GHz on dual-band networks and fails; 2.4GHz works (⚠️ "figure out 5GHz later").** Fully diagnosed live:
   - The AIC8800 joins **2.4GHz** networks fine — associate + DHCP confirmed (`wpa_state=COMPLETED`, DHCP `192.168.88.183`).
   - **5GHz** association is rejected by the AP (`CTRL-EVENT-ASSOC-REJECT status_code=1`, e.g. 5765 MHz).
   - **Consequence by network type:**
     - **2.4GHz-only SSID** → ConnMan sees only 2.4GHz BSSIDs → joins fine. **WiFi provisioning works today.**
     - **Dual-band SSID (same name on 2.4+5GHz)** → ConnMan/wpa_supplicant picks the (stronger) 5GHz BSSID → `Connect()` fails (`Operation timeout` / `Input/output error`).
   - **Why forcing 2.4GHz is hard** (all tried live, none clean): driver `country_code`/`custregd`/`ccode_channels` module params reset to `00` on init (firmware-controlled); `/lib/firmware/regulatory.db` is **missing** so `iw reg set` is a no-op; the driver announces *"PERMISSIVE CUSTOM REGULATORY RULES"* (allows all bands); wpa_supplicant global `freq_list` restricts its own scans but ConnMan drives band selection and pins the 5GHz BSSID anyway (with `freq_list` set, ConnMan pinned a 5GHz BSSID that `freq_list` then excluded → *no* association attempt at all).
   - **Decision (per user, 2026-07-02): ship 2.4GHz support as-is, defer 5GHz.** WiFi provisioning is usable for 2.4GHz-only networks; dual-band is the "figure out 5GHz later" item.
   - **Candidate real fixes (board/driver work, not app):** a driver-level 2.4GHz channel restriction that survives firmware init; or a ConnMan/wpa_supplicant patch to prefer 2.4GHz BSSIDs; or fixing the driver's 5GHz-station association. Also ship `regulatory.db` (packaging bug: `wireless-regdb` is in the image but the db isn't in `/lib/firmware`).

---

## 8. Fixes applied this session

**Manual fixes on the live board (for testing — must be rolled into the image/meta layer):**
- `companion-net` binary hot-deployed with the `ap_control` passphrase fix (`strings` confirms `equip1device`). The image's own binary predates this.
- mediamtx config: **not** persisted on the board — I only ran mediamtx manually with the recipe `mediamtx.yml` to diagnose the WHEP 400 (Issue #8). The image still spawns it config-less; WHEP stays broken until the image installs the config + companion-api passes it.

**companion source (committed to working tree, not yet built into the image):**
- `internal/ble/bluez.go`: `ap_control` sets passphrase `equip1device` (was empty → ConnMan rejected the AP).
- `internal/ble/hciadv.go`: legacy LE adv now carries the 128-bit service UUID in the **primary** ADV packet (flags + UUID + name, fits 31 bytes for `equip-1`), not the scan-response. `companion-net` hot-deployed to the board with this.
- `web/src/lib/ble.js`: **name-based discovery** — unfiltered scan matching `equip-1` by name (robust against the bluetoothd-vs-legacy adv conflict). This is what made discovery reliable.
- `web/src/pages/Connect.jsx`: probe-before-connected fix; reworked BLE flow (`choose-route` / `starting-ap` / `joining-ap` / `ap-connected`); AP gateway `192.168.0.1`; **post-AP network selection** (`ap-wifi-select` / `ap-wifi-password` / `ap-provisioning`) with BLE pre-scan carried into the AP session; AP-binding released on disconnect.
- `web/src/pages/Settings.jsx`: single-radio honesty — WiFi form warns "2.4GHz works best, 5GHz unsupported" and, when reached over the AP, that joining WiFi ends the current connection; AP toggle explains the one-radio tradeoff and confirms before turning the AP off while connected through it.
- `web/src/lib/systemBars.js` (new) + `App.jsx` + `AppShell.jsx` + `MainActivity.java`: **Android status/nav bar theming.** Installed `@capacitor/status-bar@^8`. Status bar is set dark (`#0b0b0b`) with light icons via `setStyle(Style.Dark)` + `setOverlaysWebView(true)` + `setBackgroundColor` (verified on-device: `overlay:true` gives a solid-colored bar with content correctly offset below it; `overlay:false` double-counted the top safe-area inset and pushed the header down). Nav-bar glyphs forced light in `MainActivity` via `WindowInsetsControllerCompat.setAppearanceLight*Bars(false)`. **Feature:** the status bar tints accent-red while recording (AppShell effect → `setRecordingBar`), an iOS-style system-level REC cue — verified end-to-end on device by simulating a recording status through the real context.
- `web/src/pages/Viewfinder.jsx`: Record button now disabled (with a reason line) when a start would just fail — device unreachable, no FireWire camera, or <200MB free (matching the backend's own start guard). Stop stays available while recording. Verified on-device (unreachable → "Can't record: Device unreachable.").
- `web/src/lib/wifi.js` (new): `@capgo/capacitor-wifi` wrapper. ⚠️ Gotcha fixed: never return the Capacitor plugin proxy as an awaited value — the proxy forwards `.then` to native and JS thenable-adoption calls it → `"CapacitorWifi.then() is not implemented on android"`. Destructure from the awaited module namespace instead.
- `web/package.json`: added `@capgo/capacitor-wifi@^8.4.0`.

**meta layers (`wrynose`) — from earlier, build confirmed clean by user:**
- nftables ConnMan backend + nft kernel modules (AP NAT); `sgdisk` rootfs-expand; rfkill udev rule; `systemctl`-based service disabling.

## 9. App audit (rethinking each part the way the connection flow was reworked)

The connection flow needed a rethink because the UI assumed things the hardware
can't do (silent WiFi join; AP + station at once; "has IP" = connected). The
same lens applied to the rest of the app:

- **Connect / pairing** ✅ reworked + tested — see §1, §3, §4.
- **Settings** ✅ reworked — WiFi form and AP toggle now reflect the single-radio
  reality (2.4GHz-only, AP-XOR-station, "this action ends your connection").
- **Viewfinder / recording** ✅ reworked — Record disabled when the backend would
  reject it (unreachable / no camera / <200MB), with a reason.
- **System bars (Android)** ✅ done — status bar dark + light icons, matches the
  canvas; tints accent-red while recording. Nav-bar glyphs forced light.
- **Files** ✅ reviewed — already sound (offline states, delete confirm, native-share
  gating by size). No change needed.
- **Stream mode fallback** ✅ done — when WebRTC/WHEP is unavailable but the camera
  is present, the Viewfinder shows an inline **Switch to MJPEG** button next to the
  error instead of making the user go to Settings. Verified on device (simulated
  `whep_available:false`). Offered on all platforms now that native MJPEG works (below).
- **Native MJPEG preview** ✅ implemented + parser-verified — `lib/mjpeg.js` parses the
  multipart mpjpeg byte stream (fetch → `createFrameParser` → per-frame JPEG **blob:
  URLs** → `<img>`), working around the Android WebView blocking a cross-protocol
  `<img src="http://...">`. `MjpegPlayer` uses it on native (web keeps the direct
  `<img src>`). The parser is unit-tested in Node against a live ffmpeg-mpjpeg stream:
  extracts valid JPEGs, byte-identical whether fed one chunk or in 7-byte fragments.
  Also fixed a latent bug (the 12s no-frame timeout fired even on a working stream —
  now cleared once the first frame loads). **On-device visual not yet confirmed**: the
  parser + blob-`<img>` rendering (already used by Files thumbnails) + fetch-over-AP are
  each proven, but the assembled render couldn't be screenshotted because the test
  harness can't hold the Android `WifiNetworkSpecifier` local-only network binding
  outside the app's own connection flow (needs a real device session on the AP/LAN).
- **Settings "Preview & capture" card** ✅ done — the two dropdowns were unlabeled;
  now each has a heading ("Live preview" / "Recording capture"). Capture modes
  relabeled from jargon to honest names derived from the server: `dvgrab` →
  "Shared with preview (default)" (taps the always-on seamless hub), `ffmpeg-only`
  → "Separate recorder" (independent lossless-DV writer), with a one-line advanced
  explanation. Verified on device.
- **Test coverage added** ✅ — (1) Go golden tests for the `capture` package's
  ffmpeg/dvgrab argv builders (`capture_test.go`) — locks in the exact command
  construction the whole stream/record pipeline depends on; `go test ./...` green.
  (2) Added **vitest** to the web project (it had no test runner) with `npm test`, and
  `mjpeg.test.js` covering the MJPEG frame parser: byte-exact extraction, chunking
  invariance (one-byte-at-a-time == single-chunk), partial-frame buffering, and the
  unbounded-buffer guard. 4/4 pass; not bundled into the production build.
- **Thumbnail component** ✅ reviewed — already correct (cancels in-flight fetch,
  revokes blob URLs on cleanup); no leak like the players had. No change.
- **Accessibility pass** ✅ (started) — form inputs relied on `placeholder` only (not a
  reliable accessible name, and it vanishes on typing). Added `aria-label` to all six
  text/password inputs (Settings WiFi, Connect WiFi/SSID/manual-server) and to the live
  video element; the capture/preview selects already got real `<label>`s. Verified inputs
  now report an accessible name.
- **Manual server entry** ✅ done — the "Device address" field now accepts a bare
  host/IP (e.g. `192.168.1.5:8000`); `applyManual` assumes `http://` when no scheme is
  given instead of failing on a scheme-less (relative) URL. Verified on device.
- **WiFi password validation** ✅ done — a WPA passphrase must be 8–63 chars; a 1–7
  char entry is guaranteed to be rejected. Added `pskError()` (`lib/wifi.js`) and gated
  all three credential-entry points (Connect BLE join, Connect over-AP provision, Settings
  WiFi form): the submit button disables with an inline reason, and the handlers guard the
  Enter-key path too. Verified on device (5 chars → disabled + "at least 8 characters";
  8 chars → enabled, warning clears).
- **WhepPlayer robustness** ✅ done — fixed two real bugs in the WebRTC player: (1) the
  live-edge seek `progress` listener was re-added on every `connect()` (each retry, up to
  15×, and every manual reconnect), stacking handlers that fight over `currentTime` —
  moved to a one-time mount effect; (2) `onconnectionstatechange` flashed "Stream error"
  on `"closed"`, which also fires on *intentional* disconnect/reconnect/retry — now the
  handler ignores PeerConnections we've deliberately replaced (`pcRef` nulled before
  `close()`) and only errors on a genuine `"failed"`. Exercised on device (3 rapid
  reconnects, no uncaught errors, graceful error state).
- **Settings offline gating** ✅ done — the capture-mode dropdown and "Restart
  services" button no longer stay live when the device is unreachable (they'd just
  error). Both disable on `reachable === false`, with honest text ("Restart services
  (device offline)", "Connect to the device to change this"). Verified on device.
- **Stream "off" option** ✅ reviewed — legitimate (saves battery/bandwidth while still
  recording); the Viewfinder shows a clear "Preview paused" placeholder. No change.
- ⬜ **Still to audit:** thumbnail/last-recording freshness after a stop (needs a live
  camera to verify).

## 10. Next steps (planned, not started)

1. Kernel `btusb` quirk patch (`HCI_QUIRK_BROKEN_EXT_ADV` + `EXT_SCAN`) so BlueZ does legacy adv natively with the 128-bit UUID — removes the bluetoothd-vs-legacy-HCI on-air conflict entirely (#1). App-side name-scan already makes discovery reliable in the meantime.
2. Force 2.4GHz for station joins so dual-band networks work (#9) — no clean ConnMan/wpa_supplicant/reg knob found; likely a driver channel restriction. 2.4GHz-only networks already work today.
3. Ship `regulatory.db` into `/lib/firmware` (packaging bug — `wireless-regdb` present but db missing).
4. Remove `mediamtx.service` from the companion recipe (#2); ship the `paths: all_others:` mediamtx.yml so WHEP works out of the box (#5).
4b. **Confirm native MJPEG on-device** — the parser + player are implemented and parser-verified (§9), but the assembled on-device render still needs a visual check during a real device session (phone on the device AP/LAN, camera attached). Test-harness note: the Android `WifiNetworkSpecifier` AP is a *local-only* network — reach the device by driving the app's own join flow, not an out-of-band `adb`/CDP bind. For a synthetic stream target, ConnMan's tethering only forwards specific gateway ports to AP clients (8000 works, arbitrary ports don't).
5. Fold the manual board-side fixes (bluez.go passphrase, hciadv UUID-in-primary, mediamtx config) into the meta-layer / image.
6. End-to-end streaming/recording test with a real DV camera attached.
7. Finish the app audit items in §9.
