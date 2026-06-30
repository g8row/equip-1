import React, {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import {
  discoverServers,
  getDefaultApiBase,
  getFiles,
  getRecordingCaptureMode,
  getStatus,
  probeServer,
} from "../api";

const SERVER_KEY = "equip1:selectedApiBase";
const STREAM_MODE_KEY = "equip1:streamMode";

const ServerContext = createContext(null);

export function ServerProvider({ children }) {
  const initialApiBase = localStorage.getItem(SERVER_KEY) || getDefaultApiBase();
  const [apiBase, setApiBaseState] = useState(initialApiBase);
  const [status, setStatus] = useState(null);
  const [files, setFiles] = useState([]);
  const [error, setError] = useState("");
  const [captureModeConfig, setCaptureModeConfig] = useState(null);
  const [reachable, setReachable] = useState(null); // null=unknown
  const [streamMode, setStreamModeState] = useState(
    () => localStorage.getItem(STREAM_MODE_KEY) || "webrtc"
  );

  const apiBaseRef = useRef(apiBase);
  apiBaseRef.current = apiBase;

  const setApiBase = useCallback((base) => {
    const clean = (base || "").trim().replace(/\/+$/, "");
    if (!clean) return;
    localStorage.setItem(SERVER_KEY, clean);
    setApiBaseState(clean);
  }, []);

  const setStreamMode = useCallback((mode) => {
    localStorage.setItem(STREAM_MODE_KEY, mode);
    setStreamModeState(mode);
  }, []);

  const refresh = useCallback(async (base = apiBaseRef.current) => {
    try {
      const [statusRes, fileRes] = await Promise.all([getStatus(base), getFiles(base)]);
      setStatus(statusRes);
      setFiles(fileRes.items ?? []);
      setReachable(true);
      setError("");
      return true;
    } catch (err) {
      setReachable(false);
      setError(err.message || "Failed to reach API");
      return false;
    }
  }, []);

  // Candidate local addresses to probe before falling back to LAN scan.
  const candidateServers = useCallback(() => {
    const host = typeof window !== "undefined" ? window.location.hostname : "";
    return [
      ...new Set(
        [
          getDefaultApiBase(),
          host ? `${window.location.protocol}//${host}:8000` : null,
          host ? `http://${host}:8000` : null,
          "http://127.0.0.1:8000",
          "http://localhost:8000",
        ].filter(Boolean)
      ),
    ];
  }, []);

  // Poll status + files for the active server.
  useEffect(() => {
    if (!apiBase) return undefined;
    let cancelled = false;

    (async () => {
      await refresh(apiBase);
      if (cancelled) return;
      try {
        const modeConfig = await getRecordingCaptureMode(apiBase);
        if (!cancelled) setCaptureModeConfig(modeConfig);
      } catch {
        /* capture-mode endpoint optional */
      }
    })();

    const id = setInterval(() => refresh(apiBase), 1500);
    return () => {
      cancelled = true;
      clearInterval(id);
    };
  }, [apiBase, refresh]);

  const refreshCaptureMode = useCallback(async (base = apiBaseRef.current) => {
    const modeConfig = await getRecordingCaptureMode(base);
    setCaptureModeConfig(modeConfig);
    return modeConfig;
  }, []);

  const value = useMemo(
    () => ({
      apiBase,
      setApiBase,
      status,
      files,
      error,
      setError,
      reachable,
      captureModeConfig,
      refresh,
      refreshCaptureMode,
      streamMode,
      setStreamMode,
      candidateServers,
      probeServer,
      discoverServers,
    }),
    [
      apiBase,
      setApiBase,
      status,
      files,
      error,
      reachable,
      captureModeConfig,
      refresh,
      refreshCaptureMode,
      streamMode,
      setStreamMode,
      candidateServers,
    ]
  );

  return <ServerContext.Provider value={value}>{children}</ServerContext.Provider>;
}

export function useServer() {
  const ctx = useContext(ServerContext);
  if (!ctx) throw new Error("useServer must be used within ServerProvider");
  return ctx;
}
