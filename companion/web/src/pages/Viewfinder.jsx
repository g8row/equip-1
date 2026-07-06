import React, { useEffect, useMemo, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { useServer } from "../context/ServerContext";
import {
  getStreamUrl,
  getWhepUrl,
  getFileDownloadUrl,
  startRecording,
  stopRecording,
} from "../api";
import { formatDuration, formatBytes, formatDate } from "../lib/format";
import { streamIssue } from "../lib/stream";
import { hapticImpact, hapticNotification } from "../lib/haptics";
import { canShareNative, shareFile } from "../lib/share";
import WhepPlayer from "../components/WhepPlayer";
import MjpegPlayer from "../components/MjpegPlayer";
import Thumbnail from "../components/Thumbnail";
import Card from "../components/ui/Card";
import Button from "../components/ui/Button";
import StatusDot from "../components/ui/StatusDot";

// "3s" / "1m 05s" — used for the frozen-timer "last seen" badge while the
// device is unreachable. Kept local since it's only meaningful here.
function formatAgo(ms) {
  const s = Math.max(0, Math.round(ms / 1000));
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  return `${m}m ${String(s % 60).padStart(2, "0")}s`;
}

export default function Viewfinder() {
  const {
    apiBase,
    status,
    files,
    refresh,
    refreshFiles,
    streamMode,
    setStreamMode,
    reachable,
    lastSeenAt,
    setViewfinderActive,
  } = useServer();
  const [sharing, setSharing] = useState(false);
  const [loading, setLoading] = useState(false);
  // Action errors (toggle/share) are local and transient — they used to share
  // the context's connectivity `error`, which a successful poll tick wiped
  // out from under the user within ~1.5s.
  const [actionError, setActionError] = useState("");
  // A one-time notice for "the device auto-stopped while you were away",
  // surfaced once reachability returns (T4.4's last_stop_reason).
  const [returnNotice, setReturnNotice] = useState("");

  const isStale = reachable === false;

  // Tell the poll loop the fast (1.5s) cadence is worth running.
  useEffect(() => {
    setViewfinderActive(true);
    return () => setViewfinderActive(false);
  }, [setViewfinderActive]);

  // Force a re-render every second while stale so the "last seen Ns ago"
  // badge keeps counting instead of freezing on the value at disconnect.
  const [, tick] = useState(0);
  useEffect(() => {
    if (!isStale) return undefined;
    const id = setInterval(() => tick((n) => n + 1), 1000);
    return () => clearInterval(id);
  }, [isStale]);

  // Surface an auto-stop that happened while disconnected, once, when the
  // connection comes back. Inert until the backend exposes
  // status.recorder.last_stop_reason (T4.4) — safe no-op until then.
  const prevReachableRef = useRef(reachable);
  useEffect(() => {
    const wasUnreachable = prevReachableRef.current === false;
    prevReachableRef.current = reachable;
    if (!wasUnreachable || reachable !== true) return;
    const reason = status?.recorder?.last_stop_reason;
    if (reason && reason !== "user") {
      setReturnNotice(
        `Recording stopped while disconnected (${reason.replace(/_/g, " ")}).`
      );
    }
  }, [reachable, status]);

  const isRecording = status?.recorder?.mode === "recording";
  const timer = useMemo(
    () => formatDuration(status?.recorder?.elapsed_seconds ?? 0),
    [status]
  );

  const whepUrl = useMemo(() => getWhepUrl(apiBase), [apiBase]);
  const mjpegUrl = useMemo(() => getStreamUrl(apiBase), [apiBase]);
  const streamUp = status?.stream?.mediamtx_running ?? false;
  const currentStreamIssue = streamIssue(status, streamMode);
  // WebRTC can be unavailable (e.g. no WHEP-capable encoder) while MJPEG still
  // works from the same camera. Rather than send the user to Settings, offer a
  // one-tap switch right where the problem shows, as long as the camera is
  // present. MJPEG now works on native too (MjpegPlayer parses the stream and
  // renders blob-URL frames), so this is offered on all platforms.
  const canFallbackToMjpeg =
    streamMode === "webrtc" &&
    status?.stream?.whep_available === false &&
    status?.stream?.requirements?.camera_present !== false;

  const freeBytes = status?.storage?.free_bytes;
  // Mirrors the backend's recorder thresholds (200MB blocks starting,
  // 50MB auto-stops an in-progress recording) — warn before either bites.
  const storageLevel =
    freeBytes == null
      ? null
      : freeBytes < 50 * 1024 * 1024
      ? "critical"
      : freeBytes < 200 * 1024 * 1024
      ? "low"
      : null;

  const lastFile = !isRecording && files.length > 0 ? files[0] : null;

  // Don't offer Record when the backend would just reject the start: it blocks
  // starting below 200MB free (storageLevel != null), needs a camera, and needs
  // to be reachable. Stop, however, must always stay available once recording.
  const noCamera = status?.stream?.requirements?.camera_present === false;
  const startBlockedReason =
    reachable === false
      ? "Device unreachable."
      : noCamera
      ? "No FireWire camera detected."
      : storageLevel != null
      ? "Not enough free storage to start."
      : null;
  const recordDisabled = loading || (!isRecording && startBlockedReason != null);

  async function onShareLast() {
    if (!lastFile) return;
    setSharing(true);
    try {
      await shareFile(getFileDownloadUrl(apiBase, lastFile.name), lastFile.name);
    } catch (err) {
      if (!/cancel/i.test(err.message || "")) {
        setActionError(err.message || "Share failed");
      }
    } finally {
      setSharing(false);
    }
  }

  async function onToggleRecording() {
    setActionError("");
    setLoading(true);
    try {
      if (isRecording) {
        await stopRecording(apiBase);
        hapticImpact("Light");
        await refresh();
        await refreshFiles();
      } else {
        await startRecording(apiBase);
        hapticImpact("Heavy");
        await refresh();
      }
    } catch (err) {
      setActionError(err.message || "Toggle failed");
      hapticNotification("Error");
    } finally {
      setLoading(false);
    }
  }

  return (
    <div className="stack">
      <div className="page-head">
        <span className="label">viewfinder</span>
        <h1 className="display">Live</h1>
      </div>

      {isStale && (
        <div className="notice notice--hazard" role="alert">
          Connection lost — the device may still be recording.
          {lastSeenAt ? ` Last seen ${formatAgo(Date.now() - lastSeenAt)} ago.` : ""}
        </div>
      )}

      {returnNotice && (
        <div className="notice" role="status" aria-live="polite">
          {returnNotice}{" "}
          <Button size="sm" variant="ghost" onClick={() => setReturnNotice("")}>
            Dismiss
          </Button>
        </div>
      )}

      {actionError ? (
        <div className="notice" role="alert">
          {actionError}
        </div>
      ) : null}

      {storageLevel && (
        <div className={`notice ${storageLevel === "critical" ? "notice--hazard" : ""}`}>
          {storageLevel === "critical"
            ? `Storage critically low — ${formatBytes(freeBytes)} free. Recording will auto-stop to avoid a corrupt file.`
            : `Storage running low — ${formatBytes(freeBytes)} free. Delete some recordings soon.`}
        </div>
      )}

      <Card>
        <div className="card__head">
          <div className="status-line">
            <StatusDot state={streamMode === "off" ? "idle" : streamUp ? "ok" : "warn"} />
            <span className="label">
              {streamMode === "off"
                ? "preview off"
                : streamUp
                ? "stream ready"
                : "starting…"}
            </span>
          </div>
        </div>

        {currentStreamIssue ? (
          <div className="stream-notice">
            {currentStreamIssue}
            {canFallbackToMjpeg && (
              <Button
                size="sm"
                variant="ghost"
                onClick={() => setStreamMode("mjpeg")}
                style={{ marginTop: "var(--sp-2)" }}
              >
                Switch to MJPEG
              </Button>
            )}
          </div>
        ) : null}

        {streamMode === "webrtc" && (
          <WhepPlayer
            whepUrl={whepUrl}
            active={streamMode === "webrtc" && !!apiBase}
            status={status}
          />
        )}
        {streamMode === "mjpeg" && (
          <MjpegPlayer
            streamUrl={mjpegUrl}
            active={streamMode === "mjpeg" && !!apiBase}
            status={status}
          />
        )}
        {streamMode === "off" && (
          <div className="viewer">
            <div className="viewer__placeholder">Preview paused — enable it in Setup</div>
          </div>
        )}
      </Card>

      <Card>
        <div className="rec-toggle">
          {/* While stale, the last-known mode isn't trustworthy — grey the dot
              rather than keep asserting a live "recording" state. */}
          <StatusDot state={isRecording && !isStale ? "live" : "idle"} />
          <div className="rec-toggle__info">
            <span className="label">
              {isStale
                ? isRecording
                  ? "recording (unconfirmed)"
                  : "standby (unconfirmed)"
                : isRecording
                ? "recording"
                : "standby"}
            </span>
            <div className="readout">
              {timer}
              {isStale && (
                <span
                  className="label"
                  style={{ marginLeft: "var(--sp-2)", whiteSpace: "normal" }}
                >
                  {lastSeenAt ? `last seen ${formatAgo(Date.now() - lastSeenAt)} ago` : "frozen"}
                </span>
              )}
            </div>
          </div>
          <Button
            variant={isRecording ? "primary" : "accent"}
            onClick={onToggleRecording}
            disabled={recordDisabled}
          >
            {isRecording ? "Stop" : "Record"}
          </Button>
        </div>
        {!isRecording && startBlockedReason && (
          <p className="dim" style={{ fontSize: "0.75rem", margin: "var(--sp-2) 0 0" }}>
            Can&apos;t record: {startBlockedReason}
          </p>
        )}
      </Card>

      {lastFile && (
        <Card title="Last recording">
          <div style={{ display: "flex", gap: "var(--sp-3)", alignItems: "center" }}>
            <Thumbnail apiBase={apiBase} name={lastFile.name} />
            <div className="file-meta">
              <span className="name">{lastFile.name}</span>
              <span className="label">
                {formatBytes(lastFile.size_bytes)} &middot; {formatDate(lastFile.modified_unix)}
              </span>
            </div>
          </div>
          <div className="row-wrap" style={{ marginTop: "var(--sp-3)" }}>
            {canShareNative(lastFile.size_bytes) && (
              <Button size="sm" variant="ghost" disabled={sharing} onClick={onShareLast}>
                {sharing ? "…" : "Share"}
              </Button>
            )}
            <Link to="/files" className="btn btn--sm btn--ghost">
              View all files
            </Link>
          </div>
        </Card>
      )}
    </div>
  );
}
