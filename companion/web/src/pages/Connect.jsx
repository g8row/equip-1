import React, { useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { useServer } from "../context/ServerContext";
import { isNative } from "../lib/native";
import Card from "../components/ui/Card";
import Button from "../components/ui/Button";
import StatusDot from "../components/ui/StatusDot";

// BLE states: null | 'scanning' | 'found' | 'connecting' | 'provisioning' | 'connected' | 'error'

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

    const { bleInit, scanForDevice, connect, readStatus, writeApControl, disconnect } =
      await import("../lib/ble.js");

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
      await disconnect(device.deviceId);
      return;
    }

    // If device already has an IP, connect directly
    if (statusData.ip && statusData.api) {
      setApiBase(statusData.api);
      await refresh(statusData.api);
      await disconnect(device.deviceId);
      setBleState("connected");
      return;
    }

    // No IP — enable AP on device and guide user to connect their phone
    try {
      await writeApControl(device.deviceId, true);
    } catch {
      // Non-fatal — AP may already be on
    }
    await disconnect(device.deviceId);
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

  // AP provisioning — user must connect phone to equip-1 AP
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
              The device is not on a WiFi network yet. It has started its hotspot.
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
