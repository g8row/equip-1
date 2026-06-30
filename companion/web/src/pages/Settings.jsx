import React, { useCallback, useEffect, useState } from "react";
import { useServer } from "../context/ServerContext";
import {
  getNetwork,
  getPower,
  getRuntimeDebug,
  scanWifi,
  setAp,
  setRecordingCaptureMode,
  setWifi,
} from "../api";
import Card from "../components/ui/Card";
import Button from "../components/ui/Button";
import Toggle from "../components/ui/Toggle";
import SignalBars from "../components/ui/SignalBars";

function Unavailable({ children = "Unavailable on this device" }) {
  return <p className="muted-box">{children}</p>;
}

export default function Settings() {
  const {
    apiBase,
    status,
    captureModeConfig,
    refreshCaptureMode,
    streamMode,
    setStreamMode,
    setError,
  } = useServer();

  const [network, setNetwork] = useState(null);
  const [networkAvailable, setNetworkAvailable] = useState(null); // null=loading
  const [scanResults, setScanResults] = useState(null);
  const [scanning, setScanning] = useState(false);
  const [ssid, setSsid] = useState("");
  const [psk, setPsk] = useState("");
  const [wifiBusy, setWifiBusy] = useState(false);
  const [apBusy, setApBusy] = useState(false);
  const [power, setPower] = useState(null);
  const [powerAvailable, setPowerAvailable] = useState(null);
  const [runtime, setRuntime] = useState(null);
  const [runtimeAvailable, setRuntimeAvailable] = useState(null);

  const loadNetwork = useCallback(async () => {
    try {
      const net = await getNetwork(apiBase);
      setNetwork(net);
      setNetworkAvailable(true);
    } catch {
      setNetworkAvailable(false);
    }
  }, [apiBase]);

  useEffect(() => {
    loadNetwork();
    (async () => {
      try {
        setPower(await getPower(apiBase));
        setPowerAvailable(true);
      } catch {
        setPowerAvailable(false);
      }
    })();
    (async () => {
      try {
        setRuntime(await getRuntimeDebug(apiBase));
        setRuntimeAvailable(true);
      } catch {
        setRuntimeAvailable(false);
      }
    })();
  }, [apiBase, loadNetwork]);

  async function onScan() {
    setScanning(true);
    try {
      const res = await scanWifi(apiBase);
      setScanResults(res.networks ?? res.items ?? res ?? []);
    } catch (err) {
      setError(err.message || "WiFi scan unavailable");
      setScanResults([]);
    } finally {
      setScanning(false);
    }
  }

  async function onConnectWifi(e) {
    e.preventDefault();
    if (!ssid) return;
    setWifiBusy(true);
    try {
      await setWifi(apiBase, { ssid, psk });
      setPsk("");
      await loadNetwork();
    } catch (err) {
      setError(err.message || "WiFi connect failed");
    } finally {
      setWifiBusy(false);
    }
  }

  async function onToggleAp(on) {
    setApBusy(true);
    try {
      await setAp(apiBase, on);
      await loadNetwork();
    } catch (err) {
      setError(err.message || "Access point toggle failed");
    } finally {
      setApBusy(false);
    }
  }

  async function onChangeCaptureMode(mode) {
    try {
      await setRecordingCaptureMode(apiBase, mode);
      await refreshCaptureMode(apiBase);
    } catch (err) {
      setError(err.message || "Failed to change capture mode");
    }
  }

  const apOn = !!(network?.ap?.enabled ?? network?.ap_enabled);
  const isRecording = status?.recorder?.mode === "recording";
  const scanList = Array.isArray(scanResults) ? scanResults : [];

  return (
    <div className="stack">
      <div className="page-head">
        <span className="label">configuration</span>
        <h1 className="display">Setup</h1>
      </div>

      {/* WiFi */}
      <Card
        title="WiFi"
        action={
          networkAvailable !== false ? (
            <Button size="sm" variant="ghost" onClick={onScan} disabled={scanning}>
              {scanning ? "Scanning…" : "Scan"}
            </Button>
          ) : null
        }
      >
        {networkAvailable === false ? (
          <Unavailable />
        ) : (
          <>
            <div className="kv" style={{ alignItems: "center" }}>
              <span className="kv__k">connected</span>
              <span className="kv__v" style={{ display: "flex", alignItems: "center", gap: "var(--sp-3)" }}>
                {network?.wifi?.ssid || network?.ssid || "—"}
                {(network?.wifi?.ssid || network?.ssid) && (
                  <Button
                    size="sm"
                    variant="ghost"
                    style={{ color: "var(--accent)", fontSize: "0.7rem" }}
                    onClick={() => {
                      setSsid(network?.wifi?.ssid || network?.ssid || "");
                      setPsk("");
                    }}
                  >
                    change
                  </Button>
                )}
              </span>
            </div>

            {scanResults !== null && (
              <div style={{ margin: "var(--sp-3) 0" }}>
                {scanList.length === 0 ? (
                  <p className="dim" style={{ fontSize: "0.78rem" }}>
                    No networks found.
                  </p>
                ) : (
                  <ul className="files-list">
                    {scanList.map((n, i) => {
                      const name = typeof n === "string" ? n : n.ssid || n.name;
                      // support both `strength` (BLE GATT scan) and `signal` (HTTP API)
                      const strength =
                        typeof n === "object"
                          ? (n.strength ?? n.signal ?? null)
                          : null;
                      const isConnected =
                        name === (network?.wifi?.ssid || network?.ssid);
                      return (
                        <li key={`${name}-${i}`}>
                          <span className="file-meta">
                            <span className="name">
                              {name}
                              {isConnected && (
                                <span
                                  className="label"
                                  style={{
                                    marginLeft: "var(--sp-2)",
                                    color: "var(--ok)",
                                  }}
                                >
                                  connected
                                </span>
                              )}
                            </span>
                            {strength != null && (
                              <SignalBars strength={strength} />
                            )}
                          </span>
                          <Button size="sm" variant="ghost" onClick={() => setSsid(name)}>
                            {isConnected ? "Reconnect" : "Use"}
                          </Button>
                        </li>
                      );
                    })}
                  </ul>
                )}
              </div>
            )}

            <form className="field-group" onSubmit={onConnectWifi}>
              <input
                className="input"
                placeholder="SSID"
                value={ssid}
                onChange={(e) => setSsid(e.target.value)}
              />
              <input
                className="input"
                type="password"
                placeholder="Password"
                value={psk}
                onChange={(e) => setPsk(e.target.value)}
              />
              <Button type="submit" variant="primary" disabled={wifiBusy || !ssid}>
                {wifiBusy ? "Connecting…" : "Connect"}
              </Button>
            </form>
          </>
        )}
      </Card>

      {/* Access point */}
      <Card title="Access point">
        {networkAvailable === false ? (
          <Unavailable />
        ) : (
          <Toggle
            label={apOn ? "broadcasting" : "off"}
            checked={apOn}
            disabled={apBusy}
            onChange={onToggleAp}
          />
        )}
      </Card>

      {/* Stream + capture defaults */}
      <Card title="Stream default">
        <div className="field-group">
          <select
            className="select"
            value={streamMode}
            onChange={(e) => setStreamMode(e.target.value)}
          >
            <option value="webrtc">WebRTC (low latency)</option>
            <option value="mjpeg">MJPEG (fallback)</option>
            <option value="off">Off</option>
          </select>
          <select
            className="select"
            value={captureModeConfig?.current_mode ?? "dvgrab"}
            onChange={(e) => onChangeCaptureMode(e.target.value)}
            disabled={isRecording}
          >
            <option value="dvgrab">Capture: dvgrab + ffmpeg</option>
            <option value="ffmpeg-only">Capture: ffmpeg only</option>
          </select>
        </div>
      </Card>

      {/* Device info */}
      <Card title="Device">
        <div className="kv">
          <span className="kv__k">network mode</span>
          <span className="kv__v">{status?.network?.mode ?? "unknown"}</span>
        </div>
        <div className="kv">
          <span className="kv__k">stream source</span>
          <span className="kv__v">{status?.stream?.source ?? "—"}</span>
        </div>
        <div className="kv">
          <span className="kv__k">power</span>
          <span className="kv__v">
            {powerAvailable === false
              ? "unavailable"
              : power
              ? `${power.battery_percent ?? power.percent ?? "—"}%${
                  power.charging ? " ⚡" : ""
                }`
              : "…"}
          </span>
        </div>
        <div className="kv">
          <span className="kv__k">api</span>
          <span className="kv__v">{apiBase}</span>
        </div>
      </Card>

      {/* Diagnostics */}
      <Card title="Diagnostics">
        {runtimeAvailable === false && !status ? (
          <Unavailable>Diagnostics unavailable</Unavailable>
        ) : (
          <pre
            className="data"
            style={{
              margin: 0,
              fontSize: "0.7rem",
              color: "var(--text-dim)",
              whiteSpace: "pre-wrap",
              wordBreak: "break-word",
            }}
          >
            {JSON.stringify(
              {
                status: status ?? null,
                runtime: runtimeAvailable === false ? "unavailable" : runtime,
              },
              null,
              2
            )}
          </pre>
        )}
      </Card>
    </div>
  );
}
