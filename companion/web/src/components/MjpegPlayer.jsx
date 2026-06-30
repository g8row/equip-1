import React, { useEffect, useRef, useState } from "react";
import Button from "./ui/Button";
import { describeStreamFailure, streamIssue } from "../lib/stream";

/** MJPEG fallback player. Restartable; nonce busts the cached stream. */
export default function MjpegPlayer({ streamUrl, active, status }) {
  const [nonce, setNonce] = useState(Date.now());
  const [imgError, setImgError] = useState("");
  const [loaded, setLoaded] = useState(false);
  const lastFrameAtRef = useRef(0);

  const preflightIssue = streamIssue(status, "mjpeg");
  const pipeline = status?.stream?.pipeline ?? "";

  useEffect(() => {
    if (!active) {
      setImgError("");
      setLoaded(false);
      return undefined;
    }
    setNonce(Date.now());
    setImgError(preflightIssue || "");
    setLoaded(false);
    lastFrameAtRef.current = 0;
    if (preflightIssue) return undefined;

    const timeoutId = setTimeout(() => {
      setImgError((current) => current || describeStreamFailure("mjpeg"));
    }, 12000);
    return () => clearTimeout(timeoutId);
  }, [active, preflightIssue]);

  // Watchdog: the browser doesn't reliably fire <img onerror> when a
  // multipart/x-mixed-replace stream's underlying connection is closed by
  // the server (e.g. camera unplugged mid-stream) — the <img> just freezes
  // on the last frame. Detect a dead capture pipeline from status polling
  // and force a reconnect once the camera (and hence the pipeline) is back.
  useEffect(() => {
    if (!active || preflightIssue || !loaded) return;
    const pipelineDead = pipeline.endsWith("-idle");
    if (!pipelineDead) return;
    const id = setTimeout(() => {
      setLoaded(false);
      setImgError("");
      setNonce(Date.now());
    }, 2000);
    return () => clearTimeout(id);
  }, [active, preflightIssue, loaded, pipeline]);

  const src = active && !preflightIssue ? `${streamUrl}?t=${nonce}` : "";

  return (
    <div>
      <div className="viewer viewer--4-3">
        {active && !preflightIssue ? (
          <img
            className="viewer__media"
            src={src}
            alt="Camera preview"
            onError={() => setImgError(describeStreamFailure("mjpeg"))}
            onLoad={() => {
              setLoaded(true);
              setImgError("");
              lastFrameAtRef.current = Date.now();
            }}
          />
        ) : (
          <div className="viewer__placeholder">
            {preflightIssue || "Enable preview in Setup"}
          </div>
        )}
        {active && !loaded && (
          <div className="viewer__cover">
            <span className={`dot ${imgError ? "warn" : "warn"}`} />
            <span>{imgError ? "Stream error" : "Connecting…"}</span>
          </div>
        )}
        {active && loaded && (
          <div className="viewer__overlay">
            <span className="dot live" />
            Live
          </div>
        )}
      </div>
      {imgError && <p className="stream-error">{imgError}</p>}
      <div className="viewer__toolbar">
        <span className="label">preview</span>
        <Button
          size="sm"
          onClick={() => {
            setImgError("");
            setLoaded(false);
            setNonce(Date.now());
          }}
          disabled={!active || !!preflightIssue}
        >
          Restart
        </Button>
      </div>
    </div>
  );
}
