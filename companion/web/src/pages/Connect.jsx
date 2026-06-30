import React, { useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { useServer } from "../context/ServerContext";
import { isNative } from "../lib/native";
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

    // If device already has an IP, connect directly
    if (statusData.ip && statusData.api) {
      setApiBase(statusData.api);
      await refresh(statusData.api);
      const { disconnect } = await import("../lib/ble.js");
      await disconnect(device.deviceId);
      setBleState("connected");
      return;
    }

    // No IP yet — start the BLE WiFi scan-and-select flow. Stay connected;
    // the device needs the BLE link open to receive wifi_creds writes and
    // send back network_result notifications.
    await startWifiScan(device);
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

  async function useHotspotInstead() {
    if (!bleDevice) return;
    const { writeApControl, disconnect } = await import("../lib/ble.js");
    try {
      await writeApControl(bleDevice.deviceId, true);
    } catch {
      // Non-fatal — AP may already be on
    }
    await disconnect(bleDevice.deviceId);
    setBleState("provisioning");
  }

  async function onApConnected() {
    // User says they connected to equip-1 AP — set the AP gateway address
    const apBase = "http://192.168.1.1:8000";
    setApiBase(apBase);
    setManualServer(apBase);
    const ok = await refresh(apBase);
    if (ok) {
      setBleState("connected");
    } else {
      setBleError(
        "Could not reach " + apBase + " — make sure your phone is connected to the equip-1 WiFi network."
      );
      setBleState("error");
    }
  }

  function cancelBle() {
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

      const found = await discoverServers({
        seeds: [window.location.hostname, manualServer, ...candidates],
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
    const base = manualServer.trim().replace(/\/+$/, "");
    if (!base) {
      setError("Enter a server URL first");
      return;
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
            <Button variant="ghost" size="sm" onClick={useHotspotInstead}>
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
              value={wifiPsk}
              onChange={(e) => setWifiPsk(e.target.value)}
              autoFocus
            />
            <Button type="submit" variant="primary" block>
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
            <Button variant="primary" onClick={onApConnected}>
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
