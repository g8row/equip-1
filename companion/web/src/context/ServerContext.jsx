import React, {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { useLocation } from "react-router-dom";
import {
  discoverServers,
  getDefaultApiBase,
  getFiles,
  getRecordingCaptureMode,
  getStatus,
  probeServer,
} from "../api";
import { isNative } from "../lib/native";

const SERVER_KEY = "equip1:selectedApiBase";
const STREAM_MODE_KEY = "equip1:streamMode";

// Poll cadence. The fast tier only runs while a view that actually needs
// near-live data (today: Viewfinder) is mounted and the app is in the
// foreground — everywhere else a calmer cadence is plenty and saves battery
// and radio. Backoff applies whenever the device is unreachable, regardless of
// which page is open, so we don't hammer a dead host.
const POLL_FAST_MS = 1500;
const POLL_IDLE_MS = 5000;
const POLL_BACKOFF_START_MS = 10000;
const POLL_BACKOFF_MAX_MS = 30000;

const ServerContext = createContext(null);

export function ServerProvider({ children }) {
  const location = useLocation();
  const hadStoredServer = !!localStorage.getItem(SERVER_KEY);
  const initialApiBase = localStorage.getItem(SERVER_KEY) || getDefaultApiBase();
  const [apiBase, setApiBaseState] = useState(initialApiBase);
  const [status, setStatus] = useState(null);
  const [files, setFiles] = useState([]);
  const [error, setError] = useState("");
  const [captureModeConfig, setCaptureModeConfig] = useState(null);
  const [reachable, setReachable] = useState(null); // null=unknown
  const [lastSeenAt, setLastSeenAt] = useState(null); // ms epoch of last good refresh
  const [hasStoredServer, setHasStoredServer] = useState(hadStoredServer);
  const [viewfinderActive, setViewfinderActive] = useState(false);
  // Assume foregrounded until the native App plugin says otherwise (web has
  // no such concept and should just always poll normally).
  const [foreground, setForeground] = useState(true);
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
    // First-run redirect only needs to fire before a device is ever chosen —
    // once the user has picked one (even earlier this session), treat them
    // as past onboarding so navigating "/" doesn't bounce back to Connect.
    setHasStoredServer(true);
  }, []);

  const setStreamMode = useCallback((mode) => {
    localStorage.setItem(STREAM_MODE_KEY, mode);
    setStreamModeState(mode);
  }, []);

  // Status-only refresh — the thing polled on a timer. Deliberately does NOT
  // touch `error`/`files`: connectivity failures are conveyed via `reachable`
  // + `lastSeenAt` (stale-state UI reads those), and `error` is left for
  // pages to use as their own transient action-error channel without a
  // successful poll tick wiping it out from under them 1.5s later.
  const refresh = useCallback(async (base = apiBaseRef.current) => {
    try {
      const statusRes = await getStatus(base);
      setStatus(statusRes);
      setReachable(true);
      setLastSeenAt(Date.now());
      return true;
    } catch {
      setReachable(false);
      return false;
    }
  }, []);

  // Files are comparatively rarely-changing and were previously fetched on
  // every 1.5s status tick for no reason. Fetched explicitly instead: once
  // per server, whenever the Files page is open, and after a recording stops.
  const refreshFiles = useCallback(async (base = apiBaseRef.current) => {
    try {
      const res = await getFiles(base);
      setFiles(res.items ?? []);
      return true;
    } catch {
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

  // Track native foreground/background transitions. Backgrounding pauses
  // polling outright (see the poll effect below); there's nothing to observe
  // while the WebView isn't visible, and it keeps the radio quiet.
  useEffect(() => {
    if (!isNative()) return undefined;
    let cancelled = false;
    let handle = null;
    import("@capacitor/app")
      .then(({ App: CapApp }) =>
        CapApp.addListener("appStateChange", ({ isActive }) => setForeground(isActive))
      )
      .then((h) => {
        if (cancelled) h.remove();
        else handle = h;
      })
      .catch(() => {
        // Plugin unavailable — stay foregrounded rather than freezing polling.
      });
    return () => {
      cancelled = true;
      handle?.remove();
    };
  }, []);

  // One-shot fetches whenever the active server changes: files (so the
  // Viewfinder "last recording" card and Files page have something on cold
  // start without waiting on a page visit) and the optional capture-mode
  // config. Neither belongs in the recurring status poll.
  useEffect(() => {
    if (!apiBase) return undefined;
    let cancelled = false;
    refreshFiles(apiBase);
    (async () => {
      try {
        const modeConfig = await getRecordingCaptureMode(apiBase);
        if (!cancelled) setCaptureModeConfig(modeConfig);
      } catch {
        /* capture-mode endpoint optional */
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [apiBase, refreshFiles]);

  // Re-fetch files whenever the Files page is opened — covers "a recording
  // finished on another page and I just navigated here" without any polling.
  useEffect(() => {
    if (!apiBase) return undefined;
    if (!location.pathname.startsWith("/files")) return undefined;
    refreshFiles(apiBase);
  }, [apiBase, location.pathname, refreshFiles]);

  // Poll status for the active server. Chained setTimeout (not setInterval):
  // the next poll is scheduled only once the previous one has settled, so a
  // slow request can never overlap with the next tick or land its (possibly
  // stale/out-of-order) result after a newer one. Paused entirely while
  // backgrounded; resumes immediately on foreground.
  useEffect(() => {
    if (!apiBase || !foreground) return undefined;
    let cancelled = false;
    let timer = null;
    let backoffMs = POLL_BACKOFF_START_MS;

    const tick = async () => {
      if (cancelled) return;
      const ok = await refresh(apiBase);
      if (cancelled) return;
      if (ok) {
        backoffMs = POLL_BACKOFF_START_MS;
        timer = setTimeout(tick, viewfinderActive ? POLL_FAST_MS : POLL_IDLE_MS);
      } else {
        timer = setTimeout(tick, backoffMs);
        backoffMs = Math.min(backoffMs * 2, POLL_BACKOFF_MAX_MS);
      }
    };

    tick();

    return () => {
      cancelled = true;
      if (timer) clearTimeout(timer);
    };
  }, [apiBase, refresh, viewfinderActive, foreground]);

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
      lastSeenAt,
      firstRun: !hasStoredServer,
      setViewfinderActive,
      captureModeConfig,
      refresh,
      refreshFiles,
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
      lastSeenAt,
      hasStoredServer,
      captureModeConfig,
      refresh,
      refreshFiles,
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
