import React, { useCallback, useEffect, useState } from "react";
import { useServer } from "../context/ServerContext";
import {
  getNetwork,
  getPower,
  getRuntimeDebug,
  restartServices,
  scanWifi,
  setAp,
  setRecordingCaptureMode,
  setWifi,
} from "../api";
import Card from "../components/ui/Card";
import Button from "../components/ui/Button";
import Toggle from "../components/ui/Toggle";
import SignalBars from "../components/ui/SignalBars";
import ConfirmDialog from "../components/ui/ConfirmDialog";
import { pskError, phoneWifiIp } from "../lib/wifi";

function Unavailable({ children = "Unavailable on this device" }) {
  return <p className="muted-box">{children}</p>;
}

export default function Settings() {
  const {
    apiBase,
    setApiBase,
    status,
    captureModeConfig,
    refreshCaptureMode,
    streamMode,
    setStreamMode,
    setError,
    refresh,
    reachable,
    candidateServers,
    probeServer,
    discoverServers,
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
  const [restarting, setRestarting] = useState(false);
  // Set while switching the device from its hotspot onto a WiFi network: the
  // radio flips AP→station, the hotspot drops, and this HTTP session dies — so
  // we can't just await the response. Holds { ssid } and drives a hand-off panel
  // that guides the user to rejoin that network and re-find the device on the LAN.
  const [apSwitching, setApSwitching] = useState(null);
  const [findingDevice, setFindingDevice] = useState(false);
  // In-app confirm modal (replaces window.confirm, which is unbranded on
  // Capacitor). askConfirm(opts) resolves true/false — a drop-in for confirm().
  const [confirm, setConfirm] = useState(null);
  const askConfirm = (opts) =>
    new Promise((resolve) => setConfirm({ ...opts, resolve }));

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
    if (!ssid || pskError(psk)) return;

    // On the hotspot: applying WiFi creds flips the radio AP→station, so THIS
    // connection drops and the response never arrives. Fire it best-effort and
    // switch to the hand-off panel rather than awaiting (which would hang then
    // error, stranding the user). This is the AP→network switch without BLE.
    if (onAp) {
      const target = ssid;
      setApSwitching({ ssid: target });
      setPsk("");
      Promise.race([
        setWifi(apiBase, { ssid: target, psk }),
        new Promise((r) => setTimeout(r, 4000)),
      ]).catch(() => {});
      return;
    }

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

  // After the device joins the chosen network and its hotspot drops, the user
  // rejoins that network on their phone and taps Find device — this re-locates
  // the device on the LAN (no BLE). Seeds discovery with the phone's real subnet.
  async function findDeviceOnLan() {
    setFindingDevice(true);
    setError("");
    try {
      const bases = candidateServers();
      for (const base of bases) {
        if (await probeServer(base)) {
          setApiBase(base);
          await refresh(base);
          setApSwitching(null);
          return;
        }
      }
      const phoneIp = await phoneWifiIp();
      const found = await discoverServers({
        seeds: [phoneIp, window.location.hostname, ...bases].filter(Boolean),
      });
      if (found.length > 0) {
        setApiBase(found[0].base);
        await refresh(found[0].base);
        setApSwitching(null);
        return;
      }
      setError(
        `Couldn't find the device on "${apSwitching?.ssid}" yet — make sure your phone is on that network, then try again.`
      );
    } finally {
      setFindingDevice(false);
    }
  }

  async function onToggleAp(on) {
    // Turning the hotspot off while we're connected *through* it severs this
    // very session. Make the user confirm rather than silently dropping them.
    if (!on && onAp) {
      const ok = await askConfirm({
        title: "Turn off hotspot?",
        message:
          "You're connected over the device hotspot. Turning it off will end this connection.",
        confirmLabel: "Turn off",
        danger: true,
      });
      if (!ok) return;
    }
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

  async function onRestartServices() {
    const ok = await askConfirm({
      title: "Restart services?",
      message:
        "Restart BLE and streaming services. The live preview and any Bluetooth connection will briefly drop.",
      confirmLabel: "Restart",
    });
    if (!ok) return;
    setRestarting(true);
    try {
      await restartServices(apiBase);
      setTimeout(() => refresh(), 2500);
    } catch (err) {
      setError(err.message || "Restart failed");
    } finally {
      setRestarting(false);
    }
  }

  const isRecording = status?.recorder?.mode === "recording";
  const scanList = Array.isArray(scanResults) ? scanResults : [];
  // Single-radio device: the chip is EITHER an AP or a WiFi station, never
  // both. If we reached the device over its own hotspot, joining a WiFi network
  // (or toggling the AP off) will drop this very connection. Surface that.
  const onAp = !!apiBase && apiBase.includes("192.168.0.1");
  // The API reports AP state as a boolean `ap` (and mode:"ap") — NOT `ap.enabled`
  // or `ap_enabled`, which is what this used to read (always undefined → toggle
  // stuck "off"). Also force-on when we reached the device through its hotspot,
  // since the AP is then on by definition.
  const apOn = onAp || network?.ap === true || network?.mode === "ap";
  // Actions that hit the device API are pointless (and just error) when it's
  // unreachable. reachable is null while the first probe is in flight — treat
  // only an explicit false as offline so we don't disable during startup.
  const offline = reachable === false;

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
          networkAvailable !== false && !onAp && !apSwitching ? (
            <Button size="sm" variant="ghost" onClick={onScan} disabled={scanning}>
              {scanning ? "Scanning…" : "Scan"}
            </Button>
          ) : null
        }
      >
        {networkAvailable === false ? (
          <Unavailable />
        ) : apSwitching ? (
          // AP → network hand-off (#3): applying WiFi creds over the hotspot drops
          // this connection, so guide the user to rejoin the target network and
          // re-find the device on the LAN — no BLE round-trip needed.
          <div className="stack" style={{ gap: "var(--sp-3)" }}>
            <p style={{ fontSize: "0.85rem", margin: 0 }}>
              Switching the device to <strong>{apSwitching.ssid}</strong>. Its
              hotspot is turning off, so this connection will drop.
            </p>
            <ol
              className="dim"
              style={{ fontSize: "0.8rem", margin: 0, paddingLeft: "1.1rem", lineHeight: 1.6 }}
            >
              <li>
                On your phone, open WiFi settings and join{" "}
                <strong>{apSwitching.ssid}</strong>.
              </li>
              <li>
                Come back here and tap <strong>Find device</strong>.
              </li>
            </ol>
            <div style={{ display: "flex", gap: "var(--sp-2)" }}>
              <Button variant="primary" onClick={findDeviceOnLan} disabled={findingDevice}>
                {findingDevice ? "Searching…" : "Find device"}
              </Button>
              <Button variant="ghost" onClick={() => setApSwitching(null)} disabled={findingDevice}>
                Cancel
              </Button>
            </div>
          </div>
        ) : (
          <>
            <p className="dim" style={{ fontSize: "0.78rem", marginTop: 0, marginBottom: "var(--sp-3)" }}>
              Connect the device to a WiFi network so you can reach it without its
              hotspot. This does <strong>not</strong> rename the hotspot.
            </p>
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
                aria-label="WiFi network name"
                value={ssid}
                onChange={(e) => setSsid(e.target.value)}
              />
              <input
                className="input"
                type="password"
                placeholder="Password"
                aria-label="WiFi password"
                value={psk}
                onChange={(e) => setPsk(e.target.value)}
              />
              {pskError(psk) && (
                <p className="dim" style={{ fontSize: "0.75rem", margin: 0, color: "var(--warn)" }}>
                  {pskError(psk)}
                </p>
              )}
              <Button
                type="submit"
                variant="primary"
                disabled={wifiBusy || !ssid || !!pskError(psk)}
              >
                {wifiBusy ? "Connecting…" : onAp ? "Switch to network" : "Connect"}
              </Button>
            </form>
            <p className="dim" style={{ fontSize: "0.75rem", marginTop: "var(--sp-2)" }}>
              {onAp && (
                <>
                  The device can&apos;t list nearby networks while its hotspot is
                  on (one radio) — type the network name above.{" "}
                </>
              )}
              The device joins one network at a time. 2.4GHz networks work best —
              5GHz isn&apos;t supported yet.
              {onAp && (
                <>
                  {" "}
                  <strong style={{ color: "var(--warn)" }}>
                    Joining a network turns the hotspot off and drops this
                    connection — you&apos;ll rejoin that network on your phone
                    next.
                  </strong>
                </>
              )}
            </p>
          </>
        )}
      </Card>

      {/* Access point */}
      <Card title="Access point">
        {networkAvailable === false ? (
          <Unavailable />
        ) : (
          <>
            <Toggle
              label={apOn ? "broadcasting" : "off"}
              checked={apOn}
              disabled={apBusy}
              onChange={onToggleAp}
            />
            <p className="dim" style={{ fontSize: "0.75rem", marginTop: "var(--sp-2)" }}>
              The device has one radio: the hotspot and WiFi can&apos;t run at
              once. Turning the hotspot on drops any WiFi connection;
              {onAp
                ? " turning it off will end this connection."
                : " turning it off returns the device to WiFi."}
            </p>
          </>
        )}
      </Card>

      {/* Stream + capture defaults */}
      <Card title="Preview &amp; capture">
        <div className="field-group">
          <label className="label" htmlFor="stream-mode">
            Live preview
          </label>
          <select
            id="stream-mode"
            className="select"
            value={streamMode}
            onChange={(e) => setStreamMode(e.target.value)}
          >
            <option value="webrtc">WebRTC (low latency)</option>
            <option value="mjpeg">MJPEG (fallback)</option>
            <option value="off">Off</option>
          </select>

          <label className="label" htmlFor="capture-mode" style={{ marginTop: "var(--sp-3)" }}>
            Recording capture
          </label>
          <select
            id="capture-mode"
            className="select"
            value={captureModeConfig?.current_mode ?? "dvgrab"}
            onChange={(e) => onChangeCaptureMode(e.target.value)}
            disabled={isRecording || offline}
          >
            <option value="dvgrab">Shared with preview (default)</option>
            <option value="ffmpeg-only">Separate recorder</option>
          </select>
          <p className="dim" style={{ fontSize: "0.75rem", marginTop: "var(--sp-2)" }}>
            Advanced. &ldquo;Shared&rdquo; records through the same pipeline as the
            live preview; &ldquo;Separate&rdquo; runs an independent recorder. Both
            save lossless DV.
            {offline
              ? " Connect to the device to change this."
              : isRecording && " Stop recording to change this."}
          </p>
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
        <Button
          size="sm"
          variant="ghost"
          disabled={restarting || offline}
          onClick={onRestartServices}
          style={{ marginTop: "var(--sp-3)" }}
        >
          {restarting
            ? "Restarting…"
            : offline
            ? "Restart services (device offline)"
            : "Restart BLE + streaming services"}
        </Button>
      </Card>

      {/* About */}
      <Card title="About">
        <div style={{ display: "flex", flexDirection: "column", gap: "var(--sp-2)" }}>
          <h2 className="display" style={{ fontSize: "1.4rem", margin: 0 }}>
            equip&middot;1
          </h2>
          <p className="dim" style={{ fontSize: "0.8rem", margin: 0, lineHeight: 1.5 }}>
            Compact DV recorder. Connects to any FireWire camcorder and saves
            footage directly to microSD — no laptop required.
          </p>
        </div>
        <div className="kv">
          <span className="kv__k">board</span>
          <span className="kv__v">Radxa ROCK 2F &middot; RK3528A</span>
        </div>
        <div className="kv">
          <span className="kv__k">firewire</span>
          <span className="kv__v">Firehat &middot; VIA VT6315N</span>
        </div>
        <div className="kv">
          <span className="kv__k">formats</span>
          <span className="kv__v" style={{ wordBreak: "normal" }}>
            MiniDV, DVCAM, DVCPRO, Digital8, HDV
          </span>
        </div>
        <div className="row-wrap" style={{ marginTop: "var(--sp-3)" }}>
          <a
            className="btn btn--sm btn--ghost"
            href="https://github.com/computerequipmentgroup/equip-1"
            target="_blank"
            rel="noreferrer"
          >
            GitHub
          </a>
          <a
            className="btn btn--sm btn--ghost"
            href="https://discord.gg/wpXmcb5mvK"
            target="_blank"
            rel="noreferrer"
          >
            Discord
          </a>
          <a
            className="btn btn--sm btn--ghost"
            href="https://www.crowdsupply.com/computer-equipment-group/equip-1"
            target="_blank"
            rel="noreferrer"
          >
            Crowd Supply
          </a>
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

      <ConfirmDialog
        open={!!confirm}
        title={confirm?.title}
        message={confirm?.message}
        confirmLabel={confirm?.confirmLabel}
        danger={confirm?.danger}
        onConfirm={() => {
          confirm?.resolve(true);
          setConfirm(null);
        }}
        onCancel={() => {
          confirm?.resolve(false);
          setConfirm(null);
        }}
      />
    </div>
  );
}
