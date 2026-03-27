# Project Rules

1. **Keep hardware control deterministic.** Any change that touches GPIO, subprocess launching, or system commands must log intent, bail gracefully on failure, and avoid leaving DVGrab running after client disconnects.
2. **Respect the pairing flow.** Bluetooth scanning must only be used to bootstrap the access point; once credentials are exchanged, the SBC should automatically drop the BLE server and rely on the AP for high-throughput operations.
3. **Favor shared contracts.** Define TypeScript types or JSON schemas for the control API, recorder state, and media metadata so the React web app and future React Native app can share the same data model without duplicating translation logic.
4. **Secure storage access.** The filesystem endpoints must only expose `captures/` and related metadata. Never expose the underlying root filesystem or reveal secrets in responses.
5. **Design for offline use.** The web UI should cache the last known recorder state and queued commands when the client disconnects, then reconcile with the SBC when it reconnects to avoid accidental double-taps.
6. **Document every phase.** Before shipping a new feature (BLE pairing, AP handover, React Native build), add a short entry to `/docs/notes.md` or the README so collaborators know the rollout steps.
