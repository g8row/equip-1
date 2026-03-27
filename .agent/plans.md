# Project Plans

## Vision
The SBC runs a lightweight Python orchestration service that mirrors the physical button interface and exposes recorder state, storage, networking, and power controls. The next step is to wrap that service in a companion experience so the operator can drive the device over Bluetooth/Wi‑Fi and review media without reaching for the hardware buttons.

## Phase 1 – API and Local Web Control
1. Harden the existing `os.py` script into a control daemon (long‑running service, command API, status hooks, camera feed exporter, filesystem inventory). Ensure `captures/` paths and DVGrab subprocess management are accessible via REST or WebSocket endpoints.
2. Introduce a lightweight HTTP server running on the SBC (Flask/FastAPI) for the React site to consume; expose recorder status, storage stats, recorded file list, and control commands (start/stop recording, toggle USB gadget, etc.).
3. Prototype a React web app served either directly from the SBC or through a separate dev server; mirror the OLED screens and buttons plus camera thumbnail previews, storage list, and network/power info.
4. Add WebSocket or SSE hooks for live updates (recorder timer, disk space, recording indicator) so the React UI mirrors the OLED display.

## Phase 2 – Bluetooth pairing + Access Point flow
1. Use Bluetooth pairing to exchange credentials or token between phone/device and SBC. The companion app should find the SBC via BLE advertising, exchange pairing info, and trigger the SBC to start an access point (AP) for higher bandwidth communication (camera stream, filesystem access).
2. Document the BLE service/characteristics used for pairing and the handoff to the AP; this is the choreography React/React Native clients must follow.
3. Flow: (a) scan via BLE, (b) pair and send Wi‑Fi credentials or approval, (c) SBC brings up AP, (d) client switches to SBC AP, (e) load web UI/media services.

## Phase 3 – React Native Conversion
1. Reuse React component tree and data layer (hooks, context, shared services) inside a React Native shell when we need native Bluetooth APIs, camera streaming, and local notifications.
2. Introduce a shared data-contract (TypeScript types, WebSocket payload shapes) so both web and mobile stay in sync.
3. Keep the React web build for desktop browsers and the React Native app for handheld operation; maintain shared design tokens and controls so the UX matches the hardware experience.

## Monitoring & Outreach
- Log significant events (record start/stop, pairing, disk warnings) to a plain file under `/var/log/equip-1` and expose recent logs via the API for diagnostics.
- Keep documentation updated under `/docs` or README so contributors understand how to build and deploy each phase.
