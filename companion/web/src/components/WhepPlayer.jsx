import React, { useCallback, useEffect, useRef, useState } from "react";
import Button from "./ui/Button";
import { describeStreamFailure, responseDetail, streamIssue } from "../lib/stream";

// One-shot guard (per app session) for the ICE-interface reload below. Lives in
// sessionStorage so it survives the reload but resets when the app is reopened.
const ICE_RELOAD_FLAG = "equip1:iceReloadedForStaleInterface";

/**
 * Resolve once the PeerConnection has finished gathering ICE candidates (or a
 * timeout elapses). We POST the offer *after* this so the SDP carries our host
 * candidates. This WHEP flow is non-trickle — we never PATCH candidates — so
 * without waiting, mediamtx receives a candidate-less offer, has nothing to run
 * connectivity checks against, and the session dies with "deadline exceeded
 * while waiting connection" (signalling succeeds but the video stays black).
 * Host candidates on a LAN/AP gather in a few ms; the timeout is just a guard.
 */
function waitForIceGathering(pc, timeoutMs) {
  if (pc.iceGatheringState === "complete") return Promise.resolve();
  return new Promise((resolve) => {
    let settled = false;
    const finish = () => {
      if (settled) return;
      settled = true;
      pc.removeEventListener("icegatheringstatechange", onChange);
      clearTimeout(timer);
      resolve();
    };
    const onChange = () => {
      if (pc.iceGatheringState === "complete") finish();
    };
    const timer = setTimeout(finish, timeoutMs);
    pc.addEventListener("icegatheringstatechange", onChange);
  });
}

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
  const watchdogReconnectsRef = useRef(0);
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
      // Gather our ICE candidates into the offer before sending it (see
      // waitForIceGathering) — otherwise mediamtx has no candidates to connect
      // to and the media session times out, leaving the video black.
      await waitForIceGathering(pc, 3000);
      if (pc !== pcRef.current) return; // replaced/closed while we waited

      // Zero candidates gathered = Chromium's WebRTC network monitor is stale.
      // On a fresh launch the app joins the device AP *after* the monitor
      // enumerated interfaces, so it sees no network, gathers nothing, ICE never
      // connects, and the media stays black even though ontrack fires ("Live"
      // over a black frame). Rebuilding the PeerConnection reuses the same stale
      // monitor — only a full document reload makes Chromium re-enumerate
      // (verified on-device: 0 candidates → reload → host candidate 192.168.0.2).
      // Reload once per app session, guarded against a loop.
      const candidateCount = (
        pc.localDescription.sdp.match(/^a=candidate/gm) || []
      ).length;
      if (candidateCount === 0) {
        if (!sessionStorage.getItem(ICE_RELOAD_FLAG)) {
          sessionStorage.setItem(ICE_RELOAD_FLAG, String(Date.now()));
          window.location.reload();
          return;
        }
        throw new Error(
          "No network route to the device for live video. Reconnect to the device's WiFi, then reopen the app."
        );
      }
      // Candidates gathered fine — clear the one-shot reload guard.
      sessionStorage.removeItem(ICE_RELOAD_FLAG);

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
          body: pc.localDescription.sdp,
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
    watchdogReconnectsRef.current = 0;
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

  // Manual reconnect: clear the retry/watchdog budgets so the user always gets
  // a fresh set of attempts.
  const reconnect = useCallback(() => {
    retryCountRef.current = 0;
    watchdogReconnectsRef.current = 0;
    sessionStorage.removeItem(ICE_RELOAD_FLAG); // allow a fresh reload attempt
    connect();
  }, [connect]);

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

  // Frame-arrival watchdog. Reaching "live" only means the track was negotiated
  // (ontrack fired) — media can still silently fail to arrive, e.g. right after
  // the phone associates with the device hotspot on a fresh launch. Without this
  // the UI sits on "Live" over a black frame until the user reopens the app.
  // Poll inbound-video stats; if no frames decode within a grace window, rebuild
  // the connection — exactly what reopening does — capped so a genuinely dead
  // stream surfaces an error instead of looping forever.
  useEffect(() => {
    if (state !== "live") return undefined;
    const pc = pcRef.current;
    if (!pc) return undefined;
    const MAX_WATCHDOG_RECONNECTS = 4;
    let lastFrames = -1;
    let stalled = 0;
    const id = setInterval(async () => {
      if (pc !== pcRef.current) return;
      let frames = 0;
      try {
        (await pc.getStats()).forEach((s) => {
          if (s.type === "inbound-rtp" && s.kind === "video") frames = s.framesDecoded || 0;
        });
      } catch {
        return;
      }
      if (frames > 0 && frames !== lastFrames) {
        stalled = 0;
        watchdogReconnectsRef.current = 0; // healthy — frames advancing
      } else if ((stalled += 1) >= 5) {
        // ~5s "live" with no new frames → media isn't flowing.
        clearInterval(id);
        if (watchdogReconnectsRef.current < MAX_WATCHDOG_RECONNECTS) {
          watchdogReconnectsRef.current += 1;
          connect();
        } else {
          setState("error");
          setErrorMsg("Connected, but no video is arriving. Tap Reconnect.");
        }
        return;
      }
      lastFrames = frames;
    }, 1000);
    return () => clearInterval(id);
  }, [state, connect]);

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
          <Button size="sm" onClick={reconnect} disabled={state === "connecting"}>
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
