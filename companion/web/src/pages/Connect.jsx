import React, { useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { useServer } from "../context/ServerContext";
import { isNative } from "../lib/native";
import { hapticNotification } from "../lib/haptics";
import { pskError, phoneWifiIp } from "../lib/wifi";
import Card from "../components/ui/Card";
import Button from "../components/ui/Button";
import StatusDot from "../components/ui/StatusDot";
import SignalBars from "../components/ui/SignalBars";

// BLE states:
//   null | 'scanning' | 'found' | 'connecting'
//   | 'wifi-scan' | 'wifi-select' | 'wifi-password' | 'wifi-joining'
//   | 'provisioning' (manual AP-hotspot fallback)
//   | 'connected' | 'error'

const WIFI_SCAN_POLL_MS = 2500;
const WIFI_SCAN_MAX_POLLS = 6; // ~15s total

export default function Connect() {
  const navigate = useNavigate();
  const {
    apiBase,
    setApiBase,
    reachable,
    candidateServers,
    probeServer,
    discoverServers,
    refresh,
    setError,
    error,
  } = useServer();

  // Manual / LAN discovery state
  const [manualServer, setManualServer] = useState(apiBase || "");
  const [isDiscovering, setIsDiscovering] = useState(false);
  const [discovered, setDiscovered] = useState([]);
  const [showSetup, setShowSetup] = useState(false);

  // BLE state machine
  const [bleState, setBleState] = useState(null); // null = idle
  const [bleDevice, setBleDevice] = useState(null);
  const [bleStatus, setBleStatus] = useState(null);
  const [bleError, setBleError] = useState(null);
  const [wifiNetworks, setWifiNetworks] = useState([]);
  const [wifiScanError, setWifiScanError] = useState(null);
  const [selectedSsid, setSelectedSsid] = useState(null);
  const [wifiPsk, setWifiPsk] = useState("");
  const [joinStatus, setJoinStatus] = useState(null); // "connecting" | "connected" | "error"
  const bleAbort = useRef(null);

  // Auto-navigate once connected
  useEffect(() => {
    if (bleState === "connected") {
      const t = setTimeout(() => navigate("/"), 1500);
      return () => clearTimeout(t);
    }
  }, [bleState, navigate]);

  // Haptic feedback on the two terminal BLE outcomes.
  useEffect(() => {
    if (bleState === "connected") hapticNotification("Success");
    else if (bleState === "error") hapticNotification("Error");
  }, [bleState]);

  // --- BLE flow -----------------------------------------------------------

  async function startBleScan() {
    setBleError(null);
    setBleState("scanning");
    setBleDevice(null);
    setBleStatus(null);

    const { bleInit, scanForDevice, connect, readStatus } = await import("../lib/ble.js");

    try {
      await bleInit();
    } catch (err) {
      setBleError("Bluetooth unavailable: " + (err.message || String(err)));
      setBleState("error");
      return;
    }

    let device;
    try {
      device = await scanForDevice((d) => {
        setBleDevice(d);
        setBleState("found");
      });
    } catch (err) {
      setBleError(err.message || "Scan failed");
      setBleState("error");
      return;
    }

    setBleState("connecting");

    try {
      await connect(device.deviceId, () => setBleState("error"));
    } catch (err) {
      setBleError("Could not connect: " + (err.message || String(err)));
      setBleState("error");
      return;
    }

    let statusData;
    try {
      statusData = await readStatus(device.deviceId);
      setBleStatus(statusData);
    } catch (err) {
      setBleError("Could not read device status: " + (err.message || String(err)));
      setBleState("error");
      const { disconnect } = await import("../lib/ble.js");
      await disconnect(device.deviceId);
      return;
    }

    // The device reporting an IP does NOT mean this phone can reach it — a
    // board on a different WiFi than the phone reports an IP that is
    // unroutable from here. Probe it before claiming connected.
    if (statusData.ip && statusData.api && (await probeServer(statusData.api))) {
      setApiBase(statusData.api);
      await refresh(statusData.api);
      const { disconnect } = await import("../lib/ble.js");
      await disconnect(device.deviceId);
      setBleState("connected");
      return;
    }

    // Not reachable on a shared LAN. Offer the connection routes; the device
    // hosting its own AP is the primary path (see doApHandoff). The BLE
    // "provision the device onto your WiFi" route stays available as an
    // alternative. Keep the BLE link open — both routes need it.
    setBleState("choose-route");
  }

  async function startWifiScan(device) {
    setBleState("wifi-scan");
    setWifiNetworks([]);
    setWifiScanError(null);

    const { readWifiScan } = await import("../lib/ble.js");

    for (let attempt = 0; attempt < WIFI_SCAN_MAX_POLLS; attempt++) {
      let result;
      try {
        result = await readWifiScan(device.deviceId);
      } catch (err) {
        setWifiScanError(err.message || "Could not read WiFi list");
        break;
      }
      if (result.err) {
        setWifiScanError(result.err);
        break;
      }
      const nets = result.networks || [];
      if (nets.length > 0 || !result.scanning) {
        setWifiNetworks(nets);
        break;
      }
      await new Promise((r) => setTimeout(r, WIFI_SCAN_POLL_MS));
    }

    setBleState("wifi-select");
  }

  async function rescanWifi() {
    if (!bleDevice) return;
    await startWifiScan(bleDevice);
  }

  function pickNetwork(ssid) {
    setSelectedSsid(ssid);
    setWifiPsk("");
    setBleState("wifi-password");
  }

  async function joinSelectedNetwork(e) {
    e.preventDefault();
    if (!bleDevice || !selectedSsid) return;
    if (pskError(wifiPsk)) return; // guard the Enter-key submit path

    const { writeWifiCreds, subscribeNetResult, readStatus, disconnect } = await import(
      "../lib/ble.js"
    );

    setBleState("wifi-joining");
    setJoinStatus("connecting");
    setBleError(null);

    let settled = false;
    const finishOk = async () => {
      if (settled) return;
      settled = true;
      try {
        const fresh = await readStatus(bleDevice.deviceId);
        if (fresh.ip && fresh.api) {
          setApiBase(fresh.api);
          await refresh(fresh.api);
        }
      } catch {
        // Net result said connected but we couldn't re-read status over BLE —
        // the LAN discovery flow below can still find it.
      }
      await disconnect(bleDevice.deviceId);
      setBleState("connected");
    };
    const finishError = async (msg) => {
      if (settled) return;
      settled = true;
      setBleError(msg || "Could not join that network");
      setBleState("error");
      await disconnect(bleDevice.deviceId).catch(() => {});
    };

    try {
      await subscribeNetResult(bleDevice.deviceId, (result) => {
        if (result.state === "connected") {
          finishOk();
        } else if (result.state === "error") {
          finishError(result.err);
        } else {
          setJoinStatus(result.state || "connecting");
        }
      });
    } catch {
      // Notifications unavailable — fall back to the timeout-based status poll below.
    }

    try {
      await writeWifiCreds(bleDevice.deviceId, selectedSsid, wifiPsk);
    } catch (err) {
      await finishError("Could not send WiFi credentials: " + (err.message || String(err)));
      return;
    }

    // Safety net: if no notification arrives (BLE/WiFi coexistence on the
    // single AIC8800 radio can be flaky right as the device joins a new
    // network), poll status directly after a grace period.
    setTimeout(async () => {
      if (settled) return;
      try {
        const fresh = await readStatus(bleDevice.deviceId);
        if (fresh.ip && fresh.api) {
          finishOk();
        } else {
          finishError("Timed out waiting for the device to join " + selectedSsid);
        }
      } catch {
        finishError("Timed out waiting for the device to join " + selectedSsid);
      }
    }, 20000);
  }

  // Primary handoff: tell the device to host its own AP, then join it from the
  // phone. On a single-radio device the AP and station modes are mutually
  // exclusive, so this is also how a phone with no shared network reaches the
  // device at all. Uses WifiNetworkSpecifier under the hood (one system-dialog
  // tap) with app-traffic binding so AP_GATEWAY is reachable.
  async function doApHandoff() {
    if (!bleDevice) return;
    setBleError(null);

    const { writeApControl, disconnect, readWifiScan } = await import("../lib/ble.js");
    const { joinDeviceAp, AP_GATEWAY, AP_SSID } = await import("../lib/wifi.js");

    setBleState("starting-ap");

    // Pre-fetch the WiFi list over BLE *before* the AP starts. The single-radio
    // chip can't scan while hosting the AP, so this is our only chance to get a
    // network list for the optional "connect device to WiFi" step later. Best
    // effort — if it fails, that step falls back to manual SSID entry.
    try {
      for (let i = 0; i < 3; i++) {
        const res = await readWifiScan(bleDevice.deviceId);
        if ((res.networks && res.networks.length) || !res.scanning) {
          setWifiNetworks(res.networks || []);
          break;
        }
        await new Promise((r) => setTimeout(r, 1800));
      }
    } catch {
      // No pre-scan list — manual entry remains available.
    }

    // 1. Ask the device to start its hotspot (over the still-open BLE link).
    try {
      await writeApControl(bleDevice.deviceId, true);
    } catch {
      // Non-fatal — the AP may already be up. We'll find out when we try to join.
    }
    // The device needs a moment to bring the AP + DHCP up before we join.
    await new Promise((r) => setTimeout(r, 2500));
    await disconnect(bleDevice.deviceId);

    // 2. Join the hotspot from the phone (system dialog).
    setBleState("joining-ap");
    if (!isNative()) {
      // Web/dev build: no WiFi control. Fall back to the manual instructions.
      setBleState("provisioning");
      return;
    }
    try {
      await joinDeviceAp();
    } catch {
      // Auto-join failed (user dismissed the system WiFi dialog, or the plugin
      // errored). Don't dead-end telling them the password is "in the app" —
      // send them to the manual screen, which shows the SSID + password and an
      // "I'm connected" button to continue.
      setBleState("provisioning");
      return;
    }

    // 3. Reach the device at the AP gateway. addNetwork() returns as soon as the
    //    system add-network dialog is accepted; the actual association takes a
    //    few seconds, so poll rather than giving up on the first miss.
    setApiBase(AP_GATEWAY);
    setManualServer(AP_GATEWAY);
    let ok = false;
    for (let attempt = 0; attempt < 12 && !ok; attempt += 1) {
      ok = await refresh(AP_GATEWAY);
      if (!ok) await new Promise((r) => setTimeout(r, 1500));
    }
    if (ok) {
      setBleState("ap-connected");
    } else {
      setBleError(
        `Joined ${AP_SSID} but couldn't reach the device at ${AP_GATEWAY}. ` +
          `Try again, or reconnect over Bluetooth.`
      );
      setBleState("error");
    }
  }

  // After the phone joins the AP, the user may leave the device on the AP
  // (done) or provision it onto a WiFi network for a normal LAN connection.
  async function useApAsIs() {
    setBleState("connected");
  }

  // Manual fallback (web build, or the plugin join failed): the user joined
  // the equip-1 AP themselves in WiFi settings and tapped "I'm connected".
  async function onApConnectedManual() {
    const { AP_GATEWAY } = await import("../lib/wifi.js");
    setApiBase(AP_GATEWAY);
    setManualServer(AP_GATEWAY);
    const ok = await refresh(AP_GATEWAY);
    if (ok) {
      setBleState("ap-connected");
    } else {
      setBleError(
        `Could not reach the device at ${AP_GATEWAY} — make sure your phone is on the equip-1 WiFi.`
      );
      setBleState("error");
    }
  }

  // --- Provision the device onto a WiFi network, over the AP (HTTP) ---------
  // Note: on this single-radio device, applying WiFi creds drops the AP (the
  // radio switches AP→station), so the phone loses the 192.168.0.1 link and the
  // HTTP request won't return. That's expected; we guide the user to rejoin the
  // chosen network and re-find the device on the LAN.

  function pickNetworkAp(ssid) {
    setSelectedSsid(ssid);
    setWifiPsk("");
    setBleState("ap-wifi-password");
  }

  async function provisionOverAp(e) {
    e?.preventDefault();
    if (!selectedSsid) return;
    if (pskError(wifiPsk)) return; // guard the Enter-key submit path
    const ssid = selectedSsid;
    const psk = wifiPsk;
    setBleError(null);
    setBleState("ap-provisioning");
    // Fire-and-forget: the AP drops mid-request as the device switches to
    // station mode, so the response usually never arrives. Swallow the error.
    const { setWifi } = await import("../api.js");
    setWifi(apiBase, { ssid, psk }).catch(() => {});
  }

  // After the user rejoins the provisioned network, find the device on the LAN.
  async function findAfterProvision() {
    setBleError(null);
    setIsDiscovering(true);
    try {
      const candidates = candidateServers();
      for (const base of candidates) {
        if (await probeServer(base)) {
          setApiBase(base);
          setManualServer(base);
          await refresh(base);
          setBleState("connected");
          return;
        }
      }
      const phoneIp = await phoneWifiIp();
      const found = await discoverServers({
        seeds: [phoneIp, window.location.hostname, ...candidates].filter(Boolean),
      });
      if (found.length > 0) {
        setApiBase(found[0].base);
        setManualServer(found[0].base);
        await refresh(found[0].base);
        setBleState("connected");
        return;
      }
      setBleError(
        `Couldn't find the device on "${selectedSsid}" yet. Make sure your phone is on that network, then try again.`
      );
    } finally {
      setIsDiscovering(false);
    }
  }

  function cancelBle() {
    // If we joined the device AP, release the binding so the phone falls back
    // to its normal network. Fire-and-forget — never blocks the UI reset.
    if (bleState === "ap-connected" || bleState === "joining-ap") {
      import("../lib/wifi.js").then((m) => m.leaveDeviceAp()).catch(() => {});
    }
    setBleState(null);
    setBleDevice(null);
    setBleStatus(null);
    setBleError(null);
    setWifiNetworks([]);
    setWifiScanError(null);
    setSelectedSsid(null);
    setWifiPsk("");
    setJoinStatus(null);
  }

  // --- LAN discovery ------------------------------------------------------

  async function autoSelect() {
    setIsDiscovering(true);
    setDiscovered([]);
    setError("");
    try {
      const candidates = candidateServers();
      for (const base of candidates) {
        if (await probeServer(base)) {
          setApiBase(base);
          setManualServer(base);
          await refresh(base);
          return;
        }
      }

      const phoneIp = await phoneWifiIp();
      const found = await discoverServers({
        seeds: [phoneIp, window.location.hostname, manualServer, ...candidates].filter(Boolean),
      });
      setDiscovered(found);

      if (found.length > 0) {
        setApiBase(found[0].base);
        setManualServer(found[0].base);
        await refresh(found[0].base);
        return;
      }

      setError(
        `No API server found. Checked common addresses and scanned LAN prefixes seeded from: ${
          [window.location.hostname, manualServer].filter(Boolean).join(", ") || "none"
        }`
      );
    } finally {
      setIsDiscovering(false);
    }
  }

  async function applyManual() {
    let base = manualServer.trim().replace(/\/+$/, "");
    if (!base) {
      setError("Enter a server URL first");
      return;
    }
    // Accept a bare host/IP (e.g. "192.168.1.5:8000") — the device API is plain
    // HTTP, so assume http:// rather than failing on a scheme-less relative URL.
    if (!/^https?:\/\//i.test(base)) {
      base = `http://${base}`;
      setManualServer(base);
    }
    if (!(await probeServer(base))) {
      setError(`Cannot reach ${base}/health`);
      return;
    }
    setApiBase(base);
    await refresh(base);
    setError("");
  }

  function selectDiscovered(base) {
    setApiBase(base);
    setManualServer(base);
    refresh(base);
  }

  // --- Render helpers -----------------------------------------------------

  // Currently connected & reachable — show status screen
  if (reachable && !showSetup && bleState === null) {
    return (
      <div className="stack">
        <div className="page-head">
          <span className="label">pairing</span>
          <h1 className="display">Connect</h1>
        </div>

        <Card>
          <div
            style={{
              display: "flex",
              flexDirection: "column",
              alignItems: "center",
              gap: "var(--sp-4)",
              padding: "var(--sp-5) 0",
            }}
          >
            <div
              style={{
                width: 56,
                height: 56,
                borderRadius: "50%",
                background: "var(--ok)",
                display: "flex",
                alignItems: "center",
                justifyContent: "center",
                fontSize: "1.6rem",
              }}
            >
              ✓
            </div>
            <div style={{ textAlign: "center" }}>
              <div className="label" style={{ marginBottom: "var(--sp-2)" }}>
                connected
              </div>
              <div className="data" style={{ color: "var(--text-dim)", fontSize: "0.8rem" }}>
                {apiBase}
              </div>
            </div>
            <Button variant="primary" block onClick={() => navigate("/")}>
              View Dashboard
            </Button>
            <button
              type="button"
              className="btn btn--ghost btn--sm"
              onClick={() => setShowSetup(true)}
            >
              Change device
            </button>
          </div>
        </Card>
      </div>
    );
  }

  // BLE scanning
  if (bleState === "scanning") {
    return (
      <div className="stack">
        <div className="page-head">
          <span className="label">pairing</span>
          <h1 className="display">Connect</h1>
        </div>
        <Card>
          <div
            style={{
              display: "flex",
              flexDirection: "column",
              alignItems: "center",
              gap: "var(--sp-4)",
              padding: "var(--sp-5) 0",
            }}
          >
            <div
              style={{
                width: 48,
                height: 48,
                border: "3px solid var(--accent)",
                borderTopColor: "transparent",
                borderRadius: "50%",
                animation: "ble-spin 0.9s linear infinite",
              }}
            />
            <style>{`@keyframes ble-spin { to { transform: rotate(360deg); } }`}</style>
            <p className="dim" style={{ margin: 0, fontSize: "0.85rem" }}>
              Scanning for equip-1...
            </p>
            <Button variant="ghost" size="sm" onClick={cancelBle}>
              Cancel
            </Button>
          </div>
        </Card>
      </div>
    );
  }

  // BLE device found — connecting
  if (bleState === "found" || bleState === "connecting") {
    return (
      <div className="stack">
        <div className="page-head">
          <span className="label">pairing</span>
          <h1 className="display">Connect</h1>
        </div>
        <Card title="Device found">
          <div style={{ display: "flex", flexDirection: "column", gap: "var(--sp-3)" }}>
            <div className="kv">
              <span className="kv__k">name</span>
              <span className="kv__v data">
                {bleDevice?.name || bleDevice?.deviceId || "equip-1"}
              </span>
            </div>
            {bleDevice?.rssi != null && (
              <div className="kv">
                <span className="kv__k">signal</span>
                <span className="kv__v data">{bleDevice.rssi} dBm</span>
              </div>
            )}
            <div className="kv">
              <span className="kv__k">status</span>
              <span className="kv__v">
                <StatusDot state="warn" /> Connecting...
              </span>
            </div>
          </div>
        </Card>
      </div>
    );
  }

  // Device found over BLE but not reachable on a shared LAN — choose a route.
  // Device-hosted AP is the primary path; provisioning onto an existing WiFi
  // over BLE is the alternative.
  if (bleState === "choose-route") {
    return (
      <div className="stack">
        <div className="page-head">
          <span className="label">pairing</span>
          <h1 className="display">Connect</h1>
        </div>
        <Card title="How do you want to connect?">
          <p className="dim" style={{ fontSize: "0.82rem", marginTop: 0 }}>
            The device isn&apos;t on your network. Join its own hotspot for a direct
            link, or put it on a WiFi network you both share.
          </p>
          <div style={{ display: "flex", flexDirection: "column", gap: "var(--sp-3)" }}>
            <Button variant="primary" block onClick={doApHandoff}>
              Join device hotspot
            </Button>
            <Button variant="ghost" size="sm" onClick={() => startWifiScan(bleDevice)}>
              Connect device to a WiFi network
            </Button>
            <Button variant="ghost" size="sm" onClick={cancelBle}>
              Cancel
            </Button>
          </div>
        </Card>
      </div>
    );
  }

  // Telling the device to start its AP (over BLE).
  if (bleState === "starting-ap" || bleState === "joining-ap") {
    const joining = bleState === "joining-ap";
    return (
      <div className="stack">
        <div className="page-head">
          <span className="label">pairing</span>
          <h1 className="display">Connect</h1>
        </div>
        <Card title={joining ? "Joining device hotspot" : "Starting device hotspot"}>
          <div
            style={{
              display: "flex",
              flexDirection: "column",
              alignItems: "center",
              gap: "var(--sp-4)",
              padding: "var(--sp-4) 0",
            }}
          >
            <div
              style={{
                width: 40,
                height: 40,
                border: "3px solid var(--accent)",
                borderTopColor: "transparent",
                borderRadius: "50%",
                animation: "ble-spin 0.9s linear infinite",
              }}
            />
            <style>{`@keyframes ble-spin { to { transform: rotate(360deg); } }`}</style>
            <p className="dim" style={{ margin: 0, fontSize: "0.85rem", textAlign: "center" }}>
              {joining
                ? "Approve the WiFi prompt to join equip-1…"
                : "Asking the device to start its hotspot…"}
            </p>
          </div>
        </Card>
      </div>
    );
  }

  // Joined the AP and reached the device — offer to use it as-is or provision
  // it onto a WiFi network.
  if (bleState === "ap-connected") {
    return (
      <div className="stack">
        <div className="page-head">
          <span className="label">pairing</span>
          <h1 className="display">Connect</h1>
        </div>
        <Card title="Connected over hotspot">
          <div style={{ display: "flex", flexDirection: "column", gap: "var(--sp-4)" }}>
            <div style={{ display: "flex", alignItems: "center", gap: "var(--sp-3)" }}>
              <StatusDot state="ok" />
              <span className="data" style={{ fontSize: "0.85rem" }}>
                {apiBase}
              </span>
            </div>
            <p className="dim" style={{ margin: 0, fontSize: "0.82rem" }}>
              You&apos;re connected directly to the device. Use it now, or connect it
              to a WiFi network so you don&apos;t need the hotspot next time.
            </p>
            <Button variant="primary" block onClick={useApAsIs}>
              View Dashboard
            </Button>
            <Button
              variant="ghost"
              size="sm"
              onClick={() => setBleState("ap-wifi-select")}
            >
              Connect device to a WiFi network
            </Button>
            <Button variant="ghost" size="sm" onClick={cancelBle}>
              Disconnect
            </Button>
          </div>
        </Card>
      </div>
    );
  }

  // Over the AP: pick a WiFi network for the device to join (list was
  // pre-fetched over BLE before the AP started; single-radio can't rescan now).
  if (bleState === "ap-wifi-select") {
    return (
      <div className="stack">
        <div className="page-head">
          <span className="label">pairing</span>
          <h1 className="display">Connect</h1>
        </div>
        <Card title="Choose a WiFi network for the device">
          <p className="dim" style={{ fontSize: "0.8rem", marginTop: 0 }}>
            The device will join this network and leave its hotspot. 2.4GHz
            networks work best.
          </p>
          {wifiNetworks.length === 0 ? (
            <p className="dim" style={{ fontSize: "0.82rem" }}>
              No network list available — enter a network name below.
            </p>
          ) : (
            <ul className="files-list">
              {wifiNetworks.map((n) => (
                <li key={n.ssid}>
                  <button
                    type="button"
                    className="btn discovery-item"
                    onClick={() => pickNetworkAp(n.ssid)}
                  >
                    <span className="data">{n.ssid}</span>
                    {n.strength != null && <SignalBars strength={n.strength} />}
                  </button>
                </li>
              ))}
            </ul>
          )}
          <form
            className="field-group"
            style={{ marginTop: "var(--sp-4)" }}
            onSubmit={(e) => {
              e.preventDefault();
              if (selectedSsid) setBleState("ap-wifi-password");
            }}
          >
            <input
              className="input"
              type="text"
              placeholder="Or type a network name (SSID)"
              aria-label="WiFi network name"
              value={selectedSsid || ""}
              onChange={(e) => setSelectedSsid(e.target.value)}
            />
            <Button type="submit" variant="ghost" size="sm" disabled={!selectedSsid}>
              Use this network
            </Button>
          </form>
          <Button
            variant="ghost"
            size="sm"
            onClick={() => setBleState("ap-connected")}
          >
            Back
          </Button>
        </Card>
      </div>
    );
  }

  // Over the AP: password for the chosen network.
  if (bleState === "ap-wifi-password") {
    return (
      <div className="stack">
        <div className="page-head">
          <span className="label">pairing</span>
          <h1 className="display">Connect</h1>
        </div>
        <Card title={selectedSsid}>
          <form className="field-group" onSubmit={provisionOverAp}>
            <input
              className="input"
              type="password"
              placeholder="Password (leave blank if open)"
              aria-label="WiFi password"
              value={wifiPsk}
              onChange={(e) => setWifiPsk(e.target.value)}
              autoFocus
            />
            {pskError(wifiPsk) && (
              <p className="dim" style={{ fontSize: "0.75rem", margin: 0, color: "var(--warn)" }}>
                {pskError(wifiPsk)}
              </p>
            )}
            <Button type="submit" variant="primary" block disabled={!!pskError(wifiPsk)}>
              Connect device
            </Button>
            <Button
              type="button"
              variant="ghost"
              size="sm"
              onClick={() => setBleState("ap-wifi-select")}
            >
              Back
            </Button>
          </form>
        </Card>
      </div>
    );
  }

  // Over the AP: creds sent; the device drops the AP to join. Guide the user to
  // rejoin the chosen network, then re-find the device on the LAN.
  if (bleState === "ap-provisioning") {
    return (
      <div className="stack">
        <div className="page-head">
          <span className="label">pairing</span>
          <h1 className="display">Connect</h1>
        </div>
        <Card title="Device is switching networks">
          <div style={{ display: "flex", flexDirection: "column", gap: "var(--sp-4)" }}>
            <p className="dim" style={{ margin: 0, fontSize: "0.85rem" }}>
              The device is leaving its hotspot to join{" "}
              <span className="data">{selectedSsid}</span>. Its hotspot will
              disappear.
            </p>
            <ol className="dim" style={{ margin: 0, fontSize: "0.82rem", paddingLeft: "1.1rem" }}>
              <li>Connect your phone to <span className="data">{selectedSsid}</span>.</li>
              <li>Then tap Find device below.</li>
            </ol>
            {bleError && <div className="notice">{bleError}</div>}
            <Button variant="primary" block onClick={findAfterProvision} disabled={isDiscovering}>
              {isDiscovering ? "Searching…" : "Find device"}
            </Button>
            <Button variant="ghost" size="sm" onClick={cancelBle}>
              Cancel
            </Button>
          </div>
        </Card>
      </div>
    );
  }

  // Scanning for nearby WiFi networks over BLE
  if (bleState === "wifi-scan") {
    return (
      <div className="stack">
        <div className="page-head">
          <span className="label">pairing</span>
          <h1 className="display">Connect</h1>
        </div>
        <Card title="Looking for WiFi networks">
          <div
            style={{
              display: "flex",
              flexDirection: "column",
              alignItems: "center",
              gap: "var(--sp-4)",
              padding: "var(--sp-4) 0",
            }}
          >
            <div
              style={{
                width: 40,
                height: 40,
                border: "3px solid var(--accent)",
                borderTopColor: "transparent",
                borderRadius: "50%",
                animation: "ble-spin 0.9s linear infinite",
              }}
            />
            <style>{`@keyframes ble-spin { to { transform: rotate(360deg); } }`}</style>
            <p className="dim" style={{ margin: 0, fontSize: "0.85rem" }}>
              Asking the device to scan for nearby networks…
            </p>
          </div>
        </Card>
      </div>
    );
  }

  // WiFi networks found over BLE — pick one
  if (bleState === "wifi-select") {
    return (
      <div className="stack">
        <div className="page-head">
          <span className="label">pairing</span>
          <h1 className="display">Connect</h1>
        </div>
        <Card title="Choose a WiFi network">
          <p className="dim" style={{ fontSize: "0.8rem", marginTop: 0 }}>
            The device isn&apos;t on a network yet. Pick a WiFi network for it to join.
          </p>
          {wifiScanError && <div className="notice">{wifiScanError}</div>}
          {wifiNetworks.length === 0 && !wifiScanError ? (
            <p className="dim" style={{ fontSize: "0.82rem" }}>
              No networks found nearby.
            </p>
          ) : (
            <ul className="files-list">
              {wifiNetworks.map((n) => (
                <li key={n.ssid}>
                  <button
                    type="button"
                    className="btn discovery-item"
                    onClick={() => pickNetwork(n.ssid)}
                  >
                    <span className="data">{n.ssid}</span>
                    {n.strength != null && <SignalBars strength={n.strength} />}
                  </button>
                </li>
              ))}
            </ul>
          )}
          <div className="row-wrap" style={{ marginTop: "var(--sp-4)" }}>
            <Button variant="ghost" size="sm" onClick={rescanWifi}>
              Rescan
            </Button>
            <Button variant="ghost" size="sm" onClick={doApHandoff}>
              Use device hotspot instead
            </Button>
            <Button variant="ghost" size="sm" onClick={cancelBle}>
              Cancel
            </Button>
          </div>
        </Card>
      </div>
    );
  }

  // Password entry for the selected network
  if (bleState === "wifi-password") {
    return (
      <div className="stack">
        <div className="page-head">
          <span className="label">pairing</span>
          <h1 className="display">Connect</h1>
        </div>
        <Card title={selectedSsid}>
          <form className="field-group" onSubmit={joinSelectedNetwork}>
            <input
              className="input"
              type="password"
              placeholder="Password (leave blank if open)"
              aria-label="WiFi password"
              value={wifiPsk}
              onChange={(e) => setWifiPsk(e.target.value)}
              autoFocus
            />
            {pskError(wifiPsk) && (
              <p className="dim" style={{ fontSize: "0.75rem", margin: 0, color: "var(--warn)" }}>
                {pskError(wifiPsk)}
              </p>
            )}
            <Button type="submit" variant="primary" block disabled={!!pskError(wifiPsk)}>
              Join
            </Button>
            <Button
              type="button"
              variant="ghost"
              size="sm"
              onClick={() => setBleState("wifi-select")}
            >
              Back
            </Button>
          </form>
        </Card>
      </div>
    );
  }

  // Credentials sent — waiting for the device to join
  if (bleState === "wifi-joining") {
    return (
      <div className="stack">
        <div className="page-head">
          <span className="label">pairing</span>
          <h1 className="display">Connect</h1>
        </div>
        <Card title={`Joining ${selectedSsid}`}>
          <div
            style={{
              display: "flex",
              flexDirection: "column",
              alignItems: "center",
              gap: "var(--sp-4)",
              padding: "var(--sp-4) 0",
            }}
          >
            <div
              style={{
                width: 40,
                height: 40,
                border: "3px solid var(--accent)",
                borderTopColor: "transparent",
                borderRadius: "50%",
                animation: "ble-spin 0.9s linear infinite",
              }}
            />
            <style>{`@keyframes ble-spin { to { transform: rotate(360deg); } }`}</style>
            <p className="dim" style={{ margin: 0, fontSize: "0.85rem" }}>
              {joinStatus === "connecting" ? "Connecting…" : joinStatus || "Working…"}
            </p>
          </div>
        </Card>
      </div>
    );
  }

  // AP provisioning — user must connect phone to equip-1 AP (manual fallback)
  if (bleState === "provisioning") {
    return (
      <div className="stack">
        <div className="page-head">
          <span className="label">pairing</span>
          <h1 className="display">Connect</h1>
        </div>
        <Card title="Connect to device hotspot">
          <div style={{ display: "flex", flexDirection: "column", gap: "var(--sp-4)" }}>
            <p className="dim" style={{ margin: 0, fontSize: "0.85rem" }}>
              The device has started its own hotspot instead.
              Open your phone&apos;s WiFi settings and connect to:
            </p>
            <div
              style={{
                background: "var(--surface-2)",
                border: "1px solid var(--line)",
                borderRadius: "var(--radius-sm)",
                padding: "var(--sp-4)",
                display: "flex",
                flexDirection: "column",
                gap: "var(--sp-2)",
              }}
            >
              <div className="kv">
                <span className="kv__k">network</span>
                <span className="kv__v data">equip-1</span>
              </div>
              <div className="kv">
                <span className="kv__k">password</span>
                <span className="kv__v data">equip1device</span>
              </div>
            </div>
            <p className="dim" style={{ margin: 0, fontSize: "0.8rem" }}>
              Once connected, tap the button below.
            </p>
            <Button variant="primary" onClick={onApConnectedManual}>
              I&apos;m connected
            </Button>
            <Button variant="ghost" size="sm" onClick={cancelBle}>
              Cancel
            </Button>
          </div>
        </Card>
      </div>
    );
  }

  // BLE connected — brief success state before auto-navigate
  if (bleState === "connected") {
    return (
      <div className="stack">
        <div className="page-head">
          <span className="label">pairing</span>
          <h1 className="display">Connect</h1>
        </div>
        <Card>
          <div
            style={{
              display: "flex",
              flexDirection: "column",
              alignItems: "center",
              gap: "var(--sp-4)",
              padding: "var(--sp-5) 0",
            }}
          >
            <div
              style={{
                width: 56,
                height: 56,
                borderRadius: "50%",
                background: "var(--ok)",
                display: "flex",
                alignItems: "center",
                justifyContent: "center",
                fontSize: "1.6rem",
                color: "#000",
              }}
            >
              ✓
            </div>
            <p style={{ margin: 0, color: "var(--ok)", fontFamily: "var(--font-mono)" }}>
              Connected
            </p>
          </div>
        </Card>
      </div>
    );
  }

  // BLE error
  if (bleState === "error") {
    return (
      <div className="stack">
        <div className="page-head">
          <span className="label">pairing</span>
          <h1 className="display">Connect</h1>
        </div>
        <Card title="Bluetooth error">
          <p className="dim" style={{ fontSize: "0.85rem", marginTop: 0 }}>
            {bleError || "An unknown error occurred."}
          </p>
          <div style={{ display: "flex", gap: "var(--sp-3)" }}>
            <Button variant="primary" onClick={startBleScan}>
              Retry
            </Button>
            <Button variant="ghost" onClick={cancelBle}>
              Cancel
            </Button>
          </div>
        </Card>
      </div>
    );
  }

  // --- Setup screen (default / showSetup) ---------------------------------
  return (
    <div className="stack">
      <div className="page-head">
        <span className="label">pairing</span>
        <h1 className="display">Connect</h1>
      </div>

      {error ? <div className="notice">{error}</div> : null}

      {/* Current status */}
      <Card title="Current">
        <div className="status-line">
          <StatusDot state={reachable ? "ok" : reachable === false ? "warn" : "idle"} />
          <span className="data">{apiBase || "—"}</span>
        </div>
      </Card>

      {/* BLE pairing — native only */}
      {isNative() && (
        <Card title="Pair over Bluetooth">
          <p className="dim" style={{ fontSize: "0.8rem", marginTop: 0 }}>
            Find your equip-1 device automatically using Bluetooth.
          </p>
          <Button variant="primary" block onClick={startBleScan}>
            Find via BLE
          </Button>
        </Card>
      )}

      {/* Manual IP / LAN discovery */}
      <Card title="Local network">
        <p className="dim" style={{ fontSize: "0.8rem", marginTop: 0 }}>
          Auto-discover the device on your LAN, or enter its address directly.
        </p>
        <div className="row-wrap" style={{ marginBottom: "var(--sp-3)" }}>
          <Button variant="primary" onClick={autoSelect} disabled={isDiscovering}>
            {isDiscovering ? "Scanning…" : "Auto Discover"}
          </Button>
        </div>
        <div className="row-wrap">
          <input
            className="input grow"
            type="text"
            value={manualServer}
            onChange={(e) => setManualServer(e.target.value)}
            placeholder="http://192.168.x.x:8000"
            aria-label="Device address"
          />
          <Button onClick={applyManual}>Use</Button>
        </div>

        {discovered.length > 0 && (
          <div style={{ marginTop: "var(--sp-4)" }}>
            <span className="label">discovered</span>
            <ul className="files-list">
              {discovered.map((server) => (
                <li key={server.base}>
                  <button
                    type="button"
                    className="btn discovery-item"
                    onClick={() => selectDiscovered(server.base)}
                  >
                    <span className="data">{server.base}</span>
                    <span className="dim">{server.hostname || "unknown-host"}</span>
                  </button>
                </li>
              ))}
            </ul>
          </div>
        )}
      </Card>

      {!isNative() && (
        <Card title="Pair over Bluetooth">
          <div className="muted-box">
            <strong style={{ color: "var(--text)" }}>Native only.</strong> Bluetooth
            LE pairing is available when this app runs as a native build. On the web
            it stays disabled — use local-network discovery above.
          </div>
        </Card>
      )}

      {reachable && showSetup && (
        <div style={{ textAlign: "center", paddingBottom: "var(--sp-4)" }}>
          <button
            type="button"
            className="btn btn--ghost btn--sm"
            onClick={() => setShowSetup(false)}
          >
            Back to status
          </button>
        </div>
      )}
    </div>
  );
}
