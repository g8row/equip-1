import React, { useEffect, useRef, useState } from "react";
import Button from "./ui/Button";
import { describeStreamFailure, streamIssue } from "../lib/stream";
import { isNative } from "../lib/native";
import { startMjpegStream } from "../lib/mjpeg";

// The Android WebView silently blocks a direct cross-protocol
// <img src="http://..."> load (mixed content) even with
// MIXED_CONTENT_ALWAYS_ALLOW. So on native we don't point <img> at the stream
// URL — instead we fetch() the multipart mpjpeg stream, parse it into per-frame
// JPEG blob: URLs (see lib/mjpeg.js), and feed those to <img>, which is not
// blocked. On the web the plain streaming <img src> works and is used directly.
const NATIVE = isNative();

/** MJPEG fallback player. Restartable; nonce busts the cached stream. */
export default function MjpegPlayer({ streamUrl, active, status }) {
  const [nonce, setNonce] = useState(Date.now());
  const [imgError, setImgError] = useState("");
  const [loaded, setLoaded] = useState(false);
  // Native frames land here directly (imgRef.current.src = url) rather than
  // through React state — a DV feed can deliver 25-30 frames/sec, and routing
  // every one through setState would mean 25-30 re-renders/sec of this whole
  // component for no benefit (React never needs to reconcile anything else
  // about the tree when a frame arrives). frameUrlRef tracks the current
  // blob: URL purely so it can be revoked once the next one lands.
  const imgRef = useRef(null);
  const frameUrlRef = useRef("");
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
    return undefined;
  }, [active, preflightIssue]);

  // No-first-frame timeout: armed only until the first frame loads (adding
  // `loaded` to the deps clears it once we're live, instead of firing a false
  // error on a stream that's actually working).
  useEffect(() => {
    if (!active || preflightIssue || loaded) return undefined;
    const id = setTimeout(() => {
      setImgError((current) => current || describeStreamFailure("mjpeg"));
    }, 12000);
    return () => clearTimeout(id);
  }, [active, preflightIssue, loaded]);

  // Native: fetch + parse the multipart mpjpeg stream into blob-URL frames (the
  // WebView blocks a direct cross-protocol <img src="http://...">). Re-runs when
  // `nonce` changes (Restart button, or the pipeline-dead watchdog below).
  useEffect(() => {
    if (!NATIVE || !active || preflightIssue) return undefined;
    const stop = startMjpegStream(streamUrl, {
      onFrame: (url) => {
        lastFrameAtRef.current = Date.now();
        const prevUrl = frameUrlRef.current;
        frameUrlRef.current = url;
        if (imgRef.current) imgRef.current.src = url;
        // Revoke the previous blob in the same tick — nothing still needs it
        // once the new one is assigned as the src.
        if (prevUrl) URL.revokeObjectURL(prevUrl);
        // setState calls with a value equal to the current one bail out
        // without a re-render, so these are only "real" updates on the first
        // frame (loaded false->true) or after an actual error clears.
        setLoaded(true);
        setImgError("");
      },
      onError: (err) => setImgError(describeStreamFailure("mjpeg", err?.message)),
      onEnd: () => setImgError((c) => c || describeStreamFailure("mjpeg")),
    });
    return () => {
      stop();
      if (frameUrlRef.current) {
        URL.revokeObjectURL(frameUrlRef.current);
        frameUrlRef.current = "";
      }
      if (imgRef.current) imgRef.current.removeAttribute("src");
    };
  }, [active, preflightIssue, nonce, streamUrl]);

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

  const streaming = active && !preflightIssue;
  // Web: point <img> straight at the multipart stream (works off-native). Native:
  // src comes from the parsed blob-URL frames set by the effect above.
  const webSrc = streaming && !NATIVE ? `${streamUrl}?t=${nonce}` : "";

  return (
    <div>
      <div className="viewer viewer--4-3">
        {streaming && NATIVE ? (
          // src is never set here — the stream effect above writes
          // imgRef.current.src directly per frame. The viewer__cover overlay
          // (below) masks this element until `loaded` flips true.
          <img ref={imgRef} className="viewer__media" alt="Camera preview" />
        ) : streaming ? (
          <img
            className="viewer__media"
            src={webSrc}
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
            <span className="dot warn" />
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
