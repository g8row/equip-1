import React, { useCallback, useEffect, useRef, useState } from "react";
import Button from "./ui/Button";
import { describeStreamFailure, responseDetail, streamIssue } from "../lib/stream";

/**
 * WebRTC WHEP player. Connects to the companion API's WHEP signalling
 * endpoint, attaches the remote stream, and aggressively seeks to the live
 * edge to keep latency low. Auto-retries on 503 (stream warming up).
 */
export default function WhepPlayer({ whepUrl, active, status }) {
  const videoRef = useRef(null);
  const pcRef = useRef(null);
  const retryTimerRef = useRef(null);
  const retryCountRef = useRef(0);
  const [state, setState] = useState("idle"); // idle | connecting | live | retrying | error
  const [errorMsg, setErrorMsg] = useState("");

  const preflightIssue = streamIssue(status, "webrtc");

  const connect = useCallback(async () => {
    if (!whepUrl || !active) return;
    if (preflightIssue) {
      setState("error");
      setErrorMsg(preflightIssue);
      return;
    }
    setState("connecting");
    setErrorMsg("");

    try {
      if (pcRef.current) {
        const old = pcRef.current;
        pcRef.current = null; // null first so its close event is ignored
        old.close();
      }

      const pc = new RTCPeerConnection({
        iceServers: [], // LAN only — no STUN needed
        bundlePolicy: "max-bundle",
      });
      pcRef.current = pc;

      pc.addTransceiver("video", { direction: "recvonly" });
      pc.addTransceiver("audio", { direction: "recvonly" });

      pc.ontrack = (event) => {
        if (videoRef.current && event.streams[0]) {
          videoRef.current.srcObject = event.streams[0];
          retryCountRef.current = 0;
          setState("live");
        }
      };

      pc.onconnectionstatechange = () => {
        // Ignore events from a PeerConnection we've already replaced or closed
        // on purpose (reconnect / retry / disconnect null out pcRef first), so a
        // deliberate close doesn't flash a spurious "Stream error".
        if (pc !== pcRef.current) return;
        if (pc.connectionState === "failed") {
          setState("error");
          setErrorMsg("WebRTC connection failed");
        }
      };

      const offer = await pc.createOffer();
      await pc.setLocalDescription(offer);

      // Without a timeout, a fetch to a fully-unreachable host (powered off,
      // dropped packets rather than an active refusal) can hang for the
      // platform's default TCP connect timeout — commonly 30+ seconds,
      // which reads as a frozen player rather than a clear error.
      const offerController = new AbortController();
      const offerTimeout = setTimeout(() => offerController.abort(), 8000);
      let res;
      try {
        res = await fetch(whepUrl, {
          method: "POST",
          headers: { "Content-Type": "application/sdp" },
          body: offer.sdp,
          signal: offerController.signal,
        });
      } finally {
        clearTimeout(offerTimeout);
      }

      // 503 = stream not ready yet (ffmpeg connecting to mediamtx); auto-retry
      if (res.status === 503) {
        const detail = await responseDetail(res);
        const cameraFailure = describeStreamFailure("webrtc", detail);
        if (cameraFailure !== detail) {
          throw new Error(cameraFailure);
        }
        const MAX_RETRIES = 15;
        const attempt = retryCountRef.current + 1;
        if (attempt <= MAX_RETRIES) {
          retryCountRef.current = attempt;
          setState("retrying");
          setErrorMsg(`Stream not ready — retrying (${attempt}/${MAX_RETRIES})…`);
          retryTimerRef.current = setTimeout(connect, 4000);
          return;
        }
      }

      if (!res.ok) {
        const detail = await responseDetail(res);
        throw new Error(describeStreamFailure("webrtc", detail));
      }

      const answerSdp = await res.text();
      await pc.setRemoteDescription({ type: "answer", sdp: answerSdp });
    } catch (err) {
      setState("error");
      const message = err.name === "AbortError" ? "Failed to fetch" : err.message;
      setErrorMsg(describeStreamFailure("webrtc", message));
    }
  }, [whepUrl, active, preflightIssue]);

  const disconnect = useCallback(() => {
    if (retryTimerRef.current) {
      clearTimeout(retryTimerRef.current);
      retryTimerRef.current = null;
    }
    retryCountRef.current = 0;
    if (pcRef.current) {
      const old = pcRef.current;
      pcRef.current = null;
      old.close();
    }
    if (videoRef.current) {
      videoRef.current.srcObject = null;
    }
    setState("idle");
    setErrorMsg("");
  }, []);

  // Auto-connect when active; disconnect when not
  useEffect(() => {
    if (active) {
      connect();
    } else {
      disconnect();
    }
    return () => {
      if (retryTimerRef.current) {
        clearTimeout(retryTimerRef.current);
        retryTimerRef.current = null;
      }
      if (pcRef.current) {
        const old = pcRef.current;
        pcRef.current = null;
        old.close();
      }
    };
  }, [active, whepUrl, preflightIssue]); // eslint-disable-line react-hooks/exhaustive-deps

  // Live-edge seek: attached once so it isn't re-added on every reconnect/retry
  // (which would stack listeners fighting over currentTime). Reads the current
  // video element each time it fires.
  useEffect(() => {
    const v = videoRef.current;
    if (!v) return undefined;
    const onProgress = () => {
      if (!v.buffered.length) return;
      const end = v.buffered.end(v.buffered.length - 1);
      if (end - v.currentTime > 0.4) v.currentTime = end - 0.1;
    };
    v.addEventListener("progress", onProgress, { passive: true });
    return () => v.removeEventListener("progress", onProgress);
  }, []);

  const dotClass =
    state === "live" ? "live" : state === "error" ? "warn" : state === "idle" ? "idle" : "warn";

  const statusText = {
    idle: "Paused",
    connecting: "Connecting…",
    retrying: "Reconnecting…",
    live: "Live",
    error: "Stream error",
  }[state];

  return (
    <div>
      <div className="viewer viewer--4-3">
        <video
          ref={videoRef}
          autoPlay
          muted
          playsInline
          className="viewer__media"
          aria-label="Live camera preview"
        />
        {state !== "live" && (
          <div className="viewer__cover">
            <span className={`dot ${dotClass}`} />
            <span>{statusText}</span>
          </div>
        )}
        {state === "live" && (
          <div className="viewer__overlay">
            <span className={`dot ${dotClass}`} />
            Live
          </div>
        )}
      </div>
      {errorMsg && <p className="stream-error">{errorMsg}</p>}
      <div className="viewer__toolbar">
        <span className="label">preview</span>
        <div className="row-wrap">
          <Button size="sm" onClick={connect} disabled={state === "connecting"}>
            Reconnect
          </Button>
          <Button size="sm" variant="ghost" onClick={disconnect} disabled={state === "idle"}>
            Disconnect
          </Button>
        </div>
      </div>
    </div>
  );
}
