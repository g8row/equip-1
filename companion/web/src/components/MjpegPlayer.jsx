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
  const [frameUrl, setFrameUrl] = useState(""); // native: current blob: URL
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
        setLoaded(true);
        setImgError("");
        setFrameUrl((prev) => {
          if (prev) URL.revokeObjectURL(prev);
          return url;
        });
      },
      onError: (err) => setImgError(describeStreamFailure("mjpeg", err?.message)),
      onEnd: () => setImgError((c) => c || describeStreamFailure("mjpeg")),
    });
    return () => {
      stop();
      setFrameUrl((prev) => {
        if (prev) URL.revokeObjectURL(prev);
        return "";
      });
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
          frameUrl ? (
            <img className="viewer__media" src={frameUrl} alt="Camera preview" />
          ) : null
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
