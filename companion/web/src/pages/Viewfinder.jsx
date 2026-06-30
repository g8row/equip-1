import React, { useMemo, useState } from "react";
import { useServer } from "../context/ServerContext";
import {
  getStreamUrl,
  getWhepUrl,
  startRecording,
  stopRecording,
  setRecordingCaptureMode,
} from "../api";
import { formatDuration } from "../lib/format";
import { streamIssue } from "../lib/stream";
import WhepPlayer from "../components/WhepPlayer";
import MjpegPlayer from "../components/MjpegPlayer";
import Card from "../components/ui/Card";
import Button from "../components/ui/Button";
import Tab, { Tabs } from "../components/ui/Tab";
import StatusDot from "../components/ui/StatusDot";

export default function Viewfinder() {
  const {
    apiBase,
    status,
    captureModeConfig,
    refresh,
    refreshCaptureMode,
    setError,
    error,
    streamMode,
    setStreamMode,
  } = useServer();

  const [loading, setLoading] = useState(false);
  const [captureModeLoading, setCaptureModeLoading] = useState(false);

  const isRecording = status?.recorder?.mode === "recording";
  const timer = useMemo(
    () => formatDuration(status?.recorder?.elapsed_seconds ?? 0),
    [status]
  );

  const whepUrl = useMemo(() => getWhepUrl(apiBase), [apiBase]);
  const mjpegUrl = useMemo(() => getStreamUrl(apiBase), [apiBase]);
  const mediamtxRunning = status?.stream?.mediamtx_running ?? false;
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

  async function onChangeRecordingMode(newMode) {
    setCaptureModeLoading(true);
    try {
      await setRecordingCaptureMode(apiBase, newMode);
      await refreshCaptureMode(apiBase);
    } catch (err) {
      setError(err.message || "Failed to change recording mode");
    } finally {
      setCaptureModeLoading(false);
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
            <StatusDot state={mediamtxRunning ? "ok" : "warn"} />
            <span className="label">
              mediamtx {mediamtxRunning ? "up" : "down"}
            </span>
          </div>
          <Tabs>
            <Tab
              active={streamMode === "webrtc"}
              badge="~100ms"
              onClick={() => setStreamMode("webrtc")}
            >
              WebRTC
            </Tab>
            <Tab
              active={streamMode === "mjpeg"}
              badge="~200ms"
              onClick={() => setStreamMode("mjpeg")}
            >
              MJPEG
            </Tab>
            <Tab active={streamMode === "off"} onClick={() => setStreamMode("off")}>
              Off
            </Tab>
          </Tabs>
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
            <div className="viewer__placeholder">Stream paused</div>
          </div>
        )}

        <p className="label" style={{ marginTop: "var(--sp-3)" }}>
          source: {status?.stream?.source ?? "—"}
        </p>
      </Card>

      <Card>
        <div className="rec-toggle">
          <StatusDot state={isRecording ? "live" : "idle"} />
          <div style={{ flex: 1 }}>
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

      <Card title="Capture mode">
        <div className="field-group">
          <select
            className="select"
            value={captureModeConfig?.current_mode ?? "dvgrab"}
            onChange={(e) => onChangeRecordingMode(e.target.value)}
            disabled={captureModeLoading || isRecording}
          >
            <option value="dvgrab">dvgrab + ffmpeg</option>
            <option value="ffmpeg-only">ffmpeg only (iec61883)</option>
          </select>
          <p className="dim" style={{ fontSize: "0.74rem", margin: 0 }}>
            {isRecording
              ? "Cannot change mode while recording"
              : "Compare capture pipelines"}
          </p>
        </div>
      </Card>
    </div>
  );
}
