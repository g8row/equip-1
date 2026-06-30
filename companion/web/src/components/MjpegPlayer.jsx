import React, { useEffect, useState } from "react";
import Button from "./ui/Button";
import { describeStreamFailure, streamIssue } from "../lib/stream";

/** MJPEG fallback player. Restartable; nonce busts the cached stream. */
export default function MjpegPlayer({ streamUrl, active, status }) {
  const [nonce, setNonce] = useState(Date.now());
  const [imgError, setImgError] = useState("");
  const [loaded, setLoaded] = useState(false);

  const preflightIssue = streamIssue(status, "mjpeg");

  useEffect(() => {
    if (!active) {
      setImgError("");
      setLoaded(false);
      return undefined;
    }
    setNonce(Date.now());
    setImgError(preflightIssue || "");
    setLoaded(false);
    if (preflightIssue) return undefined;

    const timeoutId = setTimeout(() => {
      setImgError((current) => current || describeStreamFailure("mjpeg"));
    }, 12000);
    return () => clearTimeout(timeoutId);
  }, [active, preflightIssue]);

  const src = active && !preflightIssue ? `${streamUrl}?t=${nonce}` : "";

  return (
    <div>
      <div className="viewer">
        {active && !preflightIssue ? (
          <img
            className="viewer__media"
            src={src}
            alt="MJPEG stream"
            onError={() => setImgError(describeStreamFailure("mjpeg"))}
            onLoad={() => {
              setLoaded(true);
              setImgError("");
            }}
          />
        ) : (
          <div className="viewer__placeholder">
            {preflightIssue || "Enable MJPEG to preview"}
          </div>
        )}
        {active && (
          <div className="viewer__overlay">
            <span className={`dot ${loaded ? "live" : "warn"}`} />
            {loaded ? "mjpeg live" : "mjpeg"}
          </div>
        )}
      </div>
      {imgError && <p className="stream-error">{imgError}</p>}
      <div className="viewer__toolbar">
        <span className="label">mjpeg · fallback</span>
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
