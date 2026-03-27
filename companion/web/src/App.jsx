import React, { useCallback, useEffect, useRef, useMemo, useState } from "react";
import {
  discoverServers,
  getDefaultApiBase,
  getFileDownloadUrl,
  getFiles,
  getRecordingCaptureMode,
  getStatus,
  getStreamUrl,
  getWhepUrl,
  probeServer,
  setRecordingCaptureMode,
  startRecording,
  stopRecording,
} from "./api";

const SERVER_KEY = "equip1:selectedApiBase";

function formatBytes(bytes) {
  if (!bytes) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let value = bytes;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit += 1;
  }
  return `${value.toFixed(unit > 1 ? 1 : 0)} ${units[unit]}`;
}

function formatDuration(seconds) {
  const hh = String(Math.floor(seconds / 3600)).padStart(2, "0");
  const mm = String(Math.floor((seconds % 3600) / 60)).padStart(2, "0");
  const ss = String(seconds % 60).padStart(2, "0");
  return `${hh}:${mm}:${ss}`;
}

// ---------------------------------------------------------------------------
// WebRTC WHEP Player
// ---------------------------------------------------------------------------

function WhepPlayer({ whepUrl, active }) {
  const videoRef = useRef(null);
  const pcRef = useRef(null);
  const retryTimerRef = useRef(null);
  const retryCountRef = useRef(0);
  const [state, setState] = useState("idle"); // idle | connecting | live | retrying | error
  const [errorMsg, setErrorMsg] = useState("");

  const connect = useCallback(async () => {
    if (!whepUrl || !active) return;
    setState("connecting");
    setErrorMsg("");

    try {
      // Clean up any existing connection
      if (pcRef.current) {
        pcRef.current.close();
        pcRef.current = null;
      }

      const pc = new RTCPeerConnection({
        iceServers: [], // LAN only — no STUN needed
        bundlePolicy: "max-bundle",
      });
      pcRef.current = pc;

      pc.addTransceiver("video", { direction: "recvonly" });
      pc.addTransceiver("audio", { direction: "recvonly" });

      pc.ontrack = (event) => {
        if (videoRef.current && event.streams[0]) {
          videoRef.current.srcObject = event.streams[0];
          retryCountRef.current = 0;
          setState("live");
        }
      };

      pc.onconnectionstatechange = () => {
        if (pc.connectionState === "failed" || pc.connectionState === "closed") {
          setState("error");
          setErrorMsg(`WebRTC connection ${pc.connectionState}`);
        }
      };

      // Aggressive live-edge seeking to reduce playback buffer latency
      if (videoRef.current) {
        videoRef.current.addEventListener("progress", () => {
          const v = videoRef.current;
          if (!v || !v.buffered.length) return;
          const end = v.buffered.end(v.buffered.length - 1);
          if (end - v.currentTime > 0.4) {
            v.currentTime = end - 0.1;
          }
        }, { passive: true });
      }

      const offer = await pc.createOffer();
      await pc.setLocalDescription(offer);

      const res = await fetch(whepUrl, {
        method: "POST",
        headers: { "Content-Type": "application/sdp" },
        body: offer.sdp,
      });

      // 503 = stream not ready yet (ffmpeg connecting to mediamtx); auto-retry
      if (res.status === 503) {
        const MAX_RETRIES = 15;
        const attempt = retryCountRef.current + 1;
        if (attempt <= MAX_RETRIES) {
          retryCountRef.current = attempt;
          setState("retrying");
          setErrorMsg(`Stream not ready — retrying (${attempt}/${MAX_RETRIES})…`);
          retryTimerRef.current = setTimeout(connect, 4000);
          return;
        }
      }

      if (!res.ok) {
        const text = await res.text().catch(() => res.status.toString());
        throw new Error(`WHEP ${res.status}: ${text}`);
      }

      const answerSdp = await res.text();
      await pc.setRemoteDescription({ type: "answer", sdp: answerSdp });
    } catch (err) {
      setState("error");
      setErrorMsg(err.message || "WebRTC connection failed");
    }
  }, [whepUrl, active]);

  const disconnect = useCallback(() => {
    if (retryTimerRef.current) {
      clearTimeout(retryTimerRef.current);
      retryTimerRef.current = null;
    }
    retryCountRef.current = 0;
    if (pcRef.current) {
      pcRef.current.close();
      pcRef.current = null;
    }
    if (videoRef.current) {
      videoRef.current.srcObject = null;
    }
    setState("idle");
    setErrorMsg("");
  }, []);

  // Auto-connect when active; disconnect when not
  useEffect(() => {
    if (active) {
      connect();
    } else {
      disconnect();
    }
    return () => {
      if (retryTimerRef.current) {
        clearTimeout(retryTimerRef.current);
        retryTimerRef.current = null;
      }
      if (pcRef.current) {
        pcRef.current.close();
        pcRef.current = null;
      }
    };
  }, [active, whepUrl]); // eslint-disable-line react-hooks/exhaustive-deps

  const stateColor = { idle: "#888", connecting: "#e6a817", retrying: "#e6a817", live: "#22c55e", error: "#ef4444" };

  return (
    <div className="whep-player">
      <div className="stream-status-row">
        <span style={{ color: stateColor[state], fontWeight: "bold", fontSize: "0.9rem" }}>
          ● {state.toUpperCase()}
        </span>
        <div className="stream-btn-row">
          <button onClick={connect} disabled={state === "connecting"} className="btn-sm">
            Reconnect
          </button>
          <button onClick={disconnect} disabled={state === "idle"} className="btn-sm">
            Disconnect
          </button>
        </div>
      </div>
      {errorMsg && <p className="stream-error">{errorMsg}</p>}
      <video
        ref={videoRef}
        autoPlay
        muted
        playsInline
        className="stream-preview"
        style={{ background: "#000" }}
      />
    </div>
  );
}

// ---------------------------------------------------------------------------
// MJPEG fallback player
// ---------------------------------------------------------------------------

function MjpegPlayer({ streamUrl, active }) {
  const [nonce, setNonce] = useState(Date.now());
  const [imgError, setImgError] = useState("");

  useEffect(() => {
    if (active) setNonce(Date.now());
  }, [active]);

  const src = active ? `${streamUrl}?t=${nonce}` : "";

  return (
    <div>
      <div className="stream-btn-row" style={{ marginBottom: "0.5rem" }}>
        <button className="btn-sm" onClick={() => { setImgError(""); setNonce(Date.now()); }} disabled={!active}>
          Restart
        </button>
      </div>
      {imgError && <p className="stream-error">{imgError}</p>}
      {active ? (
        <img
          className="stream-preview"
          src={src}
          alt="MJPEG stream"
          onError={() => setImgError("MJPEG stream unavailable")}
          onLoad={() => setImgError("")}
        />
      ) : (
        <div className="stream-placeholder">Enable MJPEG to preview</div>
      )}
    </div>
  );
}

// ---------------------------------------------------------------------------
// App
// ---------------------------------------------------------------------------

export default function App() {
  const initialApiBase = localStorage.getItem(SERVER_KEY) || getDefaultApiBase();
  const [apiBase, setApiBase] = useState(initialApiBase);
  const [manualServer, setManualServer] = useState(initialApiBase);
  const [isDiscovering, setIsDiscovering] = useState(false);
  const [discoveredServers, setDiscoveredServers] = useState([]);
  const [status, setStatus] = useState(null);
  const [files, setFiles] = useState([]);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);
  const [captureModeConfig, setCaptureModeConfig] = useState(null);
  const [captureModeLoading, setCaptureModeLoading] = useState(false);

  // Stream mode: "webrtc" | "mjpeg" | "off"
  const [streamMode, setStreamMode] = useState("webrtc");

  function candidateServers() {
    const host = window.location.hostname;
    return [...new Set([
      getDefaultApiBase(),
      "http://127.0.0.1:8000",
      "http://localhost:8000",
      host ? `http://${host}:8000` : null,
    ].filter(Boolean))];
  }

  async function autoSelectServer() {
    setIsDiscovering(true);
    setDiscoveredServers([]);
    setError("");
    try {
      const candidates = candidateServers();
      for (const base of candidates) {
        const ok = await probeServer(base);
        if (ok) {
          setApiBase(base);
          setManualServer(base);
          localStorage.setItem(SERVER_KEY, base);
          return;
        }
      }

      const discovered = await discoverServers({
        seeds: [window.location.hostname, manualServer, ...candidates],
      });
      setDiscoveredServers(discovered);

      if (discovered.length > 0) {
        const selected = discovered[0].base;
        setApiBase(selected);
        setManualServer(selected);
        localStorage.setItem(SERVER_KEY, selected);
        return;
      }

      setError(
        `No API server found. Checked common addresses and scanned LAN prefixes from: ${[
          window.location.hostname,
          manualServer,
        ].filter(Boolean).join(", ") || "none"}`
      );
    } finally {
      setIsDiscovering(false);
    }
  }

  async function applyManualServer() {
    const base = manualServer.trim().replace(/\/+$/, "");
    if (!base) { setError("Enter a server URL first"); return; }
    const ok = await probeServer(base);
    if (!ok) { setError(`Cannot reach ${base}/health`); return; }
    setApiBase(base);
    localStorage.setItem(SERVER_KEY, base);
    setError("");
  }

  async function refresh(base = apiBase) {
    setError("");
    try {
      const [statusRes, fileRes] = await Promise.all([getStatus(base), getFiles(base)]);
      setStatus(statusRes);
      setFiles(fileRes.items ?? []);
      return true;
    } catch (err) {
      setError(err.message || "Failed to reach API");
      return false;
    }
  }

  useEffect(() => { autoSelectServer(); }, []);

  useEffect(() => {
    if (!apiBase) return undefined;
    let cancelled = false;

    async function init() {
      await refresh(apiBase);
      if (!cancelled) {
        try {
          const modeConfig = await getRecordingCaptureMode(apiBase);
          if (!cancelled) setCaptureModeConfig(modeConfig);
        } catch (_) { /* ignore */ }
      }
    }
    init();

    const id = setInterval(() => refresh(apiBase), 1500);
    return () => { cancelled = true; clearInterval(id); };
  }, [apiBase]);

  const isRecording = status?.recorder?.mode === "recording";
  const timer = useMemo(() => formatDuration(status?.recorder?.elapsed_seconds ?? 0), [status]);

  async function onToggleRecording() {
    setLoading(true);
    try {
      if (isRecording) { await stopRecording(apiBase); }
      else { await startRecording(apiBase); }
      await refresh();
    } catch (err) {
      setError(err.message || "Toggle failed");
    } finally {
      setLoading(false);
    }
  }

  async function onChangeRecordingMode(newMode) {
    setCaptureModeLoading(true);
    try {
      await setRecordingCaptureMode(apiBase, newMode);
      const modeConfig = await getRecordingCaptureMode(apiBase);
      setCaptureModeConfig(modeConfig);
    } catch (err) {
      setError(err.message || "Failed to change recording mode");
    } finally {
      setCaptureModeLoading(false);
    }
  }

  const whepUrl = useMemo(() => getWhepUrl(apiBase), [apiBase]);
  const mjpegUrl = useMemo(() => getStreamUrl(apiBase), [apiBase]);
  const mediamtxRunning = status?.stream?.mediamtx_running ?? false;

  return (
    <main className="page">
      <section className="hero">
        <h1>equip-1 companion local</h1>
        <p>Localhost prototype: assumes the device API is reachable on your local network.</p>
      </section>

      <section className="card server-selector">
        <h2>Server</h2>
        <p className="server-now">Using: {apiBase}</p>
        <div className="server-row">
          <button onClick={autoSelectServer} disabled={isDiscovering}>
            {isDiscovering ? "Scanning..." : "Auto Select"}
          </button>
          <input
            type="text"
            value={manualServer}
            onChange={(e) => setManualServer(e.target.value)}
            placeholder="http://192.168.x.x:8000"
          />
          <button onClick={applyManualServer}>Use Manual</button>
        </div>
        {discoveredServers.length > 0 ? (
          <div className="discovery-results">
            <h3>Discovered Devices</h3>
            <ul>
              {discoveredServers.map((server) => (
                <li key={server.base}>
                  <button
                    className="discovery-item"
                    onClick={() => {
                      setApiBase(server.base);
                      setManualServer(server.base);
                      localStorage.setItem(SERVER_KEY, server.base);
                    }}
                  >
                    <span>{server.base}</span>
                    <span>{server.hostname || "unknown-host"}</span>
                  </button>
                </li>
              ))}
            </ul>
          </div>
        ) : null}
      </section>

      {error ? <div className="error">{error}</div> : null}

      <section className="grid">
        <article className="card">
          <h2>Recorder</h2>
          <div className="status-row">
            <span className={isRecording ? "dot live" : "dot idle"} />
            <strong>{isRecording ? "Recording" : "Idle"}</strong>
          </div>
          <p className="timer">{timer}</p>
          <button onClick={onToggleRecording} disabled={loading}>
            {isRecording ? "Stop Recording" : "Start Recording"}
          </button>

          <div style={{ marginTop: "1rem", paddingTop: "1rem", borderTop: "1px solid #ccc" }}>
            <label htmlFor="capture-mode-select" style={{ display: "block", marginBottom: "0.5rem", fontWeight: "bold" }}>
              Capture Mode
            </label>
            <select
              id="capture-mode-select"
              value={captureModeConfig?.current_mode ?? "dvgrab"}
              onChange={(e) => onChangeRecordingMode(e.target.value)}
              disabled={captureModeLoading || isRecording}
              style={{ width: "100%", padding: "0.5rem" }}
            >
              <option value="dvgrab">dvgrab + ffmpeg</option>
              <option value="ffmpeg-only">ffmpeg only (iec61883)</option>
            </select>
            <p style={{ fontSize: "0.85rem", color: "#666", marginTop: "0.5rem" }}>
              {captureModeConfig?.recorder_is_active
                ? "Cannot change mode while recording"
                : "Compare recording capabilities between capture methods"}
            </p>
          </div>
        </article>

        <article className="card">
          <h2>Storage</h2>
          <p>Total: {formatBytes(status?.storage?.total_bytes ?? 0)}</p>
          <p>Used: {formatBytes(status?.storage?.used_bytes ?? 0)}</p>
          <p>Free: {formatBytes(status?.storage?.free_bytes ?? 0)}</p>
          <p>Used %: {status?.storage?.used_percent ?? 0}%</p>
        </article>

        <article className="card">
          <h2>Network</h2>
          <p>Mode: {status?.network?.mode ?? "unknown"}</p>
          <p>{status?.network?.hint ?? ""}</p>
          <button onClick={refresh}>Refresh</button>
        </article>
      </section>

      {/* ------------------------------------------------------------------ */}
      {/* Stream section                                                        */}
      {/* ------------------------------------------------------------------ */}
      <section className="card stream-card">
        <div className="stream-header-row">
          <h2>Live Preview</h2>
          <div className="stream-mode-tabs">
            <button
              className={streamMode === "webrtc" ? "tab active" : "tab"}
              onClick={() => setStreamMode("webrtc")}
            >
              WebRTC <span className="badge">~100ms</span>
            </button>
            <button
              className={streamMode === "mjpeg" ? "tab active" : "tab"}
              onClick={() => setStreamMode("mjpeg")}
            >
              MJPEG <span className="badge">~200ms</span>
            </button>
            <button
              className={streamMode === "off" ? "tab active" : "tab"}
              onClick={() => setStreamMode("off")}
            >
              Off
            </button>
          </div>
        </div>

        <div className="stream-info-row">
          <span style={{ fontSize: "0.8rem", color: mediamtxRunning ? "#22c55e" : "#e6a817" }}>
            ● mediamtx {mediamtxRunning ? "running" : "not running"}
          </span>
          <span style={{ fontSize: "0.8rem", color: "#888" }}>
            Source: {status?.stream?.source ?? "—"}
          </span>
        </div>

        {streamMode === "webrtc" && (
          <WhepPlayer
            whepUrl={whepUrl}
            active={streamMode === "webrtc" && !!apiBase}
          />
        )}

        {streamMode === "mjpeg" && (
          <MjpegPlayer
            streamUrl={mjpegUrl}
            active={streamMode === "mjpeg" && !!apiBase}
          />
        )}

        {streamMode === "off" && (
          <div className="stream-placeholder">Stream paused</div>
        )}
      </section>

      <section className="card files">
        <h2>Saved Videos</h2>
        {files.length === 0 ? <p>No .dv files found yet.</p> : null}
        <ul>
          {files.map((file) => (
            <li key={`${file.name}-${file.modified_unix}`}>
              <span>{file.name}</span>
              <span className="file-actions">
                <span>{formatBytes(file.size_bytes)}</span>
                <a
                  href={getFileDownloadUrl(apiBase, file.name)}
                  target="_blank"
                  rel="noreferrer"
                  download={file.name}
                  className="download-link"
                >
                  Download
                </a>
              </span>
            </li>
          ))}
        </ul>
      </section>
    </main>
  );
}
