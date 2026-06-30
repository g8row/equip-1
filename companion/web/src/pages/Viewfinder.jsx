import React, { useMemo, useState } from "react";
import { useServer } from "../context/ServerContext";
import { getStreamUrl, getWhepUrl, startRecording, stopRecording } from "../api";
import { formatDuration } from "../lib/format";
import { streamIssue } from "../lib/stream";
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

  async function onToggleRecording() {
    setLoading(true);
    try {
      if (isRecording) await stopRecording(apiBase);
      else await startRecording(apiBase);
      await refresh();
    } catch (err) {
      setError(err.message || "Toggle failed");
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
