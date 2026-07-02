import React, { useMemo, useState } from "react";
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

export default function Viewfinder() {
  const { apiBase, status, files, refresh, setError, error, streamMode, setStreamMode, reachable } =
    useServer();
  const [sharing, setSharing] = useState(false);

  const [loading, setLoading] = useState(false);

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
        setError(err.message || "Share failed");
      }
    } finally {
      setSharing(false);
    }
  }

  async function onToggleRecording() {
    setLoading(true);
    try {
      if (isRecording) {
        await stopRecording(apiBase);
        hapticImpact("Light");
      } else {
        await startRecording(apiBase);
        hapticImpact("Heavy");
      }
      await refresh();
    } catch (err) {
      setError(err.message || "Toggle failed");
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

      {error ? <div className="notice">{error}</div> : null}

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
          <StatusDot state={isRecording ? "live" : "idle"} />
          <div className="rec-toggle__info">
            <span className="label">{isRecording ? "recording" : "standby"}</span>
            <div className="readout">{timer}</div>
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
