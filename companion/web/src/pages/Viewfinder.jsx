import React, { useMemo, useState } from "react";
import { useServer } from "../context/ServerContext";
import { getStreamUrl, getWhepUrl, startRecording, stopRecording } from "../api";
import { formatDuration, formatBytes } from "../lib/format";
import { streamIssue } from "../lib/stream";
import { hapticImpact, hapticNotification } from "../lib/haptics";
import WhepPlayer from "../components/WhepPlayer";
import MjpegPlayer from "../components/MjpegPlayer";
import Card from "../components/ui/Card";
import Button from "../components/ui/Button";
import StatusDot from "../components/ui/StatusDot";

export default function Viewfinder() {
  const { apiBase, status, refresh, setError, error, streamMode } = useServer();

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
        <div className="notice">
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
          <div className="stream-notice">{currentStreamIssue}</div>
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
            disabled={loading}
          >
            {isRecording ? "Stop" : "Record"}
          </Button>
        </div>
      </Card>
    </div>
  );
}
